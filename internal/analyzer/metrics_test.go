package analyzer

import (
	"testing"

	"pad-analyzer/internal/models"
)

// TestComputeSubflowMetrics_Complexity verifies the single-walk metrics
// computation produces the expected cyclomatic/cognitive complexity, max
// nesting depth and block count for a known nested structure:
//
//	Main:
//	  action a1
//	  condition c1
//	    action a2        (depth 1)
//	    else e1          (depth 1)
//	      action a3      (depth 2)
//	  loop l1
//	    action a4        (depth 1)
func TestComputeSubflowMetrics_Complexity(t *testing.T) {
	a1 := makeBlock("a1", "act1", models.BlockTypeAction, "SetVariable.Set", 0)
	a2 := makeBlock("a2", "act2", models.BlockTypeAction, "SetVariable.Set", 0)
	a3 := makeBlock("a3", "act3", models.BlockTypeAction, "SetVariable.Set", 0)
	a4 := makeBlock("a4", "act4", models.BlockTypeAction, "SetVariable.Set", 0)

	e1 := makeBlock("e1", "Else", models.BlockTypeElse, "Else", 0)
	e1.Children = []models.Block{*a3}

	c1 := makeBlock("c1", "If cond", models.BlockTypeCondition, "IF", 0)
	c1.Children = []models.Block{*a2, *e1}

	l1 := makeBlock("l1", "Loop items", models.BlockTypeLoop, "Loop.ForEach", 0)
	l1.Children = []models.Block{*a4}

	sf := makeSubflow("sf1", "Main", a1, c1, l1)
	doc := makeFlowWithSubflows(sf)

	fm := ComputeFlowMetrics(doc, nil)
	if len(fm.Subflows) != 1 {
		t.Fatalf("expected 1 subflow metric, got %d", len(fm.Subflows))
	}
	m := fm.Subflows[0]

	if m.BlockCount != 7 {
		t.Errorf("BlockCount = %d, want 7", m.BlockCount)
	}
	// 1 base + condition + else + loop.
	if m.CyclomaticComplexity != 4 {
		t.Errorf("CyclomaticComplexity = %d, want 4", m.CyclomaticComplexity)
	}
	// condition(+1) + else(+1) + loop(+1), all at depth 0.
	if m.CognitiveComplexity != 3 {
		t.Errorf("CognitiveComplexity = %d, want 3", m.CognitiveComplexity)
	}
	// a3 is nested two levels deep (condition → else → action).
	if m.MaxNestingDepth != 2 {
		t.Errorf("MaxNestingDepth = %d, want 2", m.MaxNestingDepth)
	}
}

// TestComputeFlowMetrics_FanInFanOut verifies fan-in/fan-out using the inverted
// call graph. Main→Helper, Main→Util, Helper→Util.
func TestComputeFlowMetrics_FanInFanOut(t *testing.T) {
	mainCall1 := makeBlock("mc1", "Call Helper", models.BlockTypeSubflow, "CALL", 0)
	mainCall2 := makeBlock("mc2", "Call Util", models.BlockTypeSubflow, "CALL", 0)
	helperCall := makeBlock("hc1", "Call Util", models.BlockTypeSubflow, "CALL", 0)
	utilAction := makeBlock("u1", "work", models.BlockTypeAction, "SetVariable.Set", 0)

	main := makeSubflow("main", "Main", mainCall1, mainCall2)
	helper := makeSubflow("helper", "Helper", helperCall)
	util := makeSubflow("util", "Util", utilAction)
	doc := makeFlowWithSubflows(main, helper, util)

	fm := ComputeFlowMetrics(doc, nil)
	byID := make(map[string]models.SubflowMetrics)
	for _, m := range fm.Subflows {
		byID[m.SubflowID] = m
	}

	cases := []struct {
		id              string
		wantIn, wantOut int
	}{
		{"main", 0, 2},
		{"helper", 1, 1},
		{"util", 2, 0},
	}
	for _, tc := range cases {
		m := byID[tc.id]
		if m.FanIn != tc.wantIn {
			t.Errorf("%s FanIn = %d, want %d", tc.id, m.FanIn, tc.wantIn)
		}
		if m.FanOut != tc.wantOut {
			t.Errorf("%s FanOut = %d, want %d", tc.id, m.FanOut, tc.wantOut)
		}
	}
}

// TestCallGraphMatchesExecutionGraph guards the unified call resolver: the
// metrics call graph and the rendered execution graph must produce identical
// edges (previously they used divergent resolution logic).
func TestCallGraphMatchesExecutionGraph(t *testing.T) {
	mainCall1 := makeBlock("mc1", "Call Helper", models.BlockTypeSubflow, "CALL", 0)
	mainCall2 := makeBlock("mc2", "Call Util", models.BlockTypeSubflow, "CALL", 0)
	helperCall := makeBlock("hc1", "Call Util", models.BlockTypeSubflow, "CALL", 0)
	utilAction := makeBlock("u1", "work", models.BlockTypeAction, "SetVariable.Set", 0)

	main := makeSubflow("main", "Main", mainCall1, mainCall2)
	helper := makeSubflow("helper", "Helper", helperCall)
	util := makeSubflow("util", "Util", utilAction)
	doc := makeFlowWithSubflows(main, helper, util)

	cg := buildCallGraph(doc)
	cgEdges := make(map[string]bool)
	for src, targets := range cg {
		for tgt := range targets {
			cgEdges[src+"->"+tgt] = true
		}
	}

	g := BuildExecutionGraph(doc, nil)
	egEdges := make(map[string]bool)
	for _, e := range g.Edges {
		egEdges[e.Source+"->"+e.Target] = true
	}

	if len(cgEdges) != len(egEdges) {
		t.Fatalf("edge count mismatch: callGraph=%d executionGraph=%d", len(cgEdges), len(egEdges))
	}
	for e := range cgEdges {
		if !egEdges[e] {
			t.Errorf("edge %s in call graph but not execution graph", e)
		}
	}
	want := map[string]bool{"main->helper": true, "main->util": true, "helper->util": true}
	for e := range want {
		if !egEdges[e] {
			t.Errorf("expected edge %s missing from execution graph", e)
		}
	}
}
