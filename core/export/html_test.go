package export

import (
	"fmt"
	"strings"
	"testing"

	"pad-core/models"
)

func TestReportToHTML_NoFindings(t *testing.T) {
	report := makeReport()
	doc := makeDoc("MyFlow", "")
	out := ReportToHTML(report, doc)

	if !strings.Contains(out, "<!DOCTYPE html>") {
		t.Error("expected doctype")
	}
	if !strings.Contains(out, "<title>PAD Analyzer — Analysis Report</title>") {
		t.Error("expected title")
	}
	if !strings.Contains(out, "MyFlow") {
		t.Error("expected flow name")
	}
	if !strings.Contains(out, "No findings detected") {
		t.Errorf("expected no-findings message, got:\n%s", out)
	}
	if !strings.HasSuffix(out, "</html>\n") {
		t.Error("expected closing </html>")
	}
}

func TestReportToHTML_WithFilePath(t *testing.T) {
	report := makeReport()
	doc := makeDoc("MyFlow", `C:\flows\main.txt`)
	out := ReportToHTML(report, doc)

	if !strings.Contains(out, `C:\flows\main.txt`) {
		t.Errorf("expected file path in output, got:\n%s", out)
	}
}

func TestReportToHTML_SeverityGrouping(t *testing.T) {
	err := models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "Bad error", BlockID: "b1"}
	warn := models.Finding{RuleID: "r2", Severity: models.SeverityWarning, Title: "Some warning", BlockID: "b2"}
	info := models.Finding{RuleID: "r3", Severity: models.SeverityInfo, Title: "FYI", BlockID: "b3"}

	report := makeReport(err, warn, info)
	doc := makeDoc("Flow", "")
	out := ReportToHTML(report, doc)

	// All three severity sections must appear in order: Errors, Warnings, Info
	errIdx := strings.Index(out, ">Errors <span")
	warnIdx := strings.Index(out, ">Warnings <span")
	infoIdx := strings.Index(out, ">Info <span")

	if errIdx == -1 || warnIdx == -1 || infoIdx == -1 {
		t.Fatalf("missing severity sections: errors=%d warnings=%d info=%d", errIdx, warnIdx, infoIdx)
	}
	if errIdx >= warnIdx || warnIdx >= infoIdx {
		t.Errorf("severity sections out of order: errors=%d warnings=%d info=%d", errIdx, warnIdx, infoIdx)
	}

	for _, title := range []string{"Bad error", "Some warning", "FYI"} {
		if !strings.Contains(out, title) {
			t.Errorf("expected finding title %q in output", title)
		}
	}
}

func TestReportToHTML_OnlyErrors(t *testing.T) {
	err := models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "Crash", BlockID: "b1"}
	report := makeReport(err)
	out := ReportToHTML(report, makeDoc("Flow", ""))

	if !strings.Contains(out, ">Errors <span") {
		t.Error("expected Errors section")
	}
	if strings.Contains(out, ">Warnings <span") || strings.Contains(out, ">Info <span") {
		t.Error("unexpected Warnings/Info sections when none exist")
	}
}

func TestReportToHTML_SuggestionIncluded(t *testing.T) {
	f := models.Finding{
		RuleID:     "r1",
		Severity:   models.SeverityWarning,
		Title:      "Use vault",
		BlockID:    "b1",
		Suggestion: "Move to key vault.",
	}
	out := ReportToHTML(makeReport(f), makeDoc("Flow", ""))

	if !strings.Contains(out, "Move to key vault.") {
		t.Errorf("expected suggestion text in output:\n%s", out)
	}
	if !strings.Contains(out, "<strong>Suggestion:</strong>") {
		t.Error("expected Suggestion label")
	}
}

func TestReportToHTML_NoSuggestionWhenEmpty(t *testing.T) {
	f := models.Finding{RuleID: "r1", Severity: models.SeverityInfo, Title: "Note", BlockID: "b1", Suggestion: ""}
	out := ReportToHTML(makeReport(f), makeDoc("Flow", ""))

	if strings.Contains(out, "Suggestion:") {
		t.Error("should not include empty suggestion block")
	}
}

func TestReportToHTML_SummaryCards(t *testing.T) {
	f := models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "E", BlockID: "b1"}
	out := ReportToHTML(makeReport(f), makeDoc("Flow", ""))

	for _, label := range []string{"Blocks analyzed", "Rules run", "Errors", "Warnings", "Info"} {
		if !strings.Contains(out, label) {
			t.Errorf("expected %q in summary cards", label)
		}
	}
}

