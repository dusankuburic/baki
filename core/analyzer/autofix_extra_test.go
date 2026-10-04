package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestReplaceWithVariablePatch_RoundTripResolvesFinding is the round-trip gate
// for the replace-with-variable fixer across hardcoded-filepath and hardcoded-url.
// For each: parse → analyze → find the rule's finding → apply the patch to the
// raw source → re-parse (faithful) → re-analyze → assert the finding is gone
// (effective). The fix replaces the hardcoded literal with %input_<key>% so the
// rule's variable-reference guard skips the property on re-analysis.
func TestReplaceWithVariablePatch_FilePathAndURL(t *testing.T) {
	cases := []struct {
		name   string
		ruleID string
		source string
	}{
		{
			name:   "hardcoded-filepath absolute windows path",
			ruleID: "hardcoded-filepath",
			source: "#Region \"Main\"\nFolder.Get Path: C:\\Users\\admin\\data\n#EndRegion\n",
		},
		{
			name:   "hardcoded-url https endpoint",
			ruleID: "hardcoded-url",
			source: "#Region \"Main\"\nHTTPClient.InvokeUrl Method: GET Url: https://api.example.com/v2\n#EndRegion\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := parser.ParseText(tc.source, "Main.txt", int64(len(tc.source)))
			if err != nil {
				t.Fatalf("initial parse: %v", err)
			}
			report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
			var finding *models.Finding
			for i := range report.Findings {
				if report.Findings[i].RuleID == tc.ruleID {
					finding = &report.Findings[i]
					break
				}
			}
			if finding == nil {
				t.Fatalf("expected a %s finding, got: %v", tc.ruleID, ruleIDs(report.Findings))
			}
			block := doc.BlocksByID[finding.BlockID]
			if block == nil {
				t.Fatalf("finding block %s not in doc", finding.BlockID)
			}
			propKey, _ := finding.Metadata["property"].(string)
			if propKey == "" {
				t.Fatalf("finding has no property metadata: %+v", finding.Metadata)
			}

			patched := ApplyPatch(tc.source, ReplaceWithVariablePatch(block, propKey))

			doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
			if err != nil {
				t.Fatalf("re-parse after replace-with-variable failed (not faithful): %v\npatched:\n%s", err, patched)
			}
			report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
			for _, f := range report2.Findings {
				if f.RuleID == tc.ruleID {
					t.Errorf("%s still present after replace-with-variable; finding: %+v\npatched:\n%s", tc.ruleID, f, patched)
				}
			}
		})
	}
}

// TestInsertDelayInLoopPatch_RoundTripResolvesFinding is the round-trip gate for
// the insert-delay-in-loop fixer (slow-pattern): a LOOP with UI automation but
// no delay must, after the patch inserts WAIT 1 inside the loop body, re-parse
// cleanly and no longer be flagged (the loop now contains a Wait action).
func TestInsertDelayInLoopPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = "#Region \"Main\"\nLOOP WHILE %X% < 10\n    WebAutomation.Click Element: 'button1'\nEND\n#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("initial parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var slowFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "slow-pattern" {
			slowFinding = &report.Findings[i]
			break
		}
	}
	if slowFinding == nil {
		t.Fatalf("expected a slow-pattern finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[slowFinding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", slowFinding.BlockID)
	}

	patch := InsertDelayInLoopPatch(block)
	if len(patch.Ops) == 0 {
		t.Fatalf("expected non-empty patch")
	}
	patched := ApplyPatch(source, patch)
	if !strings.Contains(patched, "WAIT 1") {
		t.Fatalf("expected WAIT 1 inside loop, got:\n%s", patched)
	}

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after insert-delay-in-loop failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "slow-pattern" {
			t.Errorf("slow-pattern still present after insert-delay-in-loop; finding: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestInsertDelayInLoopPatch_EmptyLoopFallsBack verifies that a loop with no
// children falls back to InsertDelayPatch (insert before the loop line).
func TestInsertDelayInLoopPatch_EmptyLoopFallsBack(t *testing.T) {
	block := &models.Block{
		Type:       models.BlockTypeLoop,
		RawType:    "LOOP",
		LineNumber: 3,
		Indent:     1,
	}
	patch := InsertDelayInLoopPatch(block)
	if len(patch.Ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(patch.Ops))
	}
	if patch.Ops[0].BeforeLine != 3 {
		t.Errorf("expected BeforeLine=3 (fallback to loop line), got %d", patch.Ops[0].BeforeLine)
	}
}

// TestInsertDelayInLoopPatch_InsertsBeforeFirstChild verifies the insert target
// is the first child's line (inside the loop body), not the loop line itself.
func TestInsertDelayInLoopPatch_InsertsBeforeFirstChild(t *testing.T) {
	child := &models.Block{LineNumber: 5}
	block := &models.Block{
		Type:       models.BlockTypeLoop,
		RawType:    "LOOP",
		LineNumber: 3,
		// block.Indent is the parser's RAW leading column count (one level of
		// 4-space indentation == 4 columns), not a level count.
		Indent:   4,
		Children: []models.Block{*child},
	}
	patch := InsertDelayInLoopPatch(block)
	if len(patch.Ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(patch.Ops))
	}
	if patch.Ops[0].BeforeLine != 5 {
		t.Errorf("expected BeforeLine=5 (first child), got %d", patch.Ops[0].BeforeLine)
	}
	indentSpace := strings.Repeat(" ", 4+4) // loop column (4) + one level (4)
	if !strings.Contains(patch.Ops[0].Lines[0], indentSpace+"WAIT 1") {
		t.Errorf("expected WAIT one level deeper than the loop, got %q", patch.Ops[0].Lines[0])
	}
}

// TestAppendOutputPatch_PatchMechanics verifies the append-output patch
// produces the correct append op. The uncaptured-output finding only fires
// when Subflow.Variables is populated (cloud-stored flows with declared
// variables), so the full round-trip gate lives in
// autofix_roundtrip_gaps_test.go (it parses a real CALL source and declares
// the output variable on the parsed subflow). The output name must be BARE
// (PAD output-capture syntax `=> VarName`, not `%VarName%`) or the parser
// won't set _output and the fix won't resolve its finding.
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
		t.Errorf("Lines = %v, expected => Output_Result", patch.Ops[0].Lines)
	}
	if strings.Contains(patch.Ops[0].Lines[0], "%") {
		t.Errorf("output capture must be bare (no %% delimiters): %v", patch.Ops[0].Lines)
	}
	// Verify the patch applies correctly
	source := "line1\nCALL Helper\nline4"
	patched := ApplyPatch(source, patch)
	if !strings.Contains(patched, "CALL Helper => Output_Result") {
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
