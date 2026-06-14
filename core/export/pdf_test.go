package export

import (
	"bytes"
	"testing"
	"time"

	"pad-core/models"
)

func TestReportToPDF_NoFindings_NoPanic(t *testing.T) {
	report := &models.AnalysisReport{
		FlowID:      "smoke-flow",
		GeneratedAt: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC),
		Findings:    nil,
		Stats:       models.AnalysisStats{BlocksAnalyzed: 5, RulesRun: 3},
		DurationMs:  10,
	}
	doc := &models.FlowDocument{Name: "SmokeFlow"}

	data, err := ReportToPDF(report, doc)
	if err != nil {
		t.Fatalf("ReportToPDF: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty PDF bytes")
	}
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		t.Errorf("output does not start with %%PDF magic: got %.10q", data)
	}
}

func TestReportToPDF_WithFindings_NoPanic(t *testing.T) {
	report := makeReport(
		models.Finding{RuleID: "r1", Severity: models.SeverityError, Title: "Bad thing", BlockID: "b1", Description: "Something went wrong."},
		models.Finding{RuleID: "r2", Severity: models.SeverityWarning, Title: "Watch out", BlockID: "b2", Description: "Might be a problem.", Suggestion: "Use a vault."},
		models.Finding{RuleID: "r3", Severity: models.SeverityInfo, Title: "FYI", BlockID: "b3"},
	)
	doc := makeDoc("TestFlow", `C:\flows\main.txt`)

	data, err := ReportToPDF(report, doc)
	if err != nil {
		t.Fatalf("ReportToPDF with findings: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty PDF bytes")
	}
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		t.Errorf("output does not start with %%PDF magic: got %.10q", data)
	}
}

func TestReportToPDF_ManyFindings_NoPanic(t *testing.T) {
	// Enough findings to trigger a page break (GetY > 260 path).
	findings := make([]models.Finding, 40)
	for i := range findings {
		findings[i] = models.Finding{
			RuleID:      "r1",
			Severity:    models.SeverityWarning,
			Title:       "Repeated warning",
			BlockID:     "b1",
			Description: "This is a finding with a moderately long description to consume vertical space in the PDF layout engine.",
			Suggestion:  "Fix it by doing the right thing.",
		}
	}
	report := makeReport(findings...)
	doc := makeDoc("BigFlow", "")

	if _, err := ReportToPDF(report, doc); err != nil {
		t.Fatalf("ReportToPDF with many findings: %v", err)
	}
}
