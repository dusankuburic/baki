package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestAppendOutputPatch_PatchMechanics verifies the append-output patch
// produces the correct append op. The uncaptured-output finding only fires
// when Subflow.Variables is populated (cloud-stored flows with declared
// variables), so a text-based round-trip isn't possible — but the patch
// mechanics (append ` => %Output_Result%` to the CALL line) are testable
// in isolation.
func TestAppendOutputPatch_PatchMechanics(t *testing.T) {
	block := &models.Block{LineNumber: 2}
	patch := AppendOutputPatch(block)
	if len(patch.Ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(patch.Ops))
	}
	if patch.Ops[0].Kind != "append" {
		t.Errorf("kind = %q, want append", patch.Ops[0].Kind)
	}
	if patch.Ops[0].StartLine != 2 {
		t.Errorf("StartLine = %d, want 2", patch.Ops[0].StartLine)
	}
	if len(patch.Ops[0].Lines) != 1 || !strings.Contains(patch.Ops[0].Lines[0], "Output_Result") {
		t.Errorf("Lines = %v, expected => %%Output_Result%%", patch.Ops[0].Lines)
	}
	// Verify the patch applies correctly
	source := "line1\nCALL Helper\nline4"
	patched := ApplyPatch(source, patch)
	if !strings.Contains(patched, "CALL Helper => %Output_Result%") {
		t.Errorf("expected output capture appended, got: %s", patched)
	}
}

// TestMaskSensitiveVariablePatch_RoundTripResolvesFinding verifies the
// mask-sensitive-variable fixer resolves sensitive-exposure. After replacing
// %Password% with '*** MASKED ***', the variable reference is gone from the
// block's properties, so block.Variables no longer contains "Password".
func TestMaskSensitiveVariablePatch_RoundTripResolvesFinding(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"SET Password TO 'secret123'\n" +
		"Text.WriteToFile TextToWrite: %Password% FilePath: 'log.txt'\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var exposureFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "sensitive-exposure" {
			exposureFinding = &report.Findings[i]
			break
		}
	}
	if exposureFinding == nil {
		t.Fatalf("expected a sensitive-exposure finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[exposureFinding.BlockID]
	if block == nil {
		t.Fatalf("block not found")
	}
	varName, _ := exposureFinding.Metadata["variable"].(string)
	if varName == "" {
		t.Fatalf("finding has no variable metadata")
	}

	patched := ApplyPatch(source, MaskSensitiveVariablePatch(block, varName))
	if strings.Contains(patched, "%"+varName+"%") {
		t.Fatalf("expected %%%s%% gone from patched source, got:\n%s", varName, patched)
	}
	if !strings.Contains(patched, "*** MASKED ***") {
		t.Fatalf("expected MASKED in patched source, got:\n%s", patched)
	}

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "sensitive-exposure" {
			t.Errorf("sensitive-exposure still present after fix\npatched:\n%s", patched)
		}
	}
}

// TestMaskSensitiveVariablePatch_EmptyVarReturnsEmpty verifies the fixer
// declines gracefully when no variable name is provided.
func TestMaskSensitiveVariablePatch_EmptyVarReturnsEmpty(t *testing.T) {
	block := &models.Block{LineNumber: 1}
	patch := MaskSensitiveVariablePatch(block, "")
	if len(patch.Ops) != 0 {
		t.Errorf("expected empty patch for empty varName, got %d ops", len(patch.Ops))
	}
}

// TestCustomRule_AutoFix stamps the configured autoFix on findings.
func TestCustomRule_AutoFix(t *testing.T) {
	rule, err := NewCustomRule(CustomRuleConfig{
		ID:           "custom-test",
		Name:         "Test Rule",
		Description:  "Test",
		Severity:     "warning",
		Category:     "Style",
		RawTypeMatch: "SET",
		AutoFix:      "remove-block",
	})
	if err != nil {
		t.Fatalf("NewCustomRule: %v", err)
	}
	block := &models.Block{
		ID:        "b1",
		Type:      models.BlockTypeVariable,
		RawType:   "SET",
		Name:      "TestVar",
		SubflowID: "sf1",
	}
	ctx := &RuleContext{}
	findings := rule.Check(block, ctx)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].AutoFix != "remove-block" {
		t.Errorf("AutoFix = %q, want remove-block", findings[0].AutoFix)
	}
}

// TestCustomRule_InvalidAutoFixRejects verifies that an unknown fixType
// causes NewCustomRule to return an error.
func TestCustomRule_InvalidAutoFixRejects(t *testing.T) {
	_, err := NewCustomRule(CustomRuleConfig{
		ID:      "custom-bad",
		Name:    "Bad",
		AutoFix: "bogus-fixer",
	})
	if err == nil {
		t.Fatal("expected error for unknown autoFix, got nil")
	}
}

// TestCustomRule_NoAutoFix leaves the finding's AutoFix empty.
func TestCustomRule_NoAutoFix(t *testing.T) {
	rule, err := NewCustomRule(CustomRuleConfig{
		ID:       "custom-plain",
		Name:     "Plain",
		Severity: "warning",
		Category: "Style",
	})
	if err != nil {
		t.Fatalf("NewCustomRule: %v", err)
	}
	block := &models.Block{
		ID:        "b1",
		Type:      models.BlockTypeAction,
		RawType:   "Test.Action",
		Name:      "Test",
		SubflowID: "sf1",
	}
	ctx := &RuleContext{}
	findings := rule.Check(block, ctx)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].AutoFix != "" {
		t.Errorf("expected empty AutoFix, got %q", findings[0].AutoFix)
	}
}
