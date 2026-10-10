package export

import (
	"bytes"
	"compress/zlib"
	_ "embed"
	"fmt"
	"sort"
	"strconv"

	"pad-core/models"

	"github.com/go-pdf/fpdf"
)

//go:embed fonts/JetBrainsMono-Regular.ttf
var jetbrainsMonoTTF []byte

//go:embed fonts/JetBrainsMono-Bold.ttf
var jetbrainsMonoBoldTTF []byte

const pdfFontFamily = "JetBrainsMono"

func init() {
	registerBuiltin(Format{ID: "pdf", Label: "PDF", MIME: "application/pdf", Ext: ".pdf", Binary: true, Order: 70},
		func(in Input, _ Options) ([]byte, error) { return ReportToPDF(in.Report, in.Doc) })
}

func ReportToPDF(report *models.AnalysisReport, doc *models.FlowDocument) ([]byte, error) {
	return reportToPDF(report, doc, finishPDF)
}

// reportToPDF lays the report out and serializes it with finish. The seam lets
// the tests serialize the same layout with fpdf's own compression, which the
// reference finishPDF must reproduce byte for byte.
func reportToPDF(report *models.AnalysisReport, doc *models.FlowDocument, finish func(*fpdf.Fpdf) ([]byte, error)) ([]byte, error) {
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
		return finish(pdf)
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

	return finish(pdf)
}

// finishPDF serializes pdf with page content streams zlib-compressed, byte
// for byte what fpdf's own page compression writes — recompressing through
// ONE reset writer avoids fpdf's per-page zlib state (~90% of export
// allocations). On an unexpected layout it returns the uncompressed document:
// still a valid PDF, just larger.
func finishPDF(pdf *fpdf.Fpdf) ([]byte, error) {
	pdf.SetCompression(false)
	pages := pdf.PageCount()
	var buf bytes.Buffer
	err := pdf.Output(&buf)
	if err != nil {
		return nil, err
	}
	if out, ok := recompressPageStreams(buf.Bytes(), pages); ok {
		return out, nil
	}
	return buf.Bytes(), nil
}

