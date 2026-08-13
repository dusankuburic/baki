package export

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"

	"pad-core/models"
)

// ReportToCSV serializes an analysis report as CSV (RFC 4180) — spreadsheet-
// friendly for ops teams who paste findings into Excel for triage. One row per
// finding, with a header row. doc may be nil; when provided, flow name and
// file path columns are populated.
func ReportToCSV(report *models.AnalysisReport, doc *models.FlowDocument) ([]byte, error) {
	if report == nil {
		report = &models.AnalysisReport{}
	}

	flowName := ""
	filePath := ""
	if doc != nil {
		flowName = doc.Name
		filePath = doc.FilePath
	}

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	header := []string{"Rule", "Severity", "Title", "Description", "Suggestion", "Flow", "File", "BlockID", "SubflowID"}
	if err := w.Write(header); err != nil {
		return nil, fmt.Errorf("CSV header write: %w", err)
	}

	for _, f := range report.Findings {
		row := []string{
			csvCell(f.RuleID),
			csvCell(string(f.Severity)),
			csvCell(f.Title),
			csvCell(f.Description),
			csvCell(f.Suggestion),
			csvCell(flowName),
			csvCell(filePath),
			csvCell(f.BlockID),
			csvCell(f.SubflowID),
		}
		if err := w.Write(row); err != nil {
			return nil, fmt.Errorf("CSV row write: %w", err)
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("CSV flush: %w", err)
	}
	return buf.Bytes(), nil
}

// csvCell prepares a value for a CSV cell: it normalizes newlines (structure)
// and neutralizes spreadsheet formula injection (security). Applied to every
// cell — for analyzer-constant fields (rule IDs, severities, UUIDs) the formula
// guard is a no-op, so there's no behavior change for them.
func csvCell(s string) string {
	return guardCSVFormula(sanitizeCSVField(s))
}

// sanitizeCSVField normalizes multi-line descriptions/suggestions into single
// cells so they don't break the CSV row structure. Go's encoding/csv already
// quotes fields containing newlines per RFC 4180, but some consumers (Excel)
// handle quoted newlines poorly, so we replace them with spaces.
func sanitizeCSVField(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// guardCSVFormula neutralizes CSV (formula) injection. A finding field can embed
// user-controlled text — a flow name, a variable name, a file path — and if that
// text begins with a formula trigger (= + - @, or a leading tab/CR that some
// apps also treat as one), opening the CSV in Excel / Google Sheets / LibreOffice
// executes it (e.g. a flow named `=cmd|'/c calc'!A1`, or `=HYPERLINK()` for data
// exfiltration). Prefixing a single quote forces the cell to be treated as text;
// spreadsheet apps hide the leading apostrophe on display.
func guardCSVFormula(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}
