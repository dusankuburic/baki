package export

import (
	"encoding/json"
	"strings"
	"testing"

	"pad-core/models"
)

func policyResult(passed bool, violations ...models.Finding) *models.PolicyResult {
	return &models.PolicyResult{
		PolicyID: "pol-1", PolicyName: "Security baseline", Passed: passed,
		Violations: violations, Errors: len(violations), Evaluated: 3, Waived: 1,
	}
}

// TestSARIF_RuleDescriptorsCarryHelp: a built-in rule's descriptor names the
// rule, carries its catalog description as fullDescription, a help text and a
// helpUri — GitHub renders these on the alert; before, the alert had a title
// and nothing else to act on.
func TestSARIF_RuleDescriptorsCarryHelp(t *testing.T) {
	report := makeReport(models.Finding{
		RuleID: "hardcoded-credential", Severity: models.SeverityError, Title: "instance title",
		BlockID: "b1", SubflowID: "s1",
	})
	log, _ := parseSARIF(t, report, makeDoc("Flow", ""))
	d := log.Runs[0].Tool.Driver.Rules[0]
	if d.Name == "" || d.FullDescription == nil || d.FullDescription.Text == "" {
		t.Errorf("built-in rule descriptor lacks name/fullDescription: %+v", d)
	}
	if d.ShortDescription == nil || d.ShortDescription.Text == "instance title" {
		t.Errorf("shortDescription should be the catalog rule name, got %+v", d.ShortDescription)
	}
	if d.Help == nil || !strings.Contains(d.Help.Text, "bakicli rules hardcoded-credential") {
		t.Errorf("help text missing: %+v", d.Help)
	}
	if d.HelpURI != RuleHelpURL {
		t.Errorf("helpUri = %q, want %q", d.HelpURI, RuleHelpURL)
	}
	if d.Properties["confidence"] == nil {
		t.Errorf("expected confidence property, got %+v", d.Properties)
	}

	// A custom rule (not in the catalog) falls back to the finding's own text.
	custom := makeReport(models.Finding{
		RuleID: "org-custom", Severity: models.SeverityWarning, Title: "Custom title",
		Description: "custom body", BlockID: "b1", SubflowID: "s1",
	})
	log, _ = parseSARIF(t, custom, makeDoc("Flow", ""))
	d = log.Runs[0].Tool.Driver.Rules[0]
	if d.ShortDescription == nil || d.ShortDescription.Text != "Custom title" {
		t.Errorf("custom rule shortDescription = %+v, want the finding title", d.ShortDescription)
	}
	if d.Help == nil || !strings.HasPrefix(d.Help.Text, "custom body") {
		t.Errorf("custom rule help should fall back to the finding description, got %+v", d.Help)
	}
}

// TestSARIF_AutomationDetailsAndInvocation pins the run-level envelope every
// upload carries: a stable automationDetails.id and an invocation that reports
// success when no policy gate failed.
func TestSARIF_AutomationDetailsAndInvocation(t *testing.T) {
	log, _ := parseSARIF(t, makeReport(), makeDoc("Flow", ""))
	run := log.Runs[0]
	if run.AutomationDetails == nil || run.AutomationDetails.ID != "pad-analysis" {
		t.Errorf("automationDetails = %+v, want id pad-analysis", run.AutomationDetails)
	}
	if len(run.Invocations) != 1 || !run.Invocations[0].ExecutionSuccessful {
		t.Errorf("expected one successful invocation, got %+v", run.Invocations)
	}
	if run.Properties != nil {
		t.Errorf("no policy → no run.properties, got %+v", run.Properties)
	}
	if run.OriginalURIBaseIDs != nil {
		t.Errorf("no source root → no originalUriBaseIds, got %+v", run.OriginalURIBaseIDs)
	}
}

// TestSARIF_PolicyVerdictEmbedded: with a policy, the verdict is in
// run.properties.policy, the invocation is unsuccessful on a failed gate, and
// the violating result is tagged — so an uploaded SARIF explains the red build.
func TestSARIF_PolicyVerdictEmbedded(t *testing.T) {
	bad := models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "boom", BlockID: "b1", SubflowID: "s1", Fingerprint: "fp-bad"}
	ok := models.Finding{RuleID: "r2", Severity: models.SeverityInfo, Title: "meh", BlockID: "b2", SubflowID: "s1", Fingerprint: "fp-ok"}
	report := makeReport(bad, ok)
	raw, err := ReportToSARIFWithOptions(report, makeDoc("Flow", ""), Options{Policy: policyResult(false, bad)})
	if err != nil {
		t.Fatal(err)
	}
	var log sarifLog
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	run := log.Runs[0]
	if run.Invocations[0].ExecutionSuccessful {
		t.Error("failed policy gate must set executionSuccessful=false")
	}
	pol, _ := run.Properties["policy"].(map[string]any)
	if pol == nil || pol["passed"] != false || pol["policyName"] != "Security baseline" || pol["violations"] != float64(1) {
		t.Errorf("run.properties.policy = %+v", run.Properties)
	}
	fps, _ := pol["violationFingerprints"].([]any)
	if len(fps) != 1 || fps[0] != fingerprint(bad) {
		t.Errorf("violationFingerprints = %+v, want the result's padFindingId", fps)
	}
	if run.Results[0].Properties["policyViolation"] != true {
		t.Errorf("violating result not tagged: %+v", run.Results[0].Properties)
	}
	if run.Results[1].Properties != nil {
		t.Errorf("non-violating result must not be tagged: %+v", run.Results[1].Properties)
	}

	// A passing policy keeps the invocation successful but still records the verdict.
	raw, _ = ReportToSARIFWithOptions(report, makeDoc("Flow", ""), Options{Policy: policyResult(true)})
	_ = json.Unmarshal(raw, &log)
	if !log.Runs[0].Invocations[0].ExecutionSuccessful || log.Runs[0].Properties["policy"] == nil {
		t.Errorf("passing policy: invocation/properties wrong: %+v", log.Runs[0])
	}
}

