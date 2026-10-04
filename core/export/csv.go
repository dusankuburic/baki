package export

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"

	"pad-core/models"
)

// ReportToCSV serializes a report as CSV (RFC 4180): header plus one row per
// finding; doc may be nil. The first line is a `#` summary comment (skip with
// csv.Reader.Comment = '#') carrying the counts with no natural column,
// including findings hidden by inline pad-ignore.
func ReportToCSV(report *models.AnalysisReport, doc *models.FlowDocument) ([]byte, error) {
	if report == nil {
		report = &models.AnalysisReport{}
	}
	doc = withIndexes(doc)

	flowName := ""
	filePath := ""
	if doc != nil {
		flowName = doc.Name
		filePath = doc.FilePath
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# PAD Analyzer: %d finding(s) — %d error(s), %d warning(s), %d info; %d suppressed inline\n",
		len(report.Findings), report.Stats.Errors, report.Stats.Warnings, report.Stats.Info, report.Stats.Suppressed)
	w := csv.NewWriter(&buf)

	// New columns are APPENDED so consumers indexing the original nine keep
	// working.
	header := []string{"Rule", "Severity", "Title", "Description", "Suggestion", "Flow", "File", "BlockID", "SubflowID", "Line", "Fingerprint", "Category"}
	if err := w.Write(header); err != nil {
		return nil, fmt.Errorf("CSV header write: %w", err)
	}

	for _, f := range report.Findings {
		line := ""
		if n := blockLine(f, doc); n > 0 {
			line = strconv.Itoa(n)
		}
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
			line,
			csvCell(f.Fingerprint),
			csvCell(f.Category),
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

// csvCell prepares a value for a CSV cell: normalizes newlines (structure) and
// neutralizes spreadsheet formula injection (security); for analyzer-constant
// fields (rule IDs, severities, UUIDs) the formula guard is a no-op.
func csvCell(s string) string {
	return guardCSVFormula(sanitizeCSVField(s))
}

// sanitizeCSVField normalizes multi-line descriptions/suggestions into single
// cells: encoding/csv already quotes newlines per RFC 4180, but some consumers
// (Excel) handle quoted newlines poorly.
func sanitizeCSVField(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// guardCSVFormula neutralizes CSV formula injection: user-controlled text
// (flow/variable names, file paths) beginning with a trigger (= + - @, or a
// leading tab/CR some apps also treat as one) executes when the CSV is opened
// in Excel / Google Sheets / LibreOffice. A prefixed single quote forces text
// treatment and is hidden on display.
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