func TestReportToHTML_EscapesFindingContent(t *testing.T) {
	f := models.Finding{
		RuleID:      "<script>x</script>",
		Severity:    models.SeverityError,
		Title:       `Evil <img src=x onerror="alert(1)">`,
		Description: "Desc <b>bold</b> & 'quotes'",
		BlockID:     "b1",
		Suggestion:  "Fix <iframe src=evil></iframe>",
	}
	out := ReportToHTML(makeReport(f), makeDoc("Flow", ""))

	for _, raw := range []string{"<script>", "<img", "<iframe", "<b>bold</b>", `onerror="alert`} {
		if strings.Contains(out, raw) {
			t.Errorf("unescaped content %q leaked into HTML output", raw)
		}
	}
	for _, want := range []string{"&lt;script&gt;", "&lt;img", "&lt;iframe", "&lt;b&gt;bold&lt;/b&gt;"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected escaped content %q in output", want)
		}
	}
	// The embedded CSS/structure legitimately contains tags; only finding
	// content must be escaped. Sanity-check the doc is still valid overall.
	if !strings.Contains(out, "<style>") {
		t.Error("expected inline <style> block")
	}
}

func TestReportToHTML_NilDocAndReport(t *testing.T) {
	// Both nil — must not panic.
	out := ReportToHTML(nil, nil)
	if !strings.Contains(out, "PAD Analyzer") {
		t.Errorf("expected header in output, got: %s", out)
	}

	// Nil doc, real report — must not panic, must include duration.
	rep := &models.AnalysisReport{
		GeneratedAt: makeReport().GeneratedAt,
		DurationMs:  42,
		Findings:    []models.Finding{{RuleID: "r1", Severity: models.SeverityError, Title: "T", Description: "D", BlockID: "b"}},
	}
	out = ReportToHTML(rep, nil)
	if !strings.Contains(out, "42ms") {
		t.Errorf("expected duration in output, got: %s", out)
	}
}

func TestReportToHTML_HealthScore(t *testing.T) {
	cases := []struct {
		score     int
		wantClass string
	}{
		{85, "good"},
		{60, "warn"},
		{30, "bad"},
	}
	for _, tc := range cases {
		report := makeReport()
		report.Metrics = &models.FlowMetrics{HealthScore: tc.score}
		out := ReportToHTML(report, makeDoc("Flow", ""))
		if !strings.Contains(out, fmt.Sprintf(`<div class="health %s">Health Score: %d/100</div>`, tc.wantClass, tc.score)) {
			t.Errorf("score %d: expected health badge class %q in output", tc.score, tc.wantClass)
		}
	}

	// No metrics → no health badge at all.
	out := ReportToHTML(makeReport(), makeDoc("Flow", ""))
	if strings.Contains(out, "Health Score") {
		t.Error("unexpected health badge when Metrics is nil")
	}
}

func TestReportToHTML_Category(t *testing.T) {
	f := models.Finding{RuleID: "r1", Severity: models.SeverityWarning, Title: "T", BlockID: "b1", Category: "Reliability"}
	out := ReportToHTML(makeReport(f), makeDoc("Flow", ""))
	if !strings.Contains(out, "Category: Reliability") {
		t.Error("expected category in finding props")
	}

	// Empty category → no Category prop.
	f.Category = ""
	out = ReportToHTML(makeReport(f), makeDoc("Flow", ""))
	if strings.Contains(out, "Category:") {
		t.Error("unexpected category prop when empty")
	}
}

func TestReportToHTML_FlowNameFallbackWithoutDoc(t *testing.T) {
	report := makeReport()
	report.FlowName = "Named Flow"
	out := ReportToHTML(report, nil)
	if !strings.Contains(out, "Flow: Named Flow") {
		t.Error("expected report.FlowName fallback when doc is nil")
	}
}

func TestSeverityClass(t *testing.T) {
	cases := []struct {
		sev  models.Severity
		want string
	}{
		{models.SeverityError, "err"},
		{models.SeverityWarning, "warn"},
		{models.SeverityInfo, "info"},
		{"custom", "info"},
	}
	for _, tc := range cases {
		if got := severityClass(tc.sev); got != tc.want {
			t.Errorf("severityClass(%q) = %q, want %q", tc.sev, got, tc.want)
		}
	}
}
