package export

import (
	"encoding/csv"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"pad-core/models"
)

// ── JUnit ──────────────────────────────────────────────────────────

func TestReportToJUnit_BasicStructure(t *testing.T) {
	report := &models.AnalysisReport{
		FlowName:    "TestFlow",
		GeneratedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		DurationMs:  42,
		Findings: []models.Finding{
			{RuleID: "unhandled-error", Severity: models.SeverityError, Title: "Unhandled error", Description: "desc", Suggestion: "fix it"},
			{RuleID: "missing-timeout", Severity: models.SeverityWarning, Title: "Missing timeout", Description: "desc2"},
			{RuleID: "hardcoded-url", Severity: models.SeverityInfo, Title: "Hardcoded URL", Description: "desc3"},
		},
	}
	doc := &models.FlowDocument{Name: "TestFlow"}

	out, err := ReportToJUnit(report, doc)
	if err != nil {
		t.Fatalf("ReportToJUnit: %v", err)
	}

	var root junitRoot
	if err := xml.Unmarshal(out, &root); err != nil {
		t.Fatalf("invalid JUnit XML: %v\nraw:\n%s", err, out)
	}
	if len(root.Suites) != 1 {
		t.Fatalf("expected 1 suite, got %d", len(root.Suites))
	}
	suite := root.Suites[0]
	if suite.Name != "TestFlow" {
		t.Errorf("suite name = %q, want TestFlow", suite.Name)
	}
	if suite.Tests != 3 {
		t.Errorf("tests = %d, want 3", suite.Tests)
	}
	if suite.Failures != 2 {
		t.Errorf("failures = %d, want 2 (error+warning)", suite.Failures)
	}
	// Error and warning → failure element; info → no failure.
	withFailure := 0
	for _, tc := range suite.Testcases {
		if tc.Failure != nil {
			withFailure++
		}
	}
	if withFailure != 2 {
		t.Errorf("testcases with failure = %d, want 2", withFailure)
	}
}

func TestReportToJUnit_NilDocNoPanic(t *testing.T) {
	report := &models.AnalysisReport{Findings: []models.Finding{}}
	out, err := ReportToJUnit(report, nil)
	if err != nil {
		t.Fatalf("ReportToJUnit(nil doc): %v", err)
	}
	if !strings.Contains(string(out), "<testsuites") {
		t.Errorf("expected XML output, got: %s", out)
	}
}

func TestReportToJUnit_NilReportNoPanic(t *testing.T) {
	out, err := ReportToJUnit(nil, nil)
	if err != nil {
		t.Fatalf("ReportToJUnit(nil, nil): %v", err)
	}
	if !strings.Contains(string(out), "<testsuites") {
		t.Errorf("expected XML output, got: %s", out)
	}
}

// ── CSV ────────────────────────────────────────────────────────────

func TestReportToCSV_BasicStructure(t *testing.T) {
	report := &models.AnalysisReport{
		Findings: []models.Finding{
			{RuleID: "unhandled-error", Severity: models.SeverityError, Title: "Test", Description: "a,comma", Suggestion: "fix"},
		},
	}
	doc := &models.FlowDocument{Name: "Flow1", FilePath: "/tmp/f.txt"}

	out, err := ReportToCSV(report, doc)
	if err != nil {
		t.Fatalf("ReportToCSV: %v", err)
	}

	r := csv.NewReader(strings.NewReader(string(out)))
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (header + 1 finding), got %d", len(rows))
	}
	if rows[0][0] != "Rule" {
		t.Errorf("header[0] = %q, want Rule", rows[0][0])
	}
	if rows[1][0] != "unhandled-error" {
		t.Errorf("row[1][0] = %q, want unhandled-error", rows[1][0])
	}
	// Verify comma in description was quoted properly (CSV reader handles it)
	if rows[1][3] != "a,comma" {
		t.Errorf("description = %q, want 'a,comma'", rows[1][3])
	}
}

func TestReportToCSV_EmptyReport(t *testing.T) {
	report := &models.AnalysisReport{Findings: []models.Finding{}}
	out, err := ReportToCSV(report, nil)
	if err != nil {
		t.Fatalf("ReportToCSV: %v", err)
	}
	r := csv.NewReader(strings.NewReader(string(out)))
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (header only), got %d", len(rows))
	}
}

func TestReportToCSV_NewlineSanitized(t *testing.T) {
	report := &models.AnalysisReport{
		Findings: []models.Finding{
			{RuleID: "r1", Severity: models.SeverityError, Title: "T", Description: "line1\nline2"},
		},
	}
	out, err := ReportToCSV(report, nil)
	if err != nil {
		t.Fatalf("ReportToCSV: %v", err)
	}
	r := csv.NewReader(strings.NewReader(string(out)))
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV: %v", err)
	}
	if !strings.Contains(rows[1][3], "line1") {
		t.Errorf("description lost its text: %q", rows[1][3])
	}
	if strings.Contains(rows[1][3], "\n") {
		t.Errorf("description still contains newline: %q", rows[1][3])
	}
}
