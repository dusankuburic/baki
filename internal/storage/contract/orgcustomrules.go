package contract

import (
	"context"
	"encoding/json"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runOrgCustomRuleScenarios holds the organisations' custom rules.
func runOrgCustomRuleScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	// Per-org custom rules must be isolated by org at the SQL layer, not only by RLS: this suite runs
	// without an RLS transaction on purpose so the explicit predicates are what is under test (B8's
	// lesson). The (org_id, rule_id) pair is the uniqueness contract: two orgs may both define
	// "house-style" and they are DIFFERENT rules.
	run("OrgCustomRules_isolated_per_org", func(t *testing.T) {
		orgA := "contract-rules-orgA-" + runID
		orgB := "contract-rules-orgB-" + runID
		const sharedRuleID = "house-style"

		// Postgres stores config as JSONB and hands it back re-serialized — assert on parsed fields,
		// never on raw substrings.
		field := func(t *testing.T, raw json.RawMessage, key string) string {
			t.Helper()
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("stored config is not valid JSON: %v (%s)", err, raw)
			}
			v, _ := m[key].(string)
			return v
		}

		mk := func(org, id, match string) *interfaces.OrgCustomRule {
			return &interfaces.OrgCustomRule{
				ID:      id,
				OrgID:   org,
				RuleID:  sharedRuleID,
				Config:  json.RawMessage(`{"id":"` + sharedRuleID + `","name":"House style","severity":"warning","rawTypeMatch":"` + match + `"}`),
				Enabled: true,
			}
		}

		ruleA := mk(orgA, "contract-rule-a-"+runID, "^SET$")
		if err := b.SaveOrgCustomRule(ctx, ruleA); err != nil {
			t.Skipf("backend does not support org custom rules: %v", err)
		}
		ruleB := mk(orgB, "contract-rule-b-"+runID, "^WAIT$")
		if err := b.SaveOrgCustomRule(ctx, ruleB); err != nil {
			t.Fatalf("SaveOrgCustomRule(orgB): %v", err)
		}
		t.Cleanup(func() {
			_ = b.DeleteOrgCustomRule(ctx, orgA, ruleA.ID)
			_ = b.DeleteOrgCustomRule(ctx, orgB, ruleB.ID)
		})

		// The same author-id under two orgs coexists as two rows.
		listA, err := b.ListOrgCustomRules(ctx, orgA, false)
		if err != nil {
			t.Fatalf("ListOrgCustomRules(orgA): %v", err)
		}
		if len(listA) != 1 {
			t.Fatalf("orgA sees %d rule(s), want exactly its own 1", len(listA))
		}
		if got := field(t, listA[0].Config, "rawTypeMatch"); got != "^SET$" {
			t.Errorf("orgA got the wrong config (rawTypeMatch=%q, want ^SET$) — possibly orgB's", got)
		}

		listB, err := b.ListOrgCustomRules(ctx, orgB, false)
		if err != nil {
			t.Fatalf("ListOrgCustomRules(orgB): %v", err)
		}
		if len(listB) != 1 || field(t, listB[0].Config, "rawTypeMatch") != "^WAIT$" {
			t.Errorf("orgB does not see exactly its own rule: %+v", listB)
		}

		// A delete scoped to the wrong org must not remove another org's rule.
		if err := b.DeleteOrgCustomRule(ctx, orgB, ruleA.ID); err != nil {
			t.Fatalf("cross-org DeleteOrgCustomRule: %v", err)
		}
		if again, err := b.ListOrgCustomRules(ctx, orgA, false); err != nil {
			t.Fatalf("ListOrgCustomRules(orgA) after cross-org delete: %v", err)
		} else if len(again) != 1 {
			t.Errorf("orgA's rule was deleted through orgB's scope")
		}

		// Re-saving the same (org, ruleID) replaces rather than duplicating.
		ruleA.Config = json.RawMessage(`{"id":"` + sharedRuleID + `","name":"House style","severity":"error","rawTypeMatch":"^SET$"}`)
		if err := b.SaveOrgCustomRule(ctx, ruleA); err != nil {
			t.Fatalf("re-save: %v", err)
		}
		if after, err := b.ListOrgCustomRules(ctx, orgA, false); err != nil {
			t.Fatalf("list after re-save: %v", err)
		} else if len(after) != 1 {
			t.Errorf("re-saving the same (org, ruleId) produced %d rows, want 1", len(after))
		} else if got := field(t, after[0].Config, "severity"); got != "error" {
			t.Errorf("re-save did not replace the config (severity=%q, want error): %s", got, after[0].Config)
		}

		// The B8.2 shape: an admin of one org submits ANOTHER org's surrogate id — whether the backend
		// errors or no-ops does not matter; the other org's row must never change (conflicting on the
		// surrogate id instead of (org_id, rule_id) is precisely how org_channels let one tenant retarget another's row).
		hijack := &interfaces.OrgCustomRule{
			ID:      ruleA.ID, // orgA's surrogate id
			OrgID:   orgB,     // ...submitted by orgB
			RuleID:  "hijacked",
			Config:  json.RawMessage(`{"id":"hijacked","name":"Hijack","severity":"info","rawTypeMatch":"^IF$"}`),
			Enabled: true,
		}
		_ = b.SaveOrgCustomRule(ctx, hijack) // error or no-op are both fine
		if victim, err := b.ListOrgCustomRules(ctx, orgA, false); err != nil {
			t.Fatalf("list orgA after hijack attempt: %v", err)
		} else if len(victim) != 1 {
			t.Errorf("orgA has %d rule(s) after a cross-org save attempt, want 1", len(victim))
		} else if got := field(t, victim[0].Config, "rawTypeMatch"); got != "^SET$" {
			t.Errorf("orgB overwrote orgA's rule through its surrogate id (rawTypeMatch=%q, want ^SET$)", got)
		}
		if attacker, err := b.ListOrgCustomRules(ctx, orgB, false); err != nil {
			t.Fatalf("list orgB after hijack attempt: %v", err)
		} else if len(attacker) != 1 {
			t.Errorf("orgB has %d rule(s) after a rejected save, want its original 1", len(attacker))
		}

		// enabledOnly must exclude a disabled rule — the analysis path relies on
		// it to avoid compiling rules the org has paused.
		ruleA.Enabled = false
		if err := b.SaveOrgCustomRule(ctx, ruleA); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if enabled, err := b.ListOrgCustomRules(ctx, orgA, true); err != nil {
			t.Fatalf("list enabledOnly: %v", err)
		} else if len(enabled) != 0 {
			t.Errorf("enabledOnly returned %d disabled rule(s)", len(enabled))
		}
	})
}
