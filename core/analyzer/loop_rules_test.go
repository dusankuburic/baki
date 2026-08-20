package analyzer

import (
	"testing"

	"pad-core/models"
)

// TestWideLoop_DescendantCountMatchesSubtreeWalk is the behavior gate for the
// DescendantCount index: wide-loop findings must be identical to the old
// per-LOOP subtree walk on a nested-loop fixture (counts include nested
// loops' bodies, exclude End/Comment blocks and the loop itself).
func TestWideLoop_DescendantCountMatchesSubtreeWalk(t *testing.T) {
	flow := buildNestedLoopFlow(5, 6) // 5 levels × 6 actions ≈ 35+ blocks deep-chain
	report := RunAnalysis(flow, AllRules(), nil, nil)
	found := 0
	for _, f := range report.Findings {
		if f.RuleID == "wide-loop" {
			found++
		}
	}
	if found == 0 {
		t.Fatal("expected at least one wide-loop finding on the nested fixture")
	}

	// Cross-check the index against a manual subtree walk for every loop.
	ctx := buildContext(flow, nil)
	loops := 0
	for id, b := range ctx.AllBlocks {
		if b.Type != models.BlockTypeLoop {
			continue
		}
		loops++
		want := 0
		walkBlockTree(b, func(x *models.Block) {
			if x.ID == b.ID || x.Type == models.BlockTypeEnd || x.Type == models.BlockTypeComment {
				return
			}
			want++
		})
		if got := ctx.DescendantCount[id]; got != want {
			t.Errorf("DescendantCount[%s] = %d, want %d (subtree walk)", id, got, want)
		}
	}
	if loops == 0 {
		t.Fatal("fixture produced no loops")
	}
}

// TestSlowPattern_OwnBodySemantics pins the deliberate own-body behavior:
// a WAIT inside a NESTED loop no longer suppresses the OUTER loop's finding
// (that wait only paces the inner loop's iterations), and a nested loop with
// UI but no wait of its own is flagged on itself.
func TestSlowPattern_OwnBodySemantics(t *testing.T) {
	outer := &models.Block{ID: "outer", Name: "Loop", Type: models.BlockTypeLoop, RawType: "Loop.Loop", SubflowID: "sf1",
		Children: []models.Block{
			{ID: "ui1", Name: "Click", Type: models.BlockTypeAction, RawType: "WebAutomation.Click.Click", SubflowID: "sf1"},
			{ID: "inner", Name: "Loop", Type: models.BlockTypeLoop, RawType: "Loop.Loop", SubflowID: "sf1",
				Children: []models.Block{
					{ID: "wait1", Name: "Wait 1", Type: models.BlockTypeAction, RawType: "WAIT", SubflowID: "sf1"},
				}},
		}}
	flow := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*outer}}}}

	report := RunAnalysis(flow, AllRules(), nil, nil)
	outerFlagged, innerFlagged := false, false
	for _, f := range report.Findings {
		if f.RuleID != "slow-pattern" {
			continue
		}
		switch f.BlockID {
		case "outer":
			outerFlagged = true // outer's own body has UI (ui1) and no wait
		case "inner":
			innerFlagged = true
		}
	}
	if !outerFlagged {
		t.Error("outer loop must be flagged: its own body has UI automation with no wait (the nested WAIT only paces the inner loop)")
	}
	if innerFlagged {
		t.Error("inner loop must NOT be flagged: it has a wait in its own body")
	}
}
