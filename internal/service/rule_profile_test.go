package service

import (
	"context"
	"encoding/json"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/analyzer"
	"pad-core/models"
)

// stubSettings is a minimal SettingsProvider over one in-memory value.
type stubSettings struct{ s *models.AppSettings }

func (p *stubSettings) Get() *models.AppSettings          { return p.s }
func (p *stubSettings) Update(v models.AppSettings) error { p.s = &v; return nil }
func (p *stubSettings) AddRecentFile(string, int64) error { return nil }
func (p *stubSettings) RemoveRecentFile(string) error     { return nil }
func (p *stubSettings) ClearRecentFiles() error           { return nil }

func deploymentSettings(rules map[string]models.RuleConfig) *stubSettings {
	s := models.DefaultSettings()
	s.Analysis.Rules = rules
	return &stubSettings{s: s}
}

func storeRule(t *testing.T, b *testutil.FakeBackend, orgID, ruleID, match string, enabled bool) {
	t.Helper()
	cfg := analyzer.CustomRuleConfig{
		ID: ruleID, Name: ruleID, Severity: "warning",
		Category: "Style", RawTypeMatch: match,
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal rule: %v", err)
	}
	if err := b.SaveOrgCustomRule(context.Background(), &storageif.OrgCustomRule{
		ID: orgID + "-" + ruleID, OrgID: orgID, RuleID: ruleID,
		Config: raw, Enabled: enabled,
	}); err != nil {
		t.Fatalf("SaveOrgCustomRule: %v", err)
	}
}

func hasRule(rules []analyzer.Rule, id string) bool {
	for _, r := range rules {
		if r.ID() == id {
			return true
		}
	}
	return false
}

// TestRuleProfileResolver_OrgIsolation is the core guarantee: one org's custom
// rules never appear in another org's rule set, and an org with no rules of its
// own gets only the deployment layer.
func TestRuleProfileResolver_OrgIsolation(t *testing.T) {
	b := testutil.NewFakeBackend()
	storeRule(t, b, "org-a", "a-only", "^SET$", true)
	storeRule(t, b, "org-b", "b-only", "^WAIT$", true)

	r := NewRuleProfileResolver(b, deploymentSettings(nil), nil)
	ctx := context.Background()

	a := r.Resolve(ctx, "org-a")
	if !hasRule(a.Rules, "a-only") {
		t.Error("org A did not receive its own rule")
	}
	if hasRule(a.Rules, "b-only") {
		t.Error("org A received org B's rule")
	}

	bp := r.Resolve(ctx, "org-b")
	if !hasRule(bp.Rules, "b-only") {
		t.Error("org B did not receive its own rule")
	}
	if hasRule(bp.Rules, "a-only") {
		t.Error("org B received org A's rule")
	}

	// No org (desktop/local, or a flow with no org) gets neither.
	none := r.Resolve(ctx, "")
	if hasRule(none.Rules, "a-only") || hasRule(none.Rules, "b-only") {
		t.Error("the no-org profile received an org's custom rules")
	}
	if len(none.Rules) != len(analyzer.AllRules()) {
		t.Errorf("no-org profile has %d rules, want the %d built-ins", len(none.Rules), len(analyzer.AllRules()))
	}
}

// TestRuleProfileResolver_DisabledRulesAreNotCompiled — a paused rule must not
// run. The resolver asks storage for enabled-only; this pins that it does.
func TestRuleProfileResolver_DisabledRulesAreNotCompiled(t *testing.T) {
	b := testutil.NewFakeBackend()
	storeRule(t, b, "org-a", "paused", "^SET$", false)

	r := NewRuleProfileResolver(b, deploymentSettings(nil), nil)
	if hasRule(r.Resolve(context.Background(), "org-a").Rules, "paused") {
		t.Error("a disabled org rule was compiled into the rule set")
	}
}

// TestRuleProfileResolver_SettingsMergePerRule pins the merge semantics: an org
// overriding two rules must INHERIT the deployment's configuration for every
// other rule. A wholesale replace would silently drop them.
func TestRuleProfileResolver_SettingsMergePerRule(t *testing.T) {
	b := testutil.NewFakeBackend()
	deployment := deploymentSettings(map[string]models.RuleConfig{
		"rule-x": {Enabled: false, Severity: "info"},
		"rule-y": {Enabled: true, Severity: "warning"},
	})
	// The org re-grades only rule-y.
	if err := b.SaveOrgSettings(context.Background(), "org-a", &storageif.AppSettings{
		Analysis: storageif.AnalysisSettings{
			Rules: map[string]storageif.RuleConfig{
				"rule-y": {Enabled: true, Severity: "error"},
			},
		},
	}); err != nil {
		t.Fatalf("SaveOrgSettings: %v", err)
	}

	r := NewRuleProfileResolver(b, deployment, nil)
	got := r.Resolve(context.Background(), "org-a").Settings

	if rc, ok := got.Analysis.Rules["rule-y"]; !ok || rc.Severity != "error" {
		t.Errorf("org override did not win for rule-y: %+v", rc)
	}
	if rc, ok := got.Analysis.Rules["rule-x"]; !ok || rc.Severity != "info" || rc.Enabled {
		t.Errorf("org lost the deployment's config for the rule it never touched: %+v (ok=%v)", rc, ok)
	}

	// The deployment's own settings must not have been mutated by the merge.
	if deployment.s.Analysis.Rules["rule-y"].Severity != "warning" {
		t.Error("resolving an org profile mutated the shared deployment settings")
	}
}

// TestRuleProfileResolver_InvalidateForcesReresolve — a rule added after a
// resolve must be picked up once the org is invalidated, not held until the TTL.
func TestRuleProfileResolver_InvalidateForcesReresolve(t *testing.T) {
	b := testutil.NewFakeBackend()
	r := NewRuleProfileResolver(b, deploymentSettings(nil), nil)
	ctx := context.Background()

	if hasRule(r.Resolve(ctx, "org-a").Rules, "added-later") {
		t.Fatal("precondition: rule should not exist yet")
	}
	storeRule(t, b, "org-a", "added-later", "^SET$", true)

	if hasRule(r.Resolve(ctx, "org-a").Rules, "added-later") {
		t.Error("the cached profile should still be serving until invalidated — otherwise the cache is not doing its job")
	}
	r.Invalidate("org-a")
	if !hasRule(r.Resolve(ctx, "org-a").Rules, "added-later") {
		t.Error("Invalidate did not force a re-resolve")
	}
}