// TestSARIF_SourceRootMakesURIsRepoRelative: analysing ./flows from the repo
// root must yield "flows/Main.txt" against %SRCROOT% — the path GitHub needs to
// place an inline PR annotation. A bare "Main.txt" matched nothing.
func TestSARIF_SourceRootMakesURIsRepoRelative(t *testing.T) {
	doc := docWithBlock("Main", "teamA/Main.txt", "blk-1", 4)
	report := makeReport(models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "x", BlockID: "blk-1", SubflowID: "Main"})
	raw, err := ReportToSARIFWithOptions(report, doc, Options{SourceRoot: "flows", SourceRootURI: "file:///work/repo"})
	if err != nil {
		t.Fatal(err)
	}
	var log sarifLog
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	run := log.Runs[0]
	art := run.Results[0].Locations[0].PhysicalLocation.ArtifactLocation
	if art.URI != "flows/teamA/Main.txt" || art.URIBaseID != "%SRCROOT%" {
		t.Errorf("artifactLocation = %+v, want flows/teamA/Main.txt against %%SRCROOT%%", art)
	}
	if got := run.OriginalURIBaseIDs["%SRCROOT%"].URI; got != "file:///work/repo/" {
		t.Errorf("originalUriBaseIds = %q, want the root with a trailing slash", got)
	}

	// Root "." (analysing the checkout itself) adds no prefix.
	raw, _ = ReportToSARIFWithOptions(report, doc, Options{SourceRoot: ".", SourceRootURI: "file:///work/repo/"})
	_ = json.Unmarshal(raw, &log)
	if got := log.Runs[0].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI; got != "teamA/Main.txt" {
		t.Errorf("root '.' uri = %q, want teamA/Main.txt", got)
	}
}

// TestJUnit_PolicyProperties: the verdict lands in the standard <properties>
// block so Jenkins/GitLab show why the suite failed.
func TestJUnit_PolicyProperties(t *testing.T) {
	report := makeReport(models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "boom", BlockID: "b1"})
	out, err := ReportToJUnitWithOptions(report, nil, Options{Policy: policyResult(false, report.Findings[0])})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`<properties>`, `name="policy.name" value="Security baseline"`, `name="policy.passed" value="false"`, `name="policy.violations" value="1"`} {
		if !strings.Contains(s, want) {
			t.Errorf("JUnit output lacks %q:\n%s", want, s)
		}
	}
	plain, _ := ReportToJUnit(report, nil)
	if strings.Contains(string(plain), "<properties>") {
		t.Error("no policy → no <properties> block")
	}
}

// TestCSV_LineFingerprintCategoryAndSummary: the appended columns resolve the
// block's line from the document, and the leading comment carries the
// suppressed count a sheet of rows cannot show.
func TestCSV_LineFingerprintCategoryAndSummary(t *testing.T) {
	doc := docWithBlock("Main", "Main.txt", "blk-1", 42)
	report := makeReport(models.Finding{
		RuleID: "r1", Severity: models.SeverityError, Title: "x", BlockID: "blk-1", SubflowID: "Main",
		Fingerprint: "fp-1", Category: "Security",
	})
	report.Stats.Suppressed = 2
	out, err := ReportToCSV(report, doc)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(string(out), "\n", 2)[0]
	if !strings.HasPrefix(first, "# PAD Analyzer:") || !strings.Contains(first, "2 suppressed inline") {
		t.Errorf("summary comment = %q", first)
	}
	rows, err := readCSVRows(out)
	if err != nil {
		t.Fatal(err)
	}
	h := rows[0]
	if h[9] != "Line" || h[10] != "Fingerprint" || h[11] != "Category" {
		t.Errorf("header tail = %v", h[9:])
	}
	if r := rows[1]; r[9] != "42" || r[10] != "fp-1" || r[11] != "Security" {
		t.Errorf("row tail = %v, want [42 fp-1 Security]", r[9:])
	}
}

// TestHTML_LineNumbersAndSuppressedCard pins the per-finding line and the
// suppressed-inline stat card.
func TestHTML_LineNumbersAndSuppressedCard(t *testing.T) {
	doc := docWithBlock("Main", "sub/Main.txt", "blk-1", 7)
	doc.IsFolder = true
	report := makeReport(models.Finding{RuleID: "r1", Severity: models.SeverityWarning, Title: "x", BlockID: "blk-1", SubflowID: "Main"})
	report.Stats.Suppressed = 3
	out := ReportToHTML(report, doc)
	for _, want := range []string{"<span>Line: 7</span>", "File: <code>sub/Main.txt</code>", `<div class="num">3</div><div class="lbl">Suppressed inline</div>`} {
		if !strings.Contains(out, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	doc.IsFolder = false
	if strings.Contains(ReportToHTML(report, doc), "File: <code>") {
		t.Error("single-file doc must not repeat the file per finding")
	}
}
