package main

import (
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

// TestPreviewTextOutput: a preview rule's finding is tagged [PREVIEW] in the
// text report and counted as not gated on the summary, baseline-drift,
// new-since and policy lines; it does not fail any of those gates. In-process:
// the meta flip does not reach a spawned binary.
func TestPreviewTextOutput(t *testing.T) {
	defer flipToPreview(t, "hardcoded-credential")()
	doc := parseHelper(t, cliTestFlow)
	rules, _ := selectRules("hardcoded-credential", "", nil)
	report := analyzer.RunAnalysis(doc, rules, buildSettingsFromConfig(nil), nil)
	if len(report.Findings) != 1 || report.Findings[0].Severity != models.SeverityError {
		t.Fatalf("want one credential error, got %+v", report.Findings)
	}
	const note = " (1 preview finding not gated)\n"

	out := captureStdoutFile(t, func() { printText(report, false) })
	if !strings.Contains(out, "info: 0"+note) || !strings.Contains(out, "[ERROR][PREVIEW] ") {
		t.Errorf("text report:\n%s", out)
	}

	runs := []flowRun{{target: "flow.txt", doc: doc, report: report}}
	for name, g := range map[string]analyzeGate{
		"baseline drift: 1 new (1 errors, 0 warnings, 0 info) out of 1 total": {baselinePath: "bl.json", baseline: baselineFile{Found: true}},
		"new since HEAD: 1 (1 errors, 0 warnings, 0 info) out of 1 total":     {newFromRev: "HEAD"},
	} {
		var failed bool
		out := captureStdoutFile(t, func() { failed = gateFailed(runs, textFormat, "error", g) })
		if failed || !strings.Contains(out, name+note) {
			t.Errorf("failed=%v, want the line %q:\n%s", failed, name+note, out)
		}
	}

	res := analyzer.EvaluatePolicy(report, models.Policy{Name: "ci", GateSeverity: models.SeverityInfo,
		Rules: []models.PolicyRule{{RuleID: "hardcoded-credential", Enabled: true}}})
	var buf strings.Builder
	printPolicyResult(&buf, res, false)
	if !res.Passed || !strings.Contains(buf.String(), `policy "ci": PASS`) || !strings.Contains(buf.String(), "preview (not gated): 1\n") {
		t.Errorf("policy line: %s", buf.String())
	}
}
