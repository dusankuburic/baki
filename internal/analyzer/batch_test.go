package analyzer

import (
	"testing"

	"pad-analyzer/internal/models"
)

func defaultTestSettings() *models.AppSettings {
	return models.DefaultSettings()
}

func TestBatchAnalysis(t *testing.T) {
	rules := AllRules()
	settings := defaultTestSettings()

	t.Run("analyzes_multiple_flows", func(t *testing.T) {
		docs := []*models.FlowDocument{
			makeFlowDoc("flow1", "Flow One", []models.Subflow{
				makeSubflow("main", "Main",
					makeBlock("b1", "Block 1", models.BlockTypeAction, "Action.Invoke", 0),
				),
			}),
			makeFlowDoc("flow2", "Flow Two", []models.Subflow{
				makeSubflow("main", "Main",
					makeBlock("b2", "Block 2", models.BlockTypeAction, "Action.Invoke", 0),
				),
			}),
		}

		batch := RunBatchAnalysis(docs, rules, settings)
		if batch.TotalFlows != 2 {
			t.Fatalf("expected 2 flows, got %d", batch.TotalFlows)
		}
		if len(batch.Results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(batch.Results))
		}
		if batch.Results[0].FlowName != "Flow One" {
			t.Errorf("expected Flow One, got %s", batch.Results[0].FlowName)
		}
		if batch.Results[1].FlowName != "Flow Two" {
			t.Errorf("expected Flow Two, got %s", batch.Results[1].FlowName)
		}
		if batch.DurationMs < 0 {
			t.Error("duration should be non-negative")
		}
	})

	t.Run("empty_batch", func(t *testing.T) {
		batch := RunBatchAnalysis(nil, rules, settings)
		if batch.TotalFlows != 0 {
			t.Fatalf("expected 0 flows, got %d", batch.TotalFlows)
		}
		if len(batch.Results) != 0 {
			t.Fatalf("expected 0 results, got %d", len(batch.Results))
		}
	})

	t.Run("findings_aggregated", func(t *testing.T) {
		docs := []*models.FlowDocument{
			makeFlowDoc("f1", "F1", []models.Subflow{
				makeSubflow("main", "Main",
					makeBlock("b1", "", models.BlockTypeAction, "Action.Invoke", 0),
				),
			}),
		}

		batch := RunBatchAnalysis(docs, rules, settings)
		r := batch.Results[0].Report
		expected := r.Stats.Errors + r.Stats.Warnings + r.Stats.Info
		if batch.TotalFindings != expected {
			t.Errorf("total findings mismatch: total=%d sum=%d", batch.TotalFindings, expected)
		}
	})
}

func TestDiffReports(t *testing.T) {
	t.Run("all_new_findings", func(t *testing.T) {
		old := &models.AnalysisReport{Findings: []models.Finding{}}
		new := &models.AnalysisReport{
			FlowID: "f1",
			Findings: []models.Finding{
				{RuleID: "R1", BlockID: "b1"},
				{RuleID: "R2", BlockID: "b2"},
			},
		}

		diff := DiffReports(old, new)
		if diff.AddedCount != 2 {
			t.Fatalf("expected 2 added, got %d", diff.AddedCount)
		}
		if diff.RemovedCount != 0 {
			t.Fatalf("expected 0 removed, got %d", diff.RemovedCount)
		}
		if diff.PersistedCount != 0 {
			t.Fatalf("expected 0 persisted, got %d", diff.PersistedCount)
		}
	})

	t.Run("all_removed", func(t *testing.T) {
		old := &models.AnalysisReport{
			FlowID: "f1",
			Findings: []models.Finding{
				{RuleID: "R1", BlockID: "b1"},
			},
		}
		new := &models.AnalysisReport{FlowID: "f1", Findings: []models.Finding{}}

		diff := DiffReports(old, new)
		if diff.AddedCount != 0 {
			t.Fatalf("expected 0 added, got %d", diff.AddedCount)
		}
		if diff.RemovedCount != 1 {
			t.Fatalf("expected 1 removed, got %d", diff.RemovedCount)
		}
	})

	t.Run("mixed_persisted_added_removed", func(t *testing.T) {
		old := &models.AnalysisReport{
			FlowID: "f1",
			Findings: []models.Finding{
				{RuleID: "R1", BlockID: "b1"},
				{RuleID: "R2", BlockID: "b2"},
				{RuleID: "R3", BlockID: "b3"},
			},
		}
		new := &models.AnalysisReport{
			FlowID: "f1",
			Findings: []models.Finding{
				{RuleID: "R1", BlockID: "b1"},
				{RuleID: "R2", BlockID: "b2"},
				{RuleID: "R4", BlockID: "b4"},
			},
		}

		diff := DiffReports(old, new)
		if diff.PersistedCount != 2 {
			t.Fatalf("expected 2 persisted, got %d", diff.PersistedCount)
		}
		if diff.AddedCount != 1 {
			t.Fatalf("expected 1 added, got %d", diff.AddedCount)
		}
		if diff.RemovedCount != 1 {
			t.Fatalf("expected 1 removed, got %d", diff.RemovedCount)
		}
	})

	t.Run("empty_both", func(t *testing.T) {
		old := &models.AnalysisReport{Findings: []models.Finding{}}
		new := &models.AnalysisReport{FlowID: "f1", Findings: []models.Finding{}}

		diff := DiffReports(old, new)
		if diff.AddedCount != 0 || diff.RemovedCount != 0 || diff.PersistedCount != 0 {
			t.Fatalf("expected all zeros: added=%d removed=%d persisted=%d",
				diff.AddedCount, diff.RemovedCount, diff.PersistedCount)
		}
	})
}

func makeFlowDoc(id, name string, subflows []models.Subflow) *models.FlowDocument {
	return &models.FlowDocument{
		ID:       id,
		Name:     name,
		Subflows: subflows,
	}
}
