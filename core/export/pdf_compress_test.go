package export

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"pad-core/models"

	"github.com/go-pdf/fpdf"
)

// fpdfCompressedOutput is the pre-change finishPDF: fpdf's own page
// compression, one new zlib writer per page. It is the reference the
// single-writer recompression must reproduce byte for byte.
func fpdfCompressedOutput(pdf *fpdf.Fpdf) ([]byte, error) {
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pdfDates matches fpdf's /CreationDate and /ModDate, stamped from the clock
// at Output; masking keeps the byte length so xref offsets are unaffected.
var pdfDates = regexp.MustCompile(`/(CreationDate|ModDate) \(D:\d{14}\)`)

func maskPDFDates(b []byte) []byte {
	return pdfDates.ReplaceAll(b, []byte("/$1 (D:00000000000000)"))
}

func manyFindings(n int) *models.AnalysisReport {
	sev := []models.Severity{models.SeverityError, models.SeverityWarning, models.SeverityInfo}
	fs := make([]models.Finding, n)
	for i := range fs {
		fs[i] = models.Finding{
			RuleID:      "unhandled-error",
			Severity:    sev[i%3],
			Title:       fmt.Sprintf("Action without error handling #%d — «vérifié»", i),
			BlockID:     fmt.Sprintf("0b6e2f1c-%08d", i),
			Description: "This action can fail at runtime and nothing handles the error, so the whole flow stops. Ünïcödé 日本語.",
		}
		if i%2 == 0 {
			fs[i].Suggestion = "Wrap the action in ON BLOCK ERROR … END and log %LastError%."
		}
	}
	r := makeReport(fs...)
	r.GeneratedAt = time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	r.DurationMs = 42
	return r
}

// TestReportToPDF_MatchesFpdfCompression proves finishPDF writes the exact
// bytes fpdf's own compression writes (the layout code is shared, so this is
// "renders the same as before"), for empty, one-page, multi-page and nil
// inputs.
func TestReportToPDF_MatchesFpdfCompression(t *testing.T) {
	cases := []struct {
		name   string
		report *models.AnalysisReport
		doc    *models.FlowDocument
	}{
		{"nil", nil, nil},
		{"no findings", makeReport(), makeDoc("Empty", "")},
		{"one page", manyFindings(3), makeDoc("Small — Ünïcödé", "")},
		{"many pages", manyFindings(400), makeDoc("Big", `C:\flows\big.txt`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// fpdf emits its font resources in map order unless the catalog
			// is sorted, so the comparison sorts it on both sides.
			got, err := reportToPDF(tc.report, tc.doc, func(pdf *fpdf.Fpdf) ([]byte, error) {
				pdf.SetCatalogSort(true)
				return finishPDF(pdf)
			})
			if err != nil {
				t.Fatal(err)
			}
			want, err := reportToPDF(tc.report, tc.doc, func(pdf *fpdf.Fpdf) ([]byte, error) {
				pdf.SetCatalogSort(true)
				return fpdfCompressedOutput(pdf)
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(want, []byte("/Filter /FlateDecode /Length")) {
				t.Fatal("fixture rot: the reference has no compressed page stream")
			}
			if !bytes.Equal(maskPDFDates(got), maskPDFDates(want)) {
				t.Errorf("output differs from fpdf's own compression: %d vs %d bytes", len(got), len(want))
			}
		})
	}
}

// TestRecompressPageStreams_StreamsInflate checks the result independently of
// the reference: every page stream is compressed and inflates, and every xref
// offset points at its object.
func TestRecompressPageStreams_StreamsInflate(t *testing.T) {
	out, err := ReportToPDF(manyFindings(60), makeDoc("Flow", ""))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("0 obj\n<</Length ")) && !bytes.Contains(out, []byte("/Length1 ")) {
		t.Error("an uncompressed stream remains")
	}
	streams := regexp.MustCompile(`(?s)(\d+) 0 obj\n<</Filter /FlateDecode /Length (\d+)>>\nstream\n`).FindAllSubmatchIndex(out, -1)
	if len(streams) < 2 {
		t.Fatalf("found %d compressed page streams, want several", len(streams))
	}
	for _, m := range streams {
		var n int
		fmt.Sscanf(string(out[m[4]:m[5]]), "%d", &n)
		zr, err := zlib.NewReader(bytes.NewReader(out[m[1] : m[1]+n]))
		if err != nil {
			t.Fatalf("stream at %d: %v", m[0], err)
		}
		if _, err := io.Copy(io.Discard, zr); err != nil {
			t.Fatalf("stream at %d: %v", m[0], err)
		}
	}
	xref := bytes.LastIndex(out, []byte("\nxref\n")) + 1
	var start int
	fmt.Sscanf(string(out[bytes.LastIndex(out, []byte("startxref\n"))+len("startxref\n"):]), "%d", &start)
	if start != xref {
		t.Fatalf("startxref %d, xref table at %d", start, xref)
	}
	lines := strings.Split(string(out[xref:]), "\n")
	for i, l := range lines[3:] {
		if !strings.HasSuffix(l, " 00000 n ") {
			break
		}
		var off int
		fmt.Sscanf(l, "%d", &off)
		if want := fmt.Sprintf("%d 0 obj\n", i+1); !bytes.HasPrefix(out[off:], []byte(want)) {
			t.Fatalf("xref entry %d -> %d points at %.12q", i+1, off, out[off:])
		}
	}
}

// TestRecompressPageStreams_RejectsUnknownLayout: anything that is not
// fpdf's layout is refused (finishPDF then keeps the uncompressed document).
func TestRecompressPageStreams_RejectsUnknownLayout(t *testing.T) {
	for _, raw := range []string{"", "%PDF-1.3\n%%EOF\n", "garbage\nstartxref\n5\n%%EOF\n"} {
		if _, ok := recompressPageStreams([]byte(raw), 1); ok {
			t.Errorf("accepted %q", raw)
		}
	}
}

// TestReportToPDF_AllocationBound pins the export's allocations for a large
// report. fpdf's per-page zlib writer (~1 MB of deflate state each) made a
// 3,100-finding report allocate ~360 MB; with one reset writer it is ~45 MB.
func TestReportToPDF_AllocationBound(t *testing.T) {
	report := manyFindings(3100)
	doc := makeDoc("Big", "")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out, err := ReportToPDF(report, doc)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("/Filter /FlateDecode")) {
		t.Fatal("output is not compressed")
	}
	const limit = 120 << 20
	if got := after.TotalAlloc - before.TotalAlloc; got > limit {
		t.Errorf("ReportToPDF(3100 findings) allocated %d MB, want <= %d MB", got>>20, limit>>20)
	} else {
		t.Logf("ReportToPDF(3100 findings) allocated %d MB, %d bytes out", got>>20, len(out))
	}
}
