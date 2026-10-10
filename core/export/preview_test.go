package export

import (
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

// withPreviewRule flips the built-in id to the preview stage for the test.
func withPreviewRule(t *testing.T, id string) {
	t.Helper()
	m, ok := analyzer.LookupMeta(id)
	if !ok {
		t.Fatalf("%s is not a built-in", id)
	}
	m.Stage = analyzer.StagePreview
	t.Cleanup(analyzer.SetRuleMetaForTesting(id, m))
}

// previewReport mixes a stable warning and a stamped preview error of one
// rule with an unstamped error of a rule flipped to preview.
func previewReport() *models.AnalysisReport {
	return &models.AnalysisReport{Findings: []models.Finding{
		{RuleID: "r-x", Title: "stable", Severity: models.SeverityWarning, BlockID: "b1", Fingerprint: "r-x:1"},
		{RuleID: "r-x", Title: "stamped", Severity: models.SeverityError, BlockID: "b2", Fingerprint: "r-x:2", Stage: "preview"},
		{RuleID: "wait-zero", Title: "unstamped", Description: "zero wait", Severity: models.SeverityError, BlockID: "b3", Fingerprint: "wait-zero:3"},
	}}
}

func TestSARIF_PreviewFindingsAreNotes(t *testing.T) {
	withPreviewRule(t, "wait-zero")
	report := previewReport()
	policy := &models.PolicyResult{
		PolicyID: "p", Passed: true, Preview: 2,
		// A violation list naming a preview finding (an outside verdict):
		// both properties are kept.
		Violations: []models.Finding{report.Findings[1]},
	}
	raw, err := ReportToSARIFWithOptions(report, nil, Options{Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	var log sarifLog
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	run := log.Runs[0]

	wantLevels := []string{"warning", "note", "note"}
	for i, res := range run.Results {
		if res.Level != wantLevels[i] {
			t.Errorf("result %d (%s): level %q, want %q", i, report.Findings[i].Title, res.Level, wantLevels[i])
		}
		preview := i > 0
		if got, _ := res.Properties["stage"].(string); (got == "preview") != preview || (!preview && res.Properties != nil) {
			t.Errorf("result %d: properties %v", i, res.Properties)
		}
	}
	if v, _ := run.Results[1].Properties["policyViolation"].(bool); !v {
		t.Errorf("the policyViolation property must survive the stage: %v", run.Results[1].Properties)
	}

	levels := map[string]string{}
	for _, r := range run.Tool.Driver.Rules {
		levels[r.ID] = r.DefaultConfiguration.Level
	}
	if levels["r-x"] != "warning" || levels["wait-zero"] != "note" {
		t.Errorf("rule levels must follow the effective result levels: %v", levels)
	}
	if got, _ := run.Properties["policy"].(map[string]any)["preview"].(float64); got != 2 {
		t.Errorf("run.properties.policy.preview = %v, want 2", run.Properties["policy"])
	}

	// Without a preview count the policy properties keep their shape.
	if _, ok := policyProperties(&models.PolicyResult{PolicyID: "p"})["preview"]; ok {
		t.Error("a zero Preview must not appear in run.properties.policy")
	}
}

func TestJUnit_PreviewFindingsDoNotFail(t *testing.T) {
	withPreviewRule(t, "wait-zero")
	out, err := ReportToJUnitWithOptions(previewReport(), nil, Options{Policy: &models.PolicyResult{PolicyID: "p", Passed: true, Preview: 2}})
	if err != nil {
		t.Fatal(err)
	}
	var root junitRoot
	if err := xml.Unmarshal(out, &root); err != nil {
		t.Fatalf("invalid JUnit XML: %v", err)
	}
	suite := root.Suites[0]
	if suite.Tests != 3 || suite.Failures != 1 {
		t.Errorf("tests=%d failures=%d, want 3 and 1", suite.Tests, suite.Failures)
	}
	tcs := suite.Testcases
	if tcs[0].Failure == nil || tcs[0].SystemOut != nil {
		t.Error("the stable warning is a failure")
	}
	for _, tc := range tcs[1:] {
		if tc.Failure != nil {
			t.Errorf("%s: a preview finding must not be a failure", tc.Name)
		}
		if tc.SystemOut == nil || !strings.HasPrefix(tc.SystemOut.Content, junitPreviewNote) {
			t.Errorf("%s: <system-out> must say why it passed: %+v", tc.Name, tc.SystemOut)
		}
	}
	if !strings.Contains(tcs[2].SystemOut.Content, "zero wait") {
		t.Errorf("the preview body is kept: %q", tcs[2].SystemOut.Content)
	}
	found := false
	for _, p := range suite.Properties.Items {
		if p.Name == "policy.preview" {
			found = p.Value == "2"
		}
	}
	if !found {
		t.Errorf("policy.preview property missing: %+v", suite.Properties.Items)
	}

	out, err = ReportToJUnitWithOptions(&models.AnalysisReport{}, nil, Options{Policy: &models.PolicyResult{PolicyID: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "policy.preview") {
		t.Error("a zero Preview must not add a property")
	}
}

// TestJUnit_UnknownSeverityFails: a finding of an unknown severity (a stored
// report from before the engine normalised configured severities) fails, as
// in every gate; info passes.
func TestJUnit_UnknownSeverityFails(t *testing.T) {
	out, err := ReportToJUnitWithOptions(&models.AnalysisReport{Findings: []models.Finding{
		{RuleID: "r-x", Title: "critical", Severity: "critical", BlockID: "b1", Fingerprint: "r-x:1"},
		{RuleID: "r-x", Title: "info", Severity: models.SeverityInfo, BlockID: "b2", Fingerprint: "r-x:2"},
	}}, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var root junitRoot
	if err := xml.Unmarshal(out, &root); err != nil {
		t.Fatalf("invalid JUnit XML: %v", err)
	}
	if s := root.Suites[0]; s.Failures != 1 || s.Testcases[0].Failure == nil || s.Testcases[1].Failure != nil {
		t.Errorf("failures=%d: want only the unknown severity to fail", s.Failures)
	}
}
