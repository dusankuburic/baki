package analyzer

import (
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

func TestTaintedSinkRule_DetectsInputInSink(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"File.Write Content: %Input_Command% FilePath: 'out.txt'\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	found := false
	for _, f := range report.Findings {
		if f.RuleID == "tainted-sink" {
			found = true
			if f.Severity != models.SeverityError {
				t.Errorf("severity = %s, want error", f.Severity)
			}
		}
	}
	if !found {
		t.Errorf("expected tainted-sink finding for Input_ var in file sink, got: %v", ruleIDs(report.Findings))
	}
}

func TestTaintedSinkRule_NoFindingForSafeVar(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"File.Write Content: %SafeVar% FilePath: 'out.txt'\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report.Findings {
		if f.RuleID == "tainted-sink" {
			t.Errorf("tainted-sink should not fire for non-Input_ variable, got finding: %+v", f)
		}
	}
}

func TestTaintedSinkRule_NoFindingForNonSink(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"SET X TO %Input_Data%\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report.Findings {
		if f.RuleID == "tainted-sink" {
			t.Errorf("tainted-sink should not fire on a SET block (not a sink), got finding: %+v", f)
		}
	}
}
