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
			f.RuleID,
			string(f.Severity),
			f.Title,
			sanitizeCSVField(f.Description),
			sanitizeCSVField(f.Suggestion),
			flowName,
			filePath,
			f.BlockID,
			f.SubflowID,
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
