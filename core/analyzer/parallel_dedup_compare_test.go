package analyzer

import (
	"testing"

	"pad-core/models"
)

func TestDeduplicateFindings(t *testing.T) {
	t.Run("removes_exact_duplicates", func(t *testing.T) {
		findings := []models.Finding{
			{BlockID: "b1", Title: "Issue A", RuleID: "R1"},
			{BlockID: "b1", Title: "Issue A", RuleID: "R2"},
			{BlockID: "b2", Title: "Issue B", RuleID: "R1"},
		}

		deduped, groups := DeduplicateFindings(findings)
		if len(deduped) != 2 {
			t.Fatalf("expected 2 deduped findings, got %d", len(deduped))
		}
		if len(groups) != 2 {
			t.Fatalf("expected 2 groups, got %d", len(groups))
		}
	})

	t.Run("empty_input", func(t *testing.T) {
		deduped, groups := DeduplicateFindings(nil)
		if len(deduped) != 0 {
			t.Errorf("expected 0 deduped, got %d", len(deduped))
		}
		if len(groups) != 0 {
			t.Errorf("expected 0 groups, got %d", len(groups))
		}
	})

	t.Run("no_duplicates", func(t *testing.T) {
		findings := []models.Finding{
			{BlockID: "b1", Title: "Issue A", RuleID: "R1"},
			{BlockID: "b2", Title: "Issue B", RuleID: "R2"},
		}

		deduped, _ := DeduplicateFindings(findings)
		if len(deduped) != 2 {
			t.Fatalf("expected 2 deduped, got %d", len(deduped))
		}
	})
}

func TestCompareFlows(t *testing.T) {
	t.Run("identical_flows", func(t *testing.T) {
		docA := makeFlowDoc("a", "A", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b1", "Action", models.BlockTypeAction, "Action.Invoke", 0),
			),
		})
		docB := makeFlowDoc("b", "B", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b2", "Action", models.BlockTypeAction, "Action.Invoke", 0),
			),
		})

		result := CompareFlows(docA, docB)
		if result.FlowAID != "a" || result.FlowBID != "b" {
			t.Errorf("flow ID mismatch: a=%s b=%s", result.FlowAID, result.FlowBID)
		}
		if result.SharedBlocks == 0 {
			t.Error("expected some shared blocks for matching rawType")
		}
	})

	t.Run("completely_different", func(t *testing.T) {
		docA := makeFlowDoc("a", "A", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b1", "Action", models.BlockTypeAction, "Action.Invoke", 0),
			),
		})
		docB := makeFlowDoc("b", "B", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b2", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0),
			),
		})

		result := CompareFlows(docA, docB)
		if len(result.SubflowDiff) == 0 {
			t.Fatal("expected subflow diffs")
		}
	})

	t.Run("different_subflow_names", func(t *testing.T) {
		docA := makeFlowDoc("a", "A", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b1", "Action", models.BlockTypeAction, "Action.Invoke", 0),
			),
		})
		docB := makeFlowDoc("b", "B", []models.Subflow{
			makeSubflow("sf2", "Helper",
				makeBlock("b2", "Action", models.BlockTypeAction, "Action.Invoke", 0),
			),
		})

		result := CompareFlows(docA, docB)
		if result.Similarity == 1.0 {
			t.Error("different subflows should not have 100% similarity")
		}
	})

	t.Run("similarity_calculation", func(t *testing.T) {
		docA := makeFlowDoc("a", "A", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b1", "Shared", models.BlockTypeAction, "Action.Invoke", 0),
			),
		})
		docB := makeFlowDoc("b", "B", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b3", "Shared", models.BlockTypeAction, "Action.Invoke", 0),
			),
		})

		result := CompareFlows(docA, docB)
		if result.SharedBlocks < 1 {
			t.Errorf("expected at least 1 shared block, got %d", result.SharedBlocks)
		}
		if result.AddedBlocks > 0 {
			t.Errorf("expected 0 added blocks, got %d", result.AddedBlocks)
		}
		if result.RemovedBlocks > 0 {
			t.Errorf("expected 0 removed blocks, got %d", result.RemovedBlocks)
		}
		if result.Similarity != 1.0 {
			t.Errorf("expected 100%% similarity for matching blocks, got %.2f", result.Similarity)
		}
	})
}
