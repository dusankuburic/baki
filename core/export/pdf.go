package export

import (
	"bytes"
	_ "embed"
	"fmt"

	"pad-core/models"

	"github.com/go-pdf/fpdf"
)

//go:embed fonts/JetBrainsMono-Regular.ttf
var jetbrainsMonoTTF []byte

//go:embed fonts/JetBrainsMono-Bold.ttf
var jetbrainsMonoBoldTTF []byte

const pdfFontFamily = "JetBrainsMono"

func ReportToPDF(report *models.AnalysisReport, doc *models.FlowDocument) ([]byte, error) {
	if report == nil {
		report = &models.AnalysisReport{}
	}

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetAutoPageBreak(true, 20)
	pdf.AddPage()

	// Register the embedded Unicode TTFs so non-ASCII characters (é, ü, —, curly
	// quotes, CJK) render correctly. The built-in core fonts (Helvetica) use
	// WinAnsi encoding and either mis-render or error on anything outside ASCII.
	pdf.AddUTF8FontFromBytes(pdfFontFamily, "", jetbrainsMonoTTF)
	pdf.AddUTF8FontFromBytes(pdfFontFamily, "B", jetbrainsMonoBoldTTF)

	pdf.SetFont(pdfFontFamily, "B", 20)
	pdf.Cell(0, 12, "PAD Analyzer")
	pdf.Ln(8)
	pdf.SetFont(pdfFontFamily, "", 11)
	pdf.Cell(0, 6, "Analysis Report")
	pdf.Ln(12)

	pdf.SetFont(pdfFontFamily, "", 10)
	if doc != nil {
		pdf.Cell(0, 5, fmt.Sprintf("Flow: %s", doc.Name))
	} else {
		pdf.Cell(0, 5, "Flow: (unknown)")
	}
	pdf.Ln(5)
	if !report.GeneratedAt.IsZero() {
		pdf.Cell(0, 5, fmt.Sprintf("Generated: %s", report.GeneratedAt.Format("2006-01-02 15:04:05")))
		pdf.Ln(5)
	}
	if report.DurationMs > 0 {
		pdf.Cell(0, 5, fmt.Sprintf("Duration: %dms", report.DurationMs))
		pdf.Ln(5)
	}
	pdf.Ln(5)

	pdf.SetFont(pdfFontFamily, "B", 14)
	pdf.Cell(0, 8, "Summary")
	pdf.Ln(8)

	pdf.SetFont(pdfFontFamily, "", 10)
	pdf.Cell(0, 5, fmt.Sprintf("Blocks analyzed: %d", report.Stats.BlocksAnalyzed))
	pdf.Ln(5)
	pdf.Cell(0, 5, fmt.Sprintf("Rules run: %d", report.Stats.RulesRun))
	pdf.Ln(5)
	pdf.Cell(0, 5, fmt.Sprintf("Errors: %d  |  Warnings: %d  |  Info: %d",
		report.Stats.Errors, report.Stats.Warnings, report.Stats.Info))
	pdf.Ln(10)

	if len(report.Findings) == 0 {
		pdf.SetFont(pdfFontFamily, "", 11)
		pdf.Cell(0, 6, "No findings detected. The flow looks good!")
		return finishPDF(pdf)
	}

	pdf.SetFont(pdfFontFamily, "B", 14)
	pdf.Cell(0, 8, fmt.Sprintf("Findings (%d)", len(report.Findings)))
	pdf.Ln(10)

	severityOrder := []models.Severity{models.SeverityError, models.SeverityWarning, models.SeverityInfo}
	grouped := groupFindingsBySeverity(report.Findings)
	for _, sev := range severityOrder {
		idxs := grouped[sev]
		if len(idxs) == 0 {
			continue
		}

		pdf.SetFont(pdfFontFamily, "B", 12)
		pdf.Cell(0, 7, fmt.Sprintf("%s (%d)", severityTitle(sev), len(idxs)))
		pdf.Ln(8)

		for i, fi := range idxs {
			f := &report.Findings[fi]
			if pdf.GetY() > 260 {
				pdf.AddPage()
			}

			// Use MultiCell (not Cell) for the title so long titles wrap
			// instead of overflowing past the right margin.
			pdf.SetFont(pdfFontFamily, "B", 10)
			pdf.MultiCell(0, 5, fmt.Sprintf("%d. %s", i+1, f.Title), "", "", false)
			pdf.Ln(1)

			pdf.SetFont(pdfFontFamily, "", 9)
			pdf.Cell(0, 4, fmt.Sprintf("Rule: %s  |  Block: %s", f.RuleID, f.BlockID))
			pdf.Ln(4)

			pdf.MultiCell(0, 4, f.Description, "", "", false)
			pdf.Ln(2)

			if f.Suggestion != "" {
				pdf.SetFont(pdfFontFamily, "", 9)
				pdf.MultiCell(0, 4, "Suggestion: "+f.Suggestion, "", "", false)
				pdf.Ln(3)
			}
		}
	}

	return finishPDF(pdf)
}

func finishPDF(pdf *fpdf.Fpdf) ([]byte, error) {
	var buf bytes.Buffer
	err := pdf.Output(&buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
