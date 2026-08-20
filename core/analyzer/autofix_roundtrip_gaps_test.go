package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestInsertDefaultPatch_RoundTripResolvesFinding is the repo-convention gate
// (parse → analyze → fix → re-parse → re-analyze) for the insert-default
// fixer: inserting a DEFAULT branch into a SWITCH that lacks one must
// (a) re-parse cleanly with the DEFAULT nested under the SWITCH, and
// (b) resolve the switch-no-default finding.
func TestInsertDefaultPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"SWITCH %Mode%\n" +
		"    CASE 'fast'\n" +
		"        Display.ShowMessage Message: 'fast'\n" +
		"    END\n" +
		"END\n" +
		"WAIT 1\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var finding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "switch-no-default" {
			finding = &report.Findings[i]
			break
		}
	}
	if finding == nil {
		t.Fatalf("expected a switch-no-default finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[finding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", finding.BlockID)
	}

	patched := ApplyPatch(source, InsertDefaultPatch(block))

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after insert-default failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	// Faithfulness detail: the DEFAULT must be NESTED under the SWITCH
	// (a DEFAULT at top level would parse as "DEFAULT outside SWITCH").
	found := false
	for _, root := range doc2.Subflows[0].Blocks {
		if root.RawType != "SWITCH" {
			continue
		}
		for i := range root.Children {
			if root.Children[i].RawType == "DEFAULT" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("DEFAULT not nested under the SWITCH after patch:\n%s", patched)
	}

	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "switch-no-default" {
			t.Errorf("switch-no-default still present after fix: %+v\npatched:\n%s", f, patched)
		}
	}
}

// TestAppendOutputPatch_RoundTripResolvesFinding is the repo-convention gate
// for append-output. The uncaptured-output finding only fires when the target
// subflow has declared output variables, and the parser never populates
// Subflow.Variables from PAD text (they come from cloud-stored metadata) — so
// this gate parses a real two-subflow source for the CALL line, then declares
// the output variable on the parsed target subflow, exactly as a cloud doc
// would carry it. The fix must re-parse cleanly (the CALL line gains
// ` => %Output_Result%`) and resolve the finding.
func TestAppendOutputPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"CALL Helper\n" +
		"#EndRegion\n" +
		"#Region \"Helper\"\n" +
		"SET Result TO 'done'\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Declare an output variable on the target subflow (cloud metadata shape).
	for i := range doc.Subflows {
		if doc.Subflows[i].Name == "Helper" {
			doc.Subflows[i].Variables = []models.VariableDecl{{Name: "Output_Result", Scope: "output"}}
		}
	}
	doc.RebuildIndexes()

	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var finding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "subflow-mismatch" && strings.Contains(report.Findings[i].Title, "output not captured") {
			finding = &report.Findings[i]
			break
		}
	}
	if finding == nil {
		t.Fatalf("expected an uncaptured-output subflow-mismatch finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[finding.BlockID]
	if block == nil {
		t.Fatalf("finding block %s not in doc", finding.BlockID)
	}

	patched := ApplyPatch(source, AppendOutputPatch(block))
	if !strings.Contains(patched, "CALL Helper => Output_Result") {
		t.Fatalf("patched source missing output capture:\n%s", patched)
	}

	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse after append-output failed (not faithful): %v\npatched:\n%s", err, patched)
	}
	for i := range doc2.Subflows {
		if doc2.Subflows[i].Name == "Helper" {
			doc2.Subflows[i].Variables = []models.VariableDecl{{Name: "Output_Result", Scope: "output"}}
		}
	}
	doc2.RebuildIndexes()

	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "subflow-mismatch" && strings.Contains(f.Title, "output not captured") {
			t.Errorf("uncaptured-output still present after fix: %+v\npatched:\n%s", f, patched)
		}
	}
}
