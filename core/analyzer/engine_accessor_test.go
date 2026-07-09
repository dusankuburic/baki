package analyzer

import (
	"testing"

	"pad-core/models"
)

// TestRuleConfidence_Defaults verifies the catalog-level confidence accessor:
// explicitly-listed rules return their tier, everything else defaults to
// Medium. This is the data the dashboard's "confidence distribution" KPI is
// built from, so the contract must be stable.
func TestRuleConfidence_Defaults(t *testing.T) {
	cases := []struct {
		id   string
		want models.Confidence
	}{
		{"hardcoded-credential", models.ConfidenceHigh}, // explicit High
		{"infinite-loop-risk", models.ConfidenceHigh},   // explicit High
		{"hardcoded-url", models.ConfidenceLow},         // explicit Low
		{"unused-variable", models.ConfidenceLow},       // explicit Low
		{"missing-timeout", models.ConfidenceMedium},    // not listed → default
		{"no-such-rule", models.ConfidenceMedium},       // unknown → default
	}
	for _, c := range cases {
		if got := RuleConfidence(c.id); got != c.want {
			t.Errorf("RuleConfidence(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestRuleAutoFix_KnownFixers verifies the fixer map is exposed correctly:
// every rule with a deterministic fixer returns its fixType, others return "".
// The fixTypes must stay in sync with FlowService.ApplyFix dispatch.
func TestRuleAutoFix_KnownFixers(t *testing.T) {
	cases := []struct {
		id, want string
	}{
		{"missing-timeout", "set-timeout"},
		{"infinite-loop-risk", "insert-exit-condition"},
		{"resource-leak", "insert-close"},
		{"hardcoded-credential", "replace-with-variable"},
		{"unhandled-error", "wrap-error-handler"},
		{"no-such-rule", ""},
		{"deep-nesting", ""}, // style rule, no auto-fixer
	}
	for _, c := range cases {
		if got := RuleAutoFix(c.id); got != c.want {
			t.Errorf("RuleAutoFix(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestRuleCatalog_AllRulesHaveCatalogShape is a round-trip guard: every rule
// returned by AllRules() must resolve to a non-empty id, severity, and category
// through the catalog accessors so the dashboard summary never sees blanks.
func TestRuleCatalog_AllRulesHaveCatalogShape(t *testing.T) {
	rules := AllRules()
	if len(rules) == 0 {
		t.Fatal("AllRules returned no rules")
	}
	seen := map[string]bool{}
	for _, r := range rules {
		id := r.ID()
		if id == "" {
			t.Error("rule with empty ID")
		}
		if seen[id] {
			t.Errorf("duplicate rule ID %q", id)
		}
		seen[id] = true
		if r.DefaultSeverity() == "" {
			t.Errorf("rule %q has empty severity", id)
		}
		if r.Category() == "" {
			t.Errorf("rule %q has empty category", id)
		}
	}
}
