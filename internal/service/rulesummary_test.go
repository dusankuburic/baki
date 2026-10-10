package service

import (
	"context"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

// TestGetRulesSummary verifies the catalog rollup the dashboard consumes for
// its "auto-fixable rules" and "confidence distribution" KPIs. The totals must
// match the live rule catalog so a new rule can never silently fall off the
// summary.
func TestGetRulesSummary(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil, AnalysisOptions{})
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	summary := svc.GetRulesSummary()

	totalRules := len(analyzer.AllRules())
	if summary.TotalRules != totalRules {
		t.Errorf("TotalRules = %d, want %d (len(AllRules))", summary.TotalRules, totalRules)
	}

	// Auto-fixable rules: the dashboard advertises "fix availability", so the
	// count must equal the number of rules with a one-click fix: the one the
	// engine stamps or one the rule stamps itself (subflow-mismatch).
	wantFixable := 0
	for _, r := range analyzer.AllRules() {
		if m := analyzer.MetaOf(r); m.AutoFix != "" || len(m.DirectFixes) > 0 {
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
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil, AnalysisOptions{})
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}

	totalBefore := svc.GetRulesSummary().TotalRules

	// Disabling a rule changes its Enabled flag but must not change the catalog
	// total the dashboard shows.
	if err := svc.SetRuleEnabled(context.Background(), "deep-nesting", false); err != nil {
		t.Fatalf("SetRuleEnabled: %v", err)
	}
	if got := svc.GetRulesSummary().TotalRules; got != totalBefore {
		t.Errorf("TotalRules changed after toggle: %d, want %d", got, totalBefore)
	}
}

// s2-4: the catalog reports a configured severity as the engine applies it. A stored "critical" showed as the
// rule's severity, matched no option, and the threshold save re-posted it to the config route, which refuses it.
func TestCatalogRule_EffectiveConfiguredSeverity(t *testing.T) {
	r := &analyzer.WaitZeroRule{}
	for configured, want := range map[string]models.Severity{
		"critical": models.SeverityError,
		" Warning": models.SeverityWarning,
		"":         r.DefaultSeverity(),
	} {
		s := models.DefaultSettings()
		s.Analysis.Rules = map[string]models.RuleConfig{r.ID(): {Enabled: true, Severity: configured}}
		if got := catalogRule(r, s, nil).DefaultSeverity; got != want {
			t.Errorf("configured %q: catalog severity %q, want %q", configured, got, want)
		}
	}
}
