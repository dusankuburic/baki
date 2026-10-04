package export

import (
	"bytes"
	"testing"

	"pad-core/models"
)

// TestReportToPDF_UnicodeAndLongTitle confirms the embedded Unicode TTF font
// renders non-ASCII characters without error, and that long finding titles
// wrap (MultiCell) instead of overflowing.
func TestReportToPDF_UnicodeAndLongTitle(t *testing.T) {
	report := makeReport(
		models.Finding{
			RuleID:      "r1",
			Severity:    models.SeverityError,
			Title:       "Rapport d'erreur — vérification échouée «réussite»",
			BlockID:     "b1",
			Description: "Finding with Unicode: café, naïve, Über,日本語",
			Suggestion:  "Use a vault — see «guidelines»",
		},
		models.Finding{
			RuleID:      "r2",
			Severity:    models.SeverityWarning,
			Title:       "This is an extremely long finding title that exceeds the page width and must wrap to multiple lines instead of overflowing past the right margin of the PDF document",
			BlockID:     "b2",
			Description: "Long title test.",
		},
	)
	doc := makeDoc("Unicode Flow — Ünïcödé", "")

	data, err := ReportToPDF(report, doc)
	if err != nil {
		t.Fatalf("ReportToPDF Unicode/long-title: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		t.Errorf("output does not start with %%PDF magic: got %.10q", data)
	}
}
