package contract

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runCustomRuleScenarios holds what the custom-rule schema (ADR 0015) needs from the store: a stored row keeps its
// rule id, and the config's keys survive a round trip, including the ones this build does not know.
func runCustomRuleScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	org := "contract-customrules-" + runID
	other := "contract-customrules-other-" + runID
	// Both organisations, saved for each scenario: their rules need them.
	seedOrgs := func(t *testing.T) {
		t.Helper()
		seedOrg(ctx, t, b, org, "contract-cr-admin-"+runID)
		seedOrg(ctx, t, b, other, "contract-cr-admin-"+runID)
	}
	cfg := func(id, match string) json.RawMessage {
		return json.RawMessage(`{"kind":"block","schemaVersion":1,"id":"` + id + `","name":"` + id + `","severity":"warning","rawTypeMatch":"` + match + `"}`)
	}
	list := func(t *testing.T, org string) map[string]*interfaces.OrgCustomRule {
		t.Helper()
		rows, err := b.ListOrgCustomRules(ctx, org, false)
		if err != nil {
			t.Fatalf("ListOrgCustomRules(%s): %v", org, err)
		}
		out := make(map[string]*interfaces.OrgCustomRule, len(rows))
		for _, r := range rows {
			out[r.ID] = r
		}
		return out
	}
	match := func(t *testing.T, raw json.RawMessage) string {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("stored config is not JSON: %v (%s)", err, raw)
		}
		s, _ := m["rawTypeMatch"].(string)
		return s
	}

	// A save that names a stored row under another rule id is refused with ErrOrgCustomRuleRenamed and changes
	// nothing: not the row, and not the org's other rule when the new id is already taken. Postgres used to answer a
	// primary-key violation (a 500) and the fake renamed in place.
	run("CustomRules_rename_is_refused", func(t *testing.T) {
		seedOrgs(t)
		a := &interfaces.OrgCustomRule{ID: "contract-cr-a-" + runID, OrgID: org, RuleID: "rule-a", Config: cfg("rule-a", "^SET$"), Enabled: true}
		if err := b.SaveOrgCustomRule(ctx, a); err != nil {
			t.Skipf("backend does not support org custom rules: %v", err)
		}
		bRow := &interfaces.OrgCustomRule{ID: "contract-cr-b-" + runID, OrgID: org, RuleID: "rule-b", Config: cfg("rule-b", "^WAIT$"), Enabled: true}
		if err := b.SaveOrgCustomRule(ctx, bRow); err != nil {
			t.Fatalf("SaveOrgCustomRule(rule-b): %v", err)
		}
		t.Cleanup(func() {
			_ = b.DeleteOrgCustomRule(ctx, org, a.ID)
			_ = b.DeleteOrgCustomRule(ctx, org, bRow.ID)
		})

		for _, newID := range []string{"rule-renamed", "rule-b"} {
			rename := &interfaces.OrgCustomRule{ID: a.ID, OrgID: org, RuleID: newID, Config: cfg(newID, "^IF$"), Enabled: true}
			if err := b.SaveOrgCustomRule(ctx, rename); !errors.Is(err, interfaces.ErrOrgCustomRuleRenamed) {
				t.Errorf("renaming rule-a to %s: err = %v, want ErrOrgCustomRuleRenamed", newID, err)
			}
		}
		rows := list(t, org)
		if len(rows) != 2 {
			t.Fatalf("the org has %d rules after the refused renames, want 2", len(rows))
		}
		if r := rows[a.ID]; r == nil || r.RuleID != "rule-a" || match(t, r.Config) != "^SET$" {
			t.Errorf("rule-a changed: %+v", r)
		}
		if r := rows[bRow.ID]; r == nil || r.RuleID != "rule-b" || match(t, r.Config) != "^WAIT$" {
			t.Errorf("rule-b was overwritten through rule-a's row: %+v", r)
		}

		// The same row under its own rule id is an ordinary re-save.
		resave := &interfaces.OrgCustomRule{ID: a.ID, OrgID: org, RuleID: "rule-a", Config: cfg("rule-a", "^IF$"), Enabled: false}
		if err := b.SaveOrgCustomRule(ctx, resave); err != nil {
			t.Fatalf("re-save under the same rule id: %v", err)
		}
		if resave.ID != a.ID {
			t.Errorf("re-save reported row %q, want %q", resave.ID, a.ID)
		}
		if r := list(t, org)[a.ID]; r == nil || r.Enabled || match(t, r.Config) != "^IF$" {
			t.Errorf("re-save did not replace the row: %+v", r)
		}
	})

	// Another org's row id is never written through this org, renamed or not.
	run("CustomRules_other_orgs_row_is_not_found", func(t *testing.T) {
		seedOrgs(t)
		victim := &interfaces.OrgCustomRule{ID: "contract-cr-victim-" + runID, OrgID: other, RuleID: "victim", Config: cfg("victim", "^SET$"), Enabled: true}
		if err := b.SaveOrgCustomRule(ctx, victim); err != nil {
			t.Skipf("backend does not support org custom rules: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrgCustomRule(ctx, other, victim.ID) })

		for _, ruleID := range []string{"victim", "intruder"} {
			err := b.SaveOrgCustomRule(ctx, &interfaces.OrgCustomRule{ID: victim.ID, OrgID: org, RuleID: ruleID, Config: cfg(ruleID, "^IF$"), Enabled: true})
			if !errors.Is(err, interfaces.ErrNotFound) {
				t.Errorf("saving rule %s through another org's row id: err = %v, want ErrNotFound", ruleID, err)
			}
		}
		if rows := list(t, org); len(rows) != 0 {
			t.Errorf("the refused saves stored %d rule(s) in the writing org", len(rows))
		}
		if r := list(t, other)[victim.ID]; r == nil || r.RuleID != "victim" || match(t, r.Config) != "^SET$" {
			t.Errorf("the other org's rule changed: %+v", r)
		}
	})

	// A pause or resume changes the enabled flag alone: the config stays as stored, including one a full save wrote
	// after the caller read the row (a pause that wrote back the row it read restored the older config). Another
	// org's row id, or an unknown one, is ErrNotFound and changes nothing.
	run("CustomRules_set_enabled_keeps_the_config", func(t *testing.T) {
		seedOrgs(t)
		row := &interfaces.OrgCustomRule{ID: "contract-cr-toggle-" + runID, OrgID: org, RuleID: "toggle", Config: cfg("toggle", "^SET$"), Enabled: true}
		if err := b.SaveOrgCustomRule(ctx, row); err != nil {
			t.Skipf("backend does not support org custom rules: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrgCustomRule(ctx, org, row.ID) })
		read := list(t, org)[row.ID]
		edit := &interfaces.OrgCustomRule{ID: row.ID, OrgID: org, RuleID: "toggle", Config: cfg("toggle", "^WAIT$"), Enabled: true}
		if err := b.SaveOrgCustomRule(ctx, edit); err != nil {
			t.Fatalf("the full save: %v", err)
		}

		got, err := b.SetOrgCustomRuleEnabled(ctx, org, read.ID, false)
		if err != nil {
			t.Fatalf("SetOrgCustomRuleEnabled: %v", err)
		}
		if got.ID != row.ID || got.OrgID != org || got.RuleID != "toggle" || got.Enabled || match(t, got.Config) != "^WAIT$" {
			t.Errorf("SetOrgCustomRuleEnabled returned %+v (%s), want the saved rule paused", got, got.Config)
		}
		if r := list(t, org)[row.ID]; r == nil || r.Enabled || match(t, r.Config) != "^WAIT$" {
			t.Errorf("stored %+v, want the saved config paused", r)
		}
		if got, err := b.SetOrgCustomRuleEnabled(ctx, org, row.ID, true); err != nil || !got.Enabled {
			t.Errorf("resume: %+v, %v", got, err)
		}

		for _, target := range []struct{ org, id string }{{other, row.ID}, {org, "contract-cr-missing-" + runID}} {
			if _, err := b.SetOrgCustomRuleEnabled(ctx, target.org, target.id, false); !errors.Is(err, interfaces.ErrNotFound) {
				t.Errorf("SetOrgCustomRuleEnabled(%s, %s): err = %v, want ErrNotFound", target.org, target.id, err)
			}
		}
		if r := list(t, org)[row.ID]; r == nil || !r.Enabled {
			t.Errorf("a refused toggle changed the rule: %+v", r)
		}
		if rows := list(t, other); len(rows) != 0 {
			t.Errorf("a refused toggle stored %d rule(s) in the other org", len(rows))
		}
	})

	// The store carries the config opaquely: keys it does not know (a newer release's fields) come back with their
	// values, so a save that merges them back keeps them.
	run("CustomRules_unknown_config_keys_round_trip", func(t *testing.T) {
		seedOrgs(t)
		raw := json.RawMessage(`{"kind":"block","schemaVersion":1,"id":"fwd","name":"fwd","severity":"info","rawTypeMatch":"^SET$","futureField":{"n":[1,2]},"note":"kept"}`)
		row := &interfaces.OrgCustomRule{ID: "contract-cr-fwd-" + runID, OrgID: org, RuleID: "fwd", Config: raw, Enabled: true}
		if err := b.SaveOrgCustomRule(ctx, row); err != nil {
			t.Skipf("backend does not support org custom rules: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrgCustomRule(ctx, org, row.ID) })

		got := list(t, org)[row.ID]
		if got == nil {
			t.Fatal("the saved rule is not listed")
		}
		var want, have map[string]any
		_ = json.Unmarshal(raw, &want)
		if err := json.Unmarshal(got.Config, &have); err != nil {
			t.Fatalf("stored config is not JSON: %v", err)
		}
		wantJSON, _ := json.Marshal(want)
		haveJSON, _ := json.Marshal(have)
		if string(wantJSON) != string(haveJSON) {
			t.Errorf("config did not round-trip:\n got %s\nwant %s", haveJSON, wantJSON)
		}
	})
}
