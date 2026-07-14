package export

import (
	"strings"
	"testing"
	"time"

	"pad-core/models"
)

func makeReport(findings ...models.Finding) *models.AnalysisReport {
	stats := models.AnalysisStats{BlocksAnalyzed: 10, RulesRun: 3}
	for _, f := range findings {
		switch f.Severity {
		case models.SeverityError:
			stats.Errors++
		case models.SeverityWarning:
			stats.Warnings++
		case models.SeverityInfo:
			stats.Info++
		}
	}
	return &models.AnalysisReport{
		FlowID:      "test-flow",
		GeneratedAt: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
		Findings:    findings,
		Stats:       stats,
		DurationMs:  42,
	}
}

func makeDoc(name, filePath string) *models.FlowDocument {
	return &models.FlowDocument{Name: name, FilePath: filePath}
}

func TestReportToMarkdown_NoFindings(t *testing.T) {
	report := makeReport()
	doc := makeDoc("MyFlow", "")
	out := ReportToMarkdown(report, doc)

	if !strings.Contains(out, "# PAD Analyzer") {
		t.Error("expected header")
	}
	if !strings.Contains(out, "MyFlow") {
		t.Error("expected flow name")
	}
	if !strings.Contains(out, "No findings detected") {
		t.Errorf("expected no-findings message, got:\n%s", out)
	}
	// Should not have severity sections when no findings
	if strings.Contains(out, "## Errors") || strings.Contains(out, "## Warnings") {
		t.Error("unexpected severity sections for empty report")
	}
}

func TestReportToMarkdown_WithFilePath(t *testing.T) {
	report := makeReport()
	doc := makeDoc("MyFlow", `C:\flows\main.txt`)
	out := ReportToMarkdown(report, doc)

	if !strings.Contains(out, `C:\flows\main.txt`) {
		t.Errorf("expected file path in output, got:\n%s", out)
	}
}

func TestReportToMarkdown_SeverityGrouping(t *testing.T) {
	err := models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "Bad error", BlockID: "b1"}
	warn := models.Finding{RuleID: "r2", Severity: models.SeverityWarning, Title: "Some warning", BlockID: "b2"}
	info := models.Finding{RuleID: "r3", Severity: models.SeverityInfo, Title: "FYI", BlockID: "b3"}

	report := makeReport(err, warn, info)
	doc := makeDoc("Flow", "")
	out := ReportToMarkdown(report, doc)

	// All three severity sections must appear in order: Errors, Warnings, Info
	errIdx := strings.Index(out, "## Errors")
	warnIdx := strings.Index(out, "## Warnings")
	infoIdx := strings.Index(out, "## Info")

	if errIdx == -1 || warnIdx == -1 || infoIdx == -1 {
		t.Fatalf("missing severity sections: errors=%d warnings=%d info=%d", errIdx, warnIdx, infoIdx)
	}
	if errIdx >= warnIdx || warnIdx >= infoIdx {
		t.Errorf("severity sections out of order: errors=%d warnings=%d info=%d", errIdx, warnIdx, infoIdx)
	}

	// Each finding title must appear
	for _, title := range []string{"Bad error", "Some warning", "FYI"} {
		if !strings.Contains(out, title) {
			t.Errorf("expected finding title %q in output", title)
		}
	}
}

func TestReportToMarkdown_OnlyErrors(t *testing.T) {
	err := models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "Crash", BlockID: "b1"}
	report := makeReport(err)
	doc := makeDoc("Flow", "")
	out := ReportToMarkdown(report, doc)

	if !strings.Contains(out, "## Errors") {
		t.Error("expected Errors section")
	}
	if strings.Contains(out, "## Warnings") || strings.Contains(out, "## Info") {
		t.Error("unexpected Warnings/Info sections when none exist")
	}
}

func TestReportToMarkdown_SuggestionIncluded(t *testing.T) {
	f := models.Finding{
		RuleID:     "r1",
		Severity:   models.SeverityWarning,
		Title:      "Use vault",
		BlockID:    "b1",
		Suggestion: "Move to key vault.",
	}
	out := ReportToMarkdown(makeReport(f), makeDoc("Flow", ""))

	if !strings.Contains(out, "Move to key vault.") {
		t.Errorf("expected suggestion text in output:\n%s", out)
	}
	if !strings.Contains(out, "Suggestion") {
		t.Error("expected Suggestion label")
	}
}

func TestReportToMarkdown_NoSuggestionWhenEmpty(t *testing.T) {
	f := models.Finding{RuleID: "r1", Severity: models.SeverityInfo, Title: "Note", BlockID: "b1", Suggestion: ""}
	out := ReportToMarkdown(makeReport(f), makeDoc("Flow", ""))

	// The blockquote suggestion line should not appear
	if strings.Contains(out, "> **Suggestion:**") {
		t.Error("should not include empty suggestion blockquote")
	}
}

func TestReportToMarkdown_StatsTable(t *testing.T) {
	f := models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "E", BlockID: "b1"}
	out := ReportToMarkdown(makeReport(f), makeDoc("Flow", ""))

	if !strings.Contains(out, "Blocks analyzed") {
		t.Error("expected 'Blocks analyzed' in stats table")
	}
	if !strings.Contains(out, "Rules run") {
		t.Error("expected 'Rules run' in stats table")
	}
}

func TestFilterBySeverity(t *testing.T) {
	findings := []models.Finding{
		{Severity: models.SeverityError},
		{Severity: models.SeverityWarning},
		{Severity: models.SeverityError},
		{Severity: models.SeverityInfo},
	}
	errors := filterBySeverity(findings, models.SeverityError)
	if len(errors) != 2 {
		t.Errorf("expected 2 errors, got %d", len(errors))
	}
	warnings := filterBySeverity(findings, models.SeverityWarning)
	if len(warnings) != 1 {
		t.Errorf("expected 1 warning, got %d", len(warnings))
	}
	infos := filterBySeverity(findings, models.SeverityInfo)
	if len(infos) != 1 {
		t.Errorf("expected 1 info, got %d", len(infos))
	}
}

func TestSeverityTitle(t *testing.T) {
	cases := []struct {
		sev  models.Severity
		want string
	}{
		{models.SeverityError, "Errors"},
		{models.SeverityWarning, "Warnings"},
		{models.SeverityInfo, "Info"},
		{"custom", "custom"},
	}
	for _, tc := range cases {
		if got := severityTitle(tc.sev); got != tc.want {
			t.Errorf("severityTitle(%q) = %q, want %q", tc.sev, got, tc.want)
		}
	}
}

// TestReportToMarkdown_NilDocAndReport confirms the markdown exporter doesn't
// panic on nil doc/report (mirrors the SARIF guard; previously it dereferenced
// doc.Name and report.GeneratedAt unconditionally).
func TestReportToMarkdown_NilDocAndReport(t *testing.T) {
	// Both nil — must not panic.
	out := ReportToMarkdown(nil, nil)
	if !strings.Contains(out, "PAD Analyzer") {
		t.Errorf("expected header in output, got: %s", out)
	}

	// Nil doc, real report — must not panic, must include generated-at.
	rep := &models.AnalysisReport{
		GeneratedAt: time.Now(),
		DurationMs:  42,
		Findings:    []models.Finding{{RuleID: "r1", Severity: models.SeverityError, Title: "T", Description: "D"}},
	}
	out = ReportToMarkdown(rep, nil)
	if !strings.Contains(out, "42ms") {
		t.Errorf("expected duration in output, got: %s", out)
	}
}