// recompressPageStreams turns fpdf's uncompressed serialization (raw) of a
// document with the given page count into the one fpdf writes with
// compression on: each page content object
//
//	N 0 obj\n<</Length L>>\nstream\n<L bytes>\nendstream\nendobj\n
//
// becomes
//
//	N 0 obj\n<</Filter /FlateDecode /Length C>>\nstream\n<C bytes>\nendstream\nendobj\n
//
// (zlib at BestSpeed, fpdf's level), and every xref offset and startxref
// move by the bytes that changed before them. Every structural assumption is
// checked; ok is false if any fails.
func recompressPageStreams(raw []byte, pages int) (out []byte, ok bool) {
	// Trailer: "\nstartxref\n<offset>\n%%EOF\n".
	const startxref = "\nstartxref\n"
	tail := bytes.LastIndex(raw, []byte(startxref))
	if tail < 0 {
		return nil, false
	}
	numAt := tail + len(startxref)
	numEnd := bytes.IndexByte(raw[numAt:], '\n')
	if numEnd < 0 {
		return nil, false
	}
	xrefAt, err := strconv.Atoi(string(raw[numAt : numAt+numEnd]))
	if err != nil || xrefAt <= 0 || xrefAt >= tail {
		return nil, false
	}

	// Cross-reference table: "xref\n0 <size>\n0000000000 65535 f \n", then
	// one 20-byte "%010d 00000 n \n" entry for each object 1..size-1.
	x := raw[xrefAt:]
	if !bytes.HasPrefix(x, []byte("xref\n0 ")) {
		return nil, false
	}
	x = x[len("xref\n0 "):]
	nl := bytes.IndexByte(x, '\n')
	if nl < 0 {
		return nil, false
	}
	size, err := strconv.Atoi(string(x[:nl]))
	if err != nil || size < 2 {
		return nil, false
	}
	x = x[nl+1:]
	const free = "0000000000 65535 f \n"
	const entryLen = 20
	if !bytes.HasPrefix(x, []byte(free)) || len(x) < len(free)+(size-1)*entryLen {
		return nil, false
	}
	entries := x[len(free):]
	entriesAt := len(raw) - len(entries)
	offsets := make([]int, size) // [0] unused
	for n := 1; n < size; n++ {
		e := entries[(n-1)*entryLen : n*entryLen]
		if string(e[10:]) != " 00000 n \n" {
			return nil, false
		}
		off, err := strconv.Atoi(string(e[:10]))
		if err != nil || off <= 0 || off >= xrefAt {
			return nil, false
		}
		offsets[n] = off
	}

	// Objects in file order; each one runs to the next (the last to the xref).
	order := make([]int, 0, size-1)
	for n := 1; n < size; n++ {
		order = append(order, n)
	}
	sort.Slice(order, func(i, j int) bool { return offsets[order[i]] < offsets[order[j]] })
	end := make([]int, size)
	for i, n := range order {
		end[n] = xrefAt
		if i+1 < len(order) {
			end[n] = offsets[order[i+1]]
		}
		if end[n] <= offsets[n] {
			return nil, false
		}
	}

	// Each page object names its content object: "/Contents N 0 R>>".
	content := make(map[int]bool, pages)
	for n := 1; n < size; n++ {
		obj := raw[offsets[n]:end[n]]
		if !bytes.HasPrefix(obj, []byte(strconv.Itoa(n)+" 0 obj\n<</Type /Page\n")) {
			continue
		}
		i := bytes.Index(obj, []byte("/Contents "))
		if i < 0 {
			return nil, false
		}
		ref := obj[i+len("/Contents "):]
		sp := bytes.IndexByte(ref, ' ')
		if sp < 0 || !bytes.HasPrefix(ref[sp:], []byte(" 0 R>>\n")) {
			return nil, false
		}
		cn, err := strconv.Atoi(string(ref[:sp]))
		if err != nil || cn <= 0 || cn >= size {
			return nil, false
		}
		content[cn] = true
	}
	if len(content) != pages {
		return nil, false
	}

	var zbuf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&zbuf, zlib.BestSpeed)
	if err != nil {
		return nil, false
	}
	var w bytes.Buffer
	w.Grow(len(raw))
	w.Write(raw[:offsets[order[0]]])
	newOffsets := make([]int, size)
	for _, n := range order {
		newOffsets[n] = w.Len()
		obj := raw[offsets[n]:end[n]]
		if !content[n] {
			w.Write(obj)
			continue
		}
		head := strconv.Itoa(n) + " 0 obj\n<</Length "
		if !bytes.HasPrefix(obj, []byte(head)) {
			return nil, false
		}
		rest := obj[len(head):]
		gt := bytes.Index(rest, []byte(">>\nstream\n"))
		if gt < 0 {
			return nil, false
		}
		length, err := strconv.Atoi(string(rest[:gt]))
		if err != nil || length < 0 {
			return nil, false
		}
		body := rest[gt+len(">>\nstream\n"):]
		const trailer = "\nendstream\nendobj\n"
		if len(body) != length+len(trailer) || string(body[length:]) != trailer {
			return nil, false
		}
		zbuf.Reset()
		zw.Reset(&zbuf)
		if _, err := zw.Write(body[:length]); err != nil {
			return nil, false
		}
		if err := zw.Close(); err != nil {
			return nil, false
		}
		w.WriteString(strconv.Itoa(n))
		w.WriteString(" 0 obj\n<</Filter /FlateDecode /Length ")
		w.WriteString(strconv.Itoa(zbuf.Len()))
		w.WriteString(">>\nstream\n")
		w.Write(zbuf.Bytes())
		w.WriteString(trailer)
	}

	newXrefAt := w.Len()
	w.Write(raw[xrefAt:entriesAt])
	for n := 1; n < size; n++ {
		fmt.Fprintf(&w, "%010d 00000 n \n", newOffsets[n])
	}
	w.Write(raw[entriesAt+(size-1)*entryLen : tail])
	fmt.Fprintf(&w, "%s%d\n", startxref, newXrefAt)
	w.Write(raw[numAt+numEnd+1:])
	return w.Bytes(), true
}
