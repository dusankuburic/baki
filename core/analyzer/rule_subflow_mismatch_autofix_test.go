package analyzer

import (
	"testing"

	"pad-core/models"
)

// TestSubflowMismatch_MissingInputsHasNoAutoFix verifies that the
// missing-inputs finding from subflow-mismatch does NOT carry an AutoFix.
// Only the uncaptured-output pattern is auto-fixable (append-output). The
// missing-inputs pattern has no deterministic fix.
//
// This is a regression test for a bug where the ruleAutoFix map entry
// "subflow-mismatch": "append-output" caused the engine to stamp AutoFix
// on BOTH findings (uncaptured-output AND missing-inputs).
func TestSubflowMismatch_MissingInputsHasNoAutoFix(t *testing.T) {
	// Build a CALL block that calls a subflow with input variables.
	// The call doesn't provide any inputs → missing-inputs finding.
	callBlock := makeBlock("call1", "Call Target", models.BlockTypeSubflow, "CALL", 1)
	callBlock.SubflowID = "sf-main"

	// Target subflow with input variables.
	targetAction := makeBlock("ta1", "Work", models.BlockTypeAction, "SET", 1)
	targetAction.SubflowID = "sf-target"
	target := models.Subflow{
		ID:        "sf-target",
		Name:      "Target",
		Variables: []models.VariableDecl{{Name: "Input_X", Scope: "input"}},
		Blocks:    []models.Block{*targetAction},
	}

	sfMain := models.Subflow{ID: "sf-main", Name: "Main", Blocks: []models.Block{*callBlock}}
	flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{sfMain, target}}

	ctx := buildContext(flow, nil)
	rule := &SubflowMismatchRule{}
	findings := rule.Check(callBlock, ctx)

	// Should produce at least one finding (missing-inputs).
	if len(findings) == 0 {
		t.Fatal("expected at least one finding, got 0")
	}

	for _, f := range findings {
		pattern, _ := f.Metadata["pattern"].(string)
		if pattern == "missing-inputs" {
			if f.AutoFix != "" {
				t.Errorf("missing-inputs finding should NOT have AutoFix, got %q", f.AutoFix)
			}
		}
		if pattern == "uncaptured-output" {
			if f.AutoFix != "append-output" {
				t.Errorf("uncaptured-output finding should have AutoFix='append-output', got %q", f.AutoFix)
			}
		}
	}
}

// TestRuleAutoFix_SubflowMismatchNotInMap verifies that subflow-mismatch
// is NOT in the ruleAutoFix map (the inline AutoFix on uncaptured-output
// is the sole mechanism, preventing missing-inputs from being stamped).
func TestRuleAutoFix_SubflowMismatchNotInMap(t *testing.T) {
	if fix := RuleAutoFix("subflow-mismatch"); fix != "" {
		t.Errorf("subflow-mismatch should NOT be in ruleAutoFix map (inline only), got %q", fix)
	}
}
