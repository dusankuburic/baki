package analyzer

import (
	"testing"

	"pad-core/models"
)

// gotoFindings runs only the GOTO anti-pattern rule over a flow and returns the
// findings, matching the per-block dispatch the engine uses.
func gotoFindings(t *testing.T, flow *models.FlowDocument) []models.Finding {
	t.Helper()
	rule := &GotoAntipatternRule{}
	ctx := buildContext(flow, nil)
	var findings []models.Finding
	for _, b := range ctx.AllBlocks {
		findings = append(findings, rule.Check(b, ctx)...)
	}
	return findings
}

// A GOTO inside a loop that jumps to a top-level LABEL crosses a scope boundary
// (different nesting depth) and must be flagged.
func TestGotoAntipattern_JumpOutOfLoop_IsScopeBreaking(t *testing.T) {
	loop := makeBlock("loop1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
	g := makeBlock("g1", "Goto End", models.BlockTypeAction, "GOTO", 4)
	g.Properties = map[string]string{"_target": "End"}
	loop.Children = []models.Block{*g}
	label := makeBlock("end", "End", models.BlockTypeAction, "LABEL", 0)

	flow := makeFlowWithSubflows(makeSubflow("sf1", "Main", loop, label))

	f := gotoFindings(t, flow)
	if len(f) != 1 {
		t.Fatalf("expected 1 scope-breaking finding, got %d", len(f))
	}
	if f[0].RuleID != "goto-antipattern" {
		t.Errorf("ruleID = %q, want goto-antipattern", f[0].RuleID)
	}
}

// A GOTO and its target LABEL at the same top level share a scope, so there is
// no boundary to break.
func TestGotoAntipattern_SameScope_NoFinding(t *testing.T) {
	g := makeBlock("g1", "Goto End", models.BlockTypeAction, "GOTO", 0)
	g.Properties = map[string]string{"_target": "End"}
	label := makeBlock("end", "End", models.BlockTypeAction, "LABEL", 0)

	flow := makeFlowWithSubflows(makeSubflow("sf1", "Main", g, label))

	if f := gotoFindings(t, flow); len(f) != 0 {
		t.Errorf("expected 0 findings for a same-scope GOTO, got %d", len(f))
	}
}

// Label resolution is case-insensitive: a GOTO targeting "EXIT" must resolve to
// a LABEL named "Exit" and still be flagged when it breaks scope.
func TestGotoAntipattern_LabelMatchIsCaseInsensitive(t *testing.T) {
	loop := makeBlock("loop1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
	g := makeBlock("g1", "Goto target", models.BlockTypeAction, "GOTO", 4)
	g.Properties = map[string]string{"_target": "EXIT"}
	loop.Children = []models.Block{*g}
	label := makeBlock("lbl", "Exit", models.BlockTypeAction, "LABEL", 0)

	flow := makeFlowWithSubflows(makeSubflow("sf1", "Main", loop, label))

	if f := gotoFindings(t, flow); len(f) != 1 {
		t.Fatalf("expected the GOTO to resolve 'EXIT'->'Exit' case-insensitively and flag, got %d findings", len(f))
	}
}

// A GOTO whose target has no matching LABEL anywhere is not a scope break (the
// orphaned-label / unknown-target case yields no finding from this rule).
func TestGotoAntipattern_UnknownLabel_NoFinding(t *testing.T) {
	loop := makeBlock("loop1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
	g := makeBlock("g1", "Goto Nowhere", models.BlockTypeAction, "GOTO", 4)
	g.Properties = map[string]string{"_target": "Nowhere"}
	loop.Children = []models.Block{*g}

	flow := makeFlowWithSubflows(makeSubflow("sf1", "Main", loop))

	if f := gotoFindings(t, flow); len(f) != 0 {
		t.Errorf("expected 0 findings when the target label is missing, got %d", len(f))
	}
}

// Same nesting depth but different scope containers (a loop vs. a condition) is
// still a scope break — exercises the ancestor-set comparison.
func TestGotoAntipattern_AcrossSiblingScopes_IsScopeBreaking(t *testing.T) {
	loop := makeBlock("loop1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
	g := makeBlock("g1", "Goto End", models.BlockTypeAction, "GOTO", 4)
	g.Properties = map[string]string{"_target": "End"}
	loop.Children = []models.Block{*g}

	cond := makeBlock("cond1", "If", models.BlockTypeCondition, "Conditionals.If", 0)
	label := makeBlock("end", "End", models.BlockTypeAction, "LABEL", 4)
	cond.Children = []models.Block{*label}

	flow := makeFlowWithSubflows(makeSubflow("sf1", "Main", loop, cond))

	if f := gotoFindings(t, flow); len(f) != 1 {
		t.Fatalf("expected 1 finding for a GOTO crossing sibling loop/condition scopes, got %d", len(f))
	}
}
