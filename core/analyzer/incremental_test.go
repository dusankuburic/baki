package analyzer

import (
	"testing"

	"pad-core/models"
)

func TestAnalyzeRuleDependencies(t *testing.T) {
	t.Run("returns_dependencies", func(t *testing.T) {
		da := AnalyzeRuleDependencies()
		if len(da.Dependencies) == 0 {
			t.Fatal("expected some dependencies")
		}
		for _, d := range da.Dependencies {
			if d.FromRuleID == "" || d.ToRuleID == "" {
				t.Errorf("dependency has empty rule IDs: %+v", d)
			}
			if d.Reason == "" {
				t.Errorf("dependency missing reason: %+v", d)
			}
		}
	})

	t.Run("topo_order_contains_all_rules", func(t *testing.T) {
		da := AnalyzeRuleDependencies()
		rules := AllRules()
		if len(da.TopoOrder) != len(rules) {
			t.Fatalf("topo order has %d entries, expected %d rules", len(da.TopoOrder), len(rules))
		}
		seen := map[string]bool{}
		for _, id := range da.TopoOrder {
			if seen[id] {
				t.Errorf("duplicate rule in topo order: %s", id)
			}
			seen[id] = true
		}
	})

	t.Run("no_cycles", func(t *testing.T) {
		da := AnalyzeRuleDependencies()
		if len(da.Cycles) > 0 {
			t.Errorf("expected no cycles, got %d: %v", len(da.Cycles), da.Cycles)
		}
	})
}

func TestComputeSubflowHashes(t *testing.T) {
	t.Run("generates_hashes", func(t *testing.T) {
		doc := makeFlowDoc("f1", "Test", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b1", "Action", models.BlockTypeAction, "Action.Invoke", 0),
			),
			makeSubflow("sf2", "Helper",
				makeBlock("b2", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0),
			),
		})

		hashes := ComputeSubflowHashes(doc)
		if len(hashes) != 2 {
			t.Fatalf("expected 2 hashes, got %d", len(hashes))
		}
		if hashes[0].Hash == "" || hashes[1].Hash == "" {
			t.Error("hashes should not be empty")
		}
		if hashes[0].Hash == hashes[1].Hash {
			t.Error("different subflows should produce different hashes")
		}
	})

	t.Run("same_content_same_hash", func(t *testing.T) {
		doc := makeFlowDoc("f1", "Test", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b1", "Action", models.BlockTypeAction, "Action.Invoke", 0),
			),
		})

		h1 := ComputeSubflowHashes(doc)
		h2 := ComputeSubflowHashes(doc)
		if h1[0].Hash != h2[0].Hash {
			t.Errorf("same content should produce same hash: %s != %s", h1[0].Hash, h2[0].Hash)
		}
	})
}

func TestComputeDashboard(t *testing.T) {
	t.Run("empty_reports", func(t *testing.T) {
		d := ComputeDashboard(nil)
		if d.TotalFlowsAnalyzed != 0 {
			t.Errorf("expected 0 flows, got %d", d.TotalFlowsAnalyzed)
		}
		if d.FindingsBySeverity == nil {
			t.Error("expected non-nil severity map")
		}
	})

	t.Run("aggregates_multiple_reports", func(t *testing.T) {
		reports := []*models.AnalysisReport{
			{
				FlowID: "f1",
				Findings: []models.Finding{
					{Severity: models.SeverityError, RuleID: "R1", Category: "Reliability"},
					{Severity: models.SeverityWarning, RuleID: "R2", Category: "Style"},
				},
				Metrics: &models.FlowMetrics{HealthScore: 60},
			},
			{
				FlowID: "f2",
				Findings: []models.Finding{
					{Severity: models.SeverityError, RuleID: "R1", Category: "Security"},
				},
				Metrics: &models.FlowMetrics{HealthScore: 80},
			},
		}

		d := ComputeDashboard(reports)
		if d.TotalFlowsAnalyzed != 2 {
			t.Errorf("expected 2 flows, got %d", d.TotalFlowsAnalyzed)
		}
		if d.TotalFindings != 3 {
			t.Errorf("expected 3 findings, got %d", d.TotalFindings)
		}
		if d.FindingsBySeverity["error"] != 2 {
			t.Errorf("expected 2 errors, got %d", d.FindingsBySeverity["error"])
		}
		if d.FindingsByRule["R1"] != 2 {
			t.Errorf("expected 2 R1 findings, got %d", d.FindingsByRule["R1"])
		}
		if d.AvgHealthScore != 70.0 {
			t.Errorf("expected avg health 70, got %.1f", d.AvgHealthScore)
		}
		if len(d.TopProblemFlows) != 2 {
			t.Errorf("expected 2 problem flows, got %d", len(d.TopProblemFlows))
		}
	})
}
