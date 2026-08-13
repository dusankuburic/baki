package export

import (
	"encoding/csv"
	"strings"
	"testing"

	"pad-core/models"
)

// TestReportToCSV_FormulaInjectionNeutralized verifies CSV/formula injection is
// defused: user-controlled content that begins with a formula trigger (= + - @)
// is prefixed with a single quote so a spreadsheet treats the cell as text, not
// an executable formula. The flow name and a finding description are the vectors
// exercised here.
func TestReportToCSV_FormulaInjectionNeutralized(t *testing.T) {
	report := &models.AnalysisReport{
		Findings: []models.Finding{
			{
				RuleID:      "tainted-sink",
				Severity:    models.SeverityError,
				Title:       "T",
				Description: "@SUM(1+1)*cmd|' /C calc'!A0",
				Suggestion:  "+HYPERLINK(\"http://evil\")",
			},
		},
	}
	doc := &models.FlowDocument{Name: "=cmd|'/c calc'!A1", FilePath: "-startsWithDash"}

	out, err := ReportToCSV(report, doc)
	if err != nil {
		t.Fatalf("ReportToCSV: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(out))).ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected header + 1 row, got %d", len(rows))
	}
	row := rows[1]
	// Columns: Rule, Severity, Title, Description, Suggestion, Flow, File, Block, Subflow
	cases := map[string]string{
		"Description": row[3],
		"Suggestion":  row[4],
		"Flow":        row[5],
		"File":        row[6],
	}
	for name, cell := range cases {
		if !strings.HasPrefix(cell, "'") {
			t.Errorf("%s cell not guarded against formula injection: %q", name, cell)
		}
	}
	// A safe, analyzer-constant field (the rule ID) must be untouched.
	if row[0] != "tainted-sink" {
		t.Errorf("safe rule-ID cell should be unchanged, got %q", row[0])
	}
}

// TestGuardCSVFormula_LeavesSafeValues verifies the guard is a no-op for values
// that don't begin with a formula trigger (no spurious quoting of normal text).
func TestGuardCSVFormula_LeavesSafeValues(t *testing.T) {
	for _, s := range []string{"", "unhandled-error", "error", "Main.txt", "User input reaches sink"} {
		if got := guardCSVFormula(s); got != s {
			t.Errorf("guardCSVFormula(%q) = %q, want unchanged", s, got)
		}
	}
}
