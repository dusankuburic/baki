package service

import (
	"context"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"

	"pad-analyzer/internal/testutil"
)

// TestGetRulesFor_OrgScoped pins that the rule catalog served to an org member
// is the org-resolved profile (its custom rules and overrides), not the
// deployment catalog. Before this the catalog ignored orgs, so RuleReference
// never listed an org's custom rules and showed deployment severities while
// analysis ran on the org's.
func TestGetRulesFor_OrgScoped(t *testing.T) {
	b := testutil.NewFakeBackend()
	storeRule(t, b, "org-a", "a-only", "^SET$", true)

	deployment := deploymentSettings(map[string]models.RuleConfig{
		"deep-nesting": {Enabled: true, Severity: "info"},
	})
	svc, err := NewAnalysisService(nil, deployment, analyzer.NewHistoryStore(""))
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	svc.SetRuleProfileResolver(NewRuleProfileResolver(b, deployment, nil))
	ctx := context.Background()

	has := func(rules []models.Rule, id string) bool {
		for _, r := range rules {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	if rules := svc.GetRulesFor(ctx, "org-a"); !has(rules, "a-only") {
		t.Errorf("org-scoped catalog must include the org's custom rule")
	}
	if rules := svc.GetRulesFor(ctx, ""); has(rules, "a-only") {
		t.Errorf("deployment catalog must not include an org's custom rule")
	}
	if rules := svc.GetRulesFor(ctx, "org-b"); has(rules, "a-only") {
		t.Errorf("another org's catalog must not include org-a's custom rule")
	}
	if s := svc.GetRulesSummaryFor(ctx, "org-a"); s.TotalRules != len(svc.GetRulesFor(ctx, "org-a")) {
		t.Errorf("summary must count the org-scoped catalog: %d vs %d", s.TotalRules, len(svc.GetRulesFor(ctx, "org-a")))
	}
}
