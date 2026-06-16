package analyzer

import (
	"testing"

	"pad-core/models"
)

// callTo builds a Main subflow containing a single CALL block plus the given
// target subflow(s), and returns the analyzed findings for the call.
func subflowMismatchCheck(t *testing.T, call *models.Block, targets ...models.Subflow) []models.Finding {
	t.Helper()
	call.SubflowID = "sf1"
	subs := append([]models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*call}}}, targets...)
	flow := &models.FlowDocument{ID: "t", Subflows: subs}
	ctx := buildContext(flow, nil)
	return (&SubflowMismatchRule{}).Check(call, ctx)
}

// A call to a subflow that declares input variables but supplies none is flagged.
func TestSubflowMismatch_MissingInputs_EmitsFinding(t *testing.T) {
	call := makeBlock("b1", "Call Helper", models.BlockTypeSubflow, "CALL", 0)
	call.Properties = map[string]string{}
	target := models.Subflow{ID: "sf2", Name: "Helper",
		Variables: []models.VariableDecl{{Name: "Input_Arg", Type: "string", Scope: "input"}}}

	got := subflowMismatchCheck(t, call, target)
	if len(got) != 1 {
		t.Fatalf("expected 1 missing-inputs finding, got %d", len(got))
	}
	if got[0].RuleID != "subflow-mismatch" {
		t.Errorf("ruleID = %q, want subflow-mismatch", got[0].RuleID)
	}
}

// Supplying the required input via the call's Variables list satisfies the rule.
func TestSubflowMismatch_InputsProvidedViaVariables_NoFinding(t *testing.T) {
	call := makeBlock("b1", "Call Helper", models.BlockTypeSubflow, "CALL", 0)
	call.Properties = map[string]string{}
	call.Variables = []string{"Input_Arg"}
	target := models.Subflow{ID: "sf2", Name: "Helper",
		Variables: []models.VariableDecl{{Name: "Input_Arg", Type: "string", Scope: "input"}}}

	if got := subflowMismatchCheck(t, call, target); len(got) != 0 {
		t.Errorf("expected 0 findings when the input is provided, got %d", len(got))
	}
}

// Inputs referenced as %Name% in a property value also count as provided.
func TestSubflowMismatch_InputsProvidedViaPropertyRef_NoFinding(t *testing.T) {
	call := makeBlock("b1", "Call Helper", models.BlockTypeSubflow, "CALL", 0)
	call.Properties = map[string]string{"arg0": "%Input_Arg%"}
	target := models.Subflow{ID: "sf2", Name: "Helper",
		Variables: []models.VariableDecl{{Name: "Input_Arg", Type: "string", Scope: "input"}}}

	if got := subflowMismatchCheck(t, call, target); len(got) != 0 {
		t.Errorf("expected 0 findings when the input is provided via %%ref%%, got %d", len(got))
	}
}

// Two subflows share a name: SubflowByName keeps the FIRST in document order, so
// a call resolves to the no-output Helper and produces no uncaptured-output
// finding — locking in the precomputed index's first-wins semantics.
func TestSubflowMismatch_DuplicateName_ResolvesFirstInDocumentOrder(t *testing.T) {
	call := makeBlock("b1", "Call Helper", models.BlockTypeSubflow, "CALL", 0)
	call.Properties = map[string]string{}
	first := models.Subflow{ID: "sf2", Name: "Helper"}
	second := models.Subflow{ID: "sf3", Name: "Helper",
		Variables: []models.VariableDecl{{Name: "Output_Result", Type: "string", Scope: "output"}}}

	if got := subflowMismatchCheck(t, call, first, second); len(got) != 0 {
		t.Errorf("expected 0 findings (resolves to the first, no-output Helper), got %d", len(got))
	}
}

// The call target can be named by a property (subflowName) rather than the
// "Call X" block name — exercises resolveSubflowTarget's property branch.
func TestSubflowMismatch_TargetFromProperty_EmitsFinding(t *testing.T) {
	call := makeBlock("b1", "Invoke subflow", models.BlockTypeSubflow, "RunSubflow", 0)
	call.Properties = map[string]string{"subflowName": "Helper"}
	target := models.Subflow{ID: "sf2", Name: "Helper",
		Variables: []models.VariableDecl{{Name: "Output_Result", Type: "string", Scope: "output"}}}

	got := subflowMismatchCheck(t, call, target)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding when target resolved via property, got %d", len(got))
	}
}
