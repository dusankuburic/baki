package analyzer

import (
	"testing"

	"pad-core/models"
)

func TestComputeDashboard_NoReports(t *testing.T) {
	stats := ComputeDashboard(nil)
	if stats.TotalFlowsAnalyzed != 0 {
		t.Errorf("expected 0 flows, got %d", stats.TotalFlowsAnalyzed)
	}
	if stats.FindingsBySeverity == nil {
		t.Error("expected non-nil FindingsBySeverity map")
	}
}

func TestComputeDashboard_SingleReport(t *testing.T) {
	report := &models.AnalysisReport{
		FlowID:   "flow-1",
		FlowName: "My Flow",
		Findings: []models.Finding{
			{Severity: "error", Category: "reliability", RuleID: "unhandled-error"},
			{Severity: "error", Category: "reliability", RuleID: "unhandled-error"},
			{Severity: "warning", Category: "performance", RuleID: "slow-pattern"},
		},
		Metrics: &models.FlowMetrics{HealthScore: 80},
	}

	stats := ComputeDashboard([]*models.AnalysisReport{report})

	if stats.TotalFlowsAnalyzed != 1 {
		t.Errorf("expected 1 flow, got %d", stats.TotalFlowsAnalyzed)
	}
	if stats.TotalFindings != 3 {
		t.Errorf("expected 3 findings, got %d", stats.TotalFindings)
	}
	if stats.FindingsBySeverity["error"] != 2 {
		t.Errorf("expected 2 errors, got %d", stats.FindingsBySeverity["error"])
	}
	if stats.FindingsBySeverity["warning"] != 1 {
		t.Errorf("expected 1 warning, got %d", stats.FindingsBySeverity["warning"])
	}
	if stats.FindingsByCategory["reliability"] != 2 {
		t.Errorf("expected 2 reliability, got %d", stats.FindingsByCategory["reliability"])
	}
	if stats.FindingsByRule["unhandled-error"] != 2 {
		t.Errorf("expected 2 unhandled-error, got %d", stats.FindingsByRule["unhandled-error"])
	}
	if stats.AvgHealthScore != 80.0 {
		t.Errorf("expected avg health 80.0, got %f", stats.AvgHealthScore)
	}
}

func TestComputeDashboard_MultipleReportsAvgHealth(t *testing.T) {
	reports := []*models.AnalysisReport{
		{FlowID: "a", Metrics: &models.FlowMetrics{HealthScore: 60}},
		{FlowID: "b", Metrics: &models.FlowMetrics{HealthScore: 80}},
		{FlowID: "c"},
	}
	stats := ComputeDashboard(reports)

	if stats.AvgHealthScore != 70.0 {
		t.Errorf("expected avg health (60+80)/2=70.0, got %f", stats.AvgHealthScore)
	}
}

func TestComputeDashboard_TopProblemFlowsSortedAndCapped(t *testing.T) {
	var reports []*models.AnalysisReport
	for i := 0; i < 15; i++ {
		reports = append(reports, &models.AnalysisReport{
			FlowID:   "flow-" + string(rune('a'+i)),
			FlowName: "Flow " + string(rune('a'+i)),
			Findings: make([]models.Finding, i+1),
		})
	}

	stats := ComputeDashboard(reports)

	if len(stats.TopProblemFlows) != 10 {
		t.Fatalf("expected 10 top problems (capped), got %d", len(stats.TopProblemFlows))
	}
	for i := 0; i < len(stats.TopProblemFlows)-1; i++ {
		if stats.TopProblemFlows[i].FindingCount < stats.TopProblemFlows[i+1].FindingCount {
			t.Errorf("expected descending sort at index %d", i)
		}
	}
}

func TestComputeDashboard_FlowsWithNoFindingsExcludedFromProblems(t *testing.T) {
	reports := []*models.AnalysisReport{
		{FlowID: "clean", FlowName: "Clean Flow"},
		{FlowID: "dirty", FlowName: "Dirty Flow", Findings: []models.Finding{{Severity: "warning"}}},
	}

	stats := ComputeDashboard(reports)

	if len(stats.TopProblemFlows) != 1 {
		t.Fatalf("expected 1 problem flow, got %d", len(stats.TopProblemFlows))
	}
	if stats.TopProblemFlows[0].FlowID != "dirty" {
		t.Errorf("expected dirty flow, got %s", stats.TopProblemFlows[0].FlowID)
	}
}

func TestComputeDashboard_EmptyCategoryExcluded(t *testing.T) {
	report := &models.AnalysisReport{
		FlowID:   "flow-1",
		FlowName: "Flow",
		Findings: []models.Finding{
			{Severity: "warning", Category: "", RuleID: "some-rule"},
		},
	}

	stats := ComputeDashboard([]*models.AnalysisReport{report})

	if _, exists := stats.FindingsByCategory[""]; exists {
		t.Error("empty category should not be counted")
	}
}
