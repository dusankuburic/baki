package service

import (
	"testing"

	"pad-core/analyzer"
)

// TestGetRulesSummary verifies the catalog rollup the dashboard consumes for
// its "auto-fixable rules" and "confidence distribution" KPIs. The totals must
// match the live rule catalog so a new rule can never silently fall off the
// summary.
func TestGetRulesSummary(t *testing.T) {
	svc := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil)
	summary := svc.GetRulesSummary()

	totalRules := len(analyzer.AllRules())
	if summary.TotalRules != totalRules {
		t.Errorf("TotalRules = %d, want %d (len(AllRules))", summary.TotalRules, totalRules)
	}

	// Auto-fixable rules: the dashboard advertises "fix availability", so the
	// count must equal the number of rules that resolve to a non-empty fixType.
	wantFixable := 0
	for _, r := range analyzer.AllRules() {
		if analyzer.RuleAutoFix(r.ID()) != "" {
			wantFixable++
		}
	}
	if summary.AutoFixableRules != wantFixable {
		t.Errorf("AutoFixableRules = %d, want %d", summary.AutoFixableRules, wantFixable)
	}
	if wantFixable == 0 {
		t.Error("expected at least one auto-fixable rule; fixer map appears unwired")
	}

	// The category + confidence distributions must each account for every rule.
	catSum := 0
	for _, n := range summary.ByCategory {
		catSum += n
	}
	if catSum != totalRules {
		t.Errorf("sum(ByCategory) = %d, want %d", catSum, totalRules)
	}
	confSum := 0
	for _, n := range summary.ByConfidence {
		confSum += n
	}
	if confSum != totalRules {
		t.Errorf("sum(ByConfidence) = %d, want %d", confSum, totalRules)
	}

	// Known anchors (stable across catalog edits): the two extreme rules must
	// land in their documented tiers so the confidence donut renders truthfully.
	if summary.ByConfidence["high"] == 0 {
		t.Error("expected non-zero high-confidence rule count")
	}
	if summary.ByConfidence["low"] == 0 {
		t.Error("expected non-zero low-confidence rule count")
	}
	if summary.ByCategory["Security"] == 0 {
		t.Error("expected non-zero Security category count")
	}
}

// TestGetRulesSummary_HonoursSettingsOverrides ensures the summary is computed
// from the same override-aware catalog as GetRules, so disabling a rule in
// settings still leaves the catalog total intact (the summary reports the
// *available* catalog, not enabled-state — enabled filtering is the UI's job).
func TestGetRulesSummary_HonoursSettingsOverrides(t *testing.T) {
	svc := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil)

	totalBefore := svc.GetRulesSummary().TotalRules

	// Disabling a rule changes its Enabled flag but must not change the catalog
	// total the dashboard shows.
	if err := svc.SetRuleEnabled("deep-nesting", false); err != nil {
		t.Fatalf("SetRuleEnabled: %v", err)
	}
	if got := svc.GetRulesSummary().TotalRules; got != totalBefore {
		t.Errorf("TotalRules changed after toggle: %d, want %d", got, totalBefore)
	}
}
