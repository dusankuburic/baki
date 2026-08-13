package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestUpgradeToHttpsPatch_RoundTripResolvesFinding verifies the upgrade-to-https
// fixer resolves insecure-http-url. After replacing http:// with https://, the
// rule's HasPrefix("http://") check no longer matches.
func TestUpgradeToHttpsPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"HttpClient.Get URL: 'http://example.com/api'\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var finding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "insecure-http-url" {
			finding = &report.Findings[i]
			break
		}
	}
	if finding == nil {
		t.Fatalf("expected insecure-http-url finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[finding.BlockID]
	if block == nil {
		t.Fatalf("block not found")
	}

	patched := ApplyPatch(source, UpgradeToHttpsPatch(block))
	if strings.Contains(patched, "http://") {
		t.Fatalf("expected http:// gone from patched source, got:\n%s", patched)
	}

	// Re-parse + re-analyze: finding must be gone.
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "insecure-http-url" {
			t.Errorf("insecure-http-url still fires after fix:\n%s", patched)
		}
	}
}

// TestReplaceWithVariablePatch_HardcodedUICoordinates_RoundTrip verifies the
// replace-with-variable fixer resolves hardcoded-ui-coordinates. After
// replacing the literal coordinate with %input_x%, the value contains % so the
// rule's strings.Contains(v, "%") check skips it.
func TestReplaceWithVariablePatch_HardcodedUICoordinates_RoundTrip(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"UIAutomation.Click X: 100\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var finding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "hardcoded-ui-coordinates" {
			finding = &report.Findings[i]
			break
		}
	}
	if finding == nil {
		t.Fatalf("expected hardcoded-ui-coordinates finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[finding.BlockID]
	if block == nil {
		t.Fatalf("block not found")
	}
	propKey, _ := finding.Metadata["property"].(string)
	if propKey == "" {
		t.Fatalf("finding has no property metadata")
	}

	patched := ApplyPatch(source, ReplaceWithVariablePatch(block, propKey))

	// Re-parse + re-analyze: finding must be gone.
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "hardcoded-ui-coordinates" {
			t.Errorf("hardcoded-ui-coordinates still fires after fix:\n%s", patched)
		}
	}
}

// TestSanitizeCommandVarsPatch_RoundTripResolvesFinding verifies the
// sanitize-command-vars fixer resolves command-injection-risk. After stripping
// %VarName% from the command properties, strings.Contains(val, "%") is false.
func TestSanitizeCommandVarsPatch_RoundTripResolvesFinding(t *testing.T) {
	const source = "#Region \"Main\"\n" +
		"System.RunDOSCommand Command: 'ping %UserInput%'\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var finding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "command-injection-risk" {
			finding = &report.Findings[i]
			break
		}
	}
	if finding == nil {
		t.Fatalf("expected command-injection-risk finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[finding.BlockID]
	if block == nil {
		t.Fatalf("block not found")
	}

	patched := ApplyPatch(source, SanitizeCommandVarsPatch(block))

	// Re-parse + re-analyze: finding must be gone.
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "command-injection-risk" {
			t.Errorf("command-injection-risk still fires after fix:\n%s", patched)
		}
	}
}
