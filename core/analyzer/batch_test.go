package analyzer

import (
	"testing"

	"pad-core/models"
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

func TestComputeDrift(t *testing.T) {
	report := &models.AnalysisReport{
		FlowID: "f1",
		Findings: []models.Finding{
			{RuleID: "hardcoded-credential", BlockID: "b1", Severity: models.SeverityError},
			{RuleID: "dead-code", BlockID: "b2", Severity: models.SeverityWarning},
			{RuleID: "missing-delay", BlockID: "b3", Severity: models.SeverityInfo},
		},
	}

	t.Run("nil baseline treats everything as new", func(t *testing.T) {
		d := ComputeDrift("f1", report, nil)
		if d.HasBaseline {
			t.Error("expected HasBaseline=false for nil baseline")
		}
		if len(d.New) != 3 {
			t.Fatalf("expected 3 new findings, got %d", len(d.New))
		}
		if d.NewErrors != 1 || d.NewWarnings != 1 || d.NewInfo != 1 {
			t.Errorf("unexpected severity counts: %+v", d)
		}
	})

	t.Run("baseline filters accepted findings", func(t *testing.T) {
		// Accept the credential and dead-code findings; only missing-delay is new.
		baseline := []string{"hardcoded-credential:b1", "dead-code:b2"}
		d := ComputeDrift("f1", report, baseline)
		if !d.HasBaseline {
			t.Error("expected HasBaseline=true for non-nil baseline")
		}
		if len(d.New) != 1 {
			t.Fatalf("expected 1 new finding, got %d", len(d.New))
		}
		if d.New[0].Key() != "missing-delay:b3" {
			t.Errorf("expected missing-delay:b3 as the new finding, got %q", d.New[0].Key())
		}
		if d.NewErrors != 0 || d.NewWarnings != 0 || d.NewInfo != 1 {
			t.Errorf("unexpected severity counts: %+v", d)
		}
	})

	t.Run("empty non-nil baseline still counts as having a baseline", func(t *testing.T) {
		d := ComputeDrift("f1", report, []string{})
		if !d.HasBaseline {
			t.Error("expected HasBaseline=true for empty (non-nil) baseline")
		}
		if len(d.New) != 3 {
			t.Errorf("empty baseline accepts nothing; expected 3 new, got %d", len(d.New))
		}
	})

	t.Run("nil report yields non-nil empty New", func(t *testing.T) {
		d := ComputeDrift("f1", nil, []string{"x:y"})
		if d.New == nil {
			t.Error("New must be a non-nil slice")
		}
		if len(d.New) != 0 {
			t.Errorf("expected 0 new findings for nil report, got %d", len(d.New))
		}
	})
}
