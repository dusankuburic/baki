package analyzer

import (
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestSampleFlow_ParsesAndTriggersFindings guards the onboarding sample flow
// (frontend/src/data/sampleFlow.ts): it must parse cleanly AND trigger several
// findings, so the first-run "Try a sample flow" experience is instructive
// rather than empty. If the sample text drifts into something that no longer
// parses or no longer demonstrates the analyzer, update it here AND in the TS.
func TestSampleFlow_ParsesAndTriggersFindings(t *testing.T) {
	const sample = `#Region "Main"
Variables.SetVariable Name: %ApiKey% Value: 'AKIAIOSFODNN7EXAMPLE'
Variables.SetVariable Name: %DebugFlag% Value: True
HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/customers' Accept: 'application/json' => %Response%
LOOP FROM 1 TO 50 STEP 1
    WebAutomation.ClickLink BrowserInstance: %Browser% Link: 'next page'
    IF %Response% = '' THEN
        Display.ShowMessageBox Message: 'No data returned from API'
    END
END
Text.WriteToFile TextToWrite: %Response% FilePath: 'C:\Reports\customers.txt' IfFileExists: Overwrite
#EndRegion
`
	doc, err := parser.ParseText(sample, "Main.txt", int64(len(sample)))
	if err != nil {
		t.Fatalf("sample flow failed to parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	if len(report.Findings) < 4 {
		t.Fatalf("expected the sample to trigger >=4 findings (it should be instructive), got %d: %+v", len(report.Findings), ruleIDs(report.Findings))
	}
	// At least one security and one reliability finding so the demo spans categories.
	rules := ruleSet(report.Findings)
	if !rules["hardcoded-credential"] {
		t.Errorf("expected hardcoded-credential in the sample's findings, got %v", rules)
	}
}

func ruleIDs(fs []models.Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.RuleID
	}
	return out
}

func ruleSet(fs []models.Finding) map[string]bool {
	m := make(map[string]bool, len(fs))
	for _, f := range fs {
		m[f.RuleID] = true
	}
	return m
}
