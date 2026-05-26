package export

import (
	"bytes"
	"fmt"

	"pad-analyzer/internal/models"

	"github.com/jung-kurt/gofpdf"
)

func ReportToPDF(report *models.AnalysisReport, doc *models.FlowDocument) ([]byte, error) {
	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.SetAutoPageBreak(true, 20)
	pdf.AddPage()

	pdf.SetFont("Helvetica", "B", 20)
	pdf.Cell(0, 12, "PAD Analyzer")
	pdf.Ln(8)
	pdf.SetFont("Helvetica", "", 11)
	pdf.Cell(0, 6, "Analysis Report")
	pdf.Ln(12)

	pdf.SetFont("Helvetica", "", 10)
	pdf.Cell(0, 5, fmt.Sprintf("Flow: %s", doc.Name))
	pdf.Ln(5)
	pdf.Cell(0, 5, fmt.Sprintf("Generated: %s", report.GeneratedAt.Format("2006-01-02 15:04:05")))
	pdf.Ln(5)
	pdf.Cell(0, 5, fmt.Sprintf("Duration: %dms", report.DurationMs))
	pdf.Ln(10)

	pdf.SetFont("Helvetica", "B", 14)
	pdf.Cell(0, 8, "Summary")
	pdf.Ln(8)

	pdf.SetFont("Helvetica", "", 10)
	pdf.Cell(0, 5, fmt.Sprintf("Blocks analyzed: %d", report.Stats.BlocksAnalyzed))
	pdf.Ln(5)
	pdf.Cell(0, 5, fmt.Sprintf("Rules run: %d", report.Stats.RulesRun))
	pdf.Ln(5)
	pdf.Cell(0, 5, fmt.Sprintf("Errors: %d  |  Warnings: %d  |  Info: %d",
		report.Stats.Errors, report.Stats.Warnings, report.Stats.Info))
	pdf.Ln(10)

	if len(report.Findings) == 0 {
		pdf.SetFont("Helvetica", "", 11)
		pdf.Cell(0, 6, "No findings detected. The flow looks good!")
		return finishPDF(pdf)
	}

	pdf.SetFont("Helvetica", "B", 14)
	pdf.Cell(0, 8, fmt.Sprintf("Findings (%d)", len(report.Findings)))
	pdf.Ln(10)

	severityOrder := []models.Severity{models.SeverityError, models.SeverityWarning, models.SeverityInfo}
	for _, sev := range severityOrder {
		findings := filterBySeverity(report.Findings, sev)
		if len(findings) == 0 {
			continue
		}

		pdf.SetFont("Helvetica", "B", 12)
		pdf.Cell(0, 7, fmt.Sprintf("%s (%d)", severityTitle(sev), len(findings)))
		pdf.Ln(8)

		for i, f := range findings {
			if pdf.GetY() > 260 {
				pdf.AddPage()
			}

			pdf.SetFont("Helvetica", "B", 10)
			pdf.Cell(0, 5, fmt.Sprintf("%d. %s", i+1, f.Title))
			pdf.Ln(5)

			pdf.SetFont("Helvetica", "", 9)
			pdf.Cell(0, 4, fmt.Sprintf("Rule: %s  |  Block: %s", f.RuleID, f.BlockID))
			pdf.Ln(4)

			pdf.MultiCell(0, 4, f.Description, "", "", false)
			pdf.Ln(2)

			if f.Suggestion != "" {
				pdf.SetFont("Helvetica", "I", 9)
				pdf.MultiCell(0, 4, "Suggestion: "+f.Suggestion, "", "", false)
				pdf.Ln(3)
			}
		}
	}

	return finishPDF(pdf)
}

func finishPDF(pdf *gofpdf.Fpdf) ([]byte, error) {
	var buf bytes.Buffer
	err := pdf.Output(&buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
