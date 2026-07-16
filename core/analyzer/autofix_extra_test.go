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
		Indent:     1,
		Children:   []models.Block{*child},
	}
	patch := InsertDelayInLoopPatch(block)
	if len(patch.Ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(patch.Ops))
	}
	if patch.Ops[0].BeforeLine != 5 {
		t.Errorf("expected BeforeLine=5 (first child), got %d", patch.Ops[0].BeforeLine)
	}
	indentSpace := strings.Repeat("    ", 2) // Indent+1 = 2 levels
	if !strings.Contains(patch.Ops[0].Lines[0], indentSpace+"WAIT 1") {
		t.Errorf("expected WAIT at indent level 2, got %q", patch.Ops[0].Lines[0])
	}
}
