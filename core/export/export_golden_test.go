package export

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"

	"github.com/go-pdf/fpdf"
)

var updateExportGolden = flag.Bool("update-export-golden", false, "rewrite core/export/testdata/golden/*.golden from the current exporters")

// Byte goldens of every format over four reports, so a refactor of the
// exporters can prove it renders exactly what it rendered before. Fixture
// text avoids raw CRs: .golden files are LF by .gitattributes, and a CR that
// an exporter writes through unescaped would not survive a checkout.
const exportGoldenDir = "testdata/golden"

var exportGoldenTime = time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)

// Inline pad-ignore directives (Stats.Suppressed > 0), HTML/Markdown special
// characters, a credential, a TODO and a zero wait.
const exportSuppressedFlow = `#Region "Main"
    # pad-ignore[unhandled-error]
    File.ReadTextFromFile.ReadText File: $'''C:\data\in.txt''' Encoding: File.TextFileEncoding.UTF8 Content=> FileContents
    # pad-ignore[missing-delay]
    Display.ShowMessageBox Message: $'''Read %FileContents% — «ok»'''
    # pad-ignore
    WebAutomation.NavigateTo Url: $'''http://example.com/api?a=1&b=<2>''' BrowserInstance: %Browser%
    SET Password TO $'''hunter2'''
    IF %FileContents% = $'''x''' THEN
        SET Seen TO True
    END
    # TODO: remove the =SUM(A1) hack
    WAIT 0
#EndRegion
`

// A two-member folder flow: SourceFile URIs per member, a call between them,
// network/database/file actions with metadata, rendered with a policy verdict
// and a source root.
var exportFolderFlow = map[string]string{
	"Main.txt": `#Region "Main"
    CALL Billing
    Excel.LaunchExcel Visible: True => ExcelInstance
    LOOP FOREACH CurrentRow IN %ExcelData%
        WebAutomation.Click.Click BrowserInstance: %Browser% Control: appmask['Submit']
        WebAutomation.Click.Click BrowserInstance: %Browser% Control: appmask['Next']
    END
#EndRegion
`,
	"Billing.txt": `#Region "Billing"
    Database.Connect ConnectionString: $'''Server=10.0.0.5;Database=billing''' Connection=> SQLConnection
    Database.ExecuteSqlStatement.Execute Connection: SQLConnection Statement: $'''SELECT * FROM invoices WHERE id = %InvoiceId%''' Timeout: 30 Result=> QueryResult
    File.Delete Files: $'''/home/ops/tmp/invoice.csv'''
#EndRegion
`,
}

// The preview case: a credential (its rule is a preview while the case is
// analyzed, so the finding is stamped) and an http URL (its rule is flipped to
// preview after the analysis, so the finding is unstamped, like JSON saved
// before stages).
const exportPreviewFlow = `#Region "Main"
    SET Password TO $'''hunter2'''
    WebAutomation.NavigateTo Url: $'''http://example.com/status''' BrowserInstance: %Browser%
#EndRegion
`

// exportPreviewStamped and exportPreviewFlipped are the preview case's two
// preview rules (see exportPreviewFlow).
const (
	exportPreviewStamped = "hardcoded-credential"
	exportPreviewFlipped = "insecure-http-url"
)

// setPreviewForTesting moves the built-in id to the preview stage until the
// returned func runs.
func setPreviewForTesting(t *testing.T, id string) (restore func()) {
	t.Helper()
	m, ok := analyzer.LookupMeta(id)
	if !ok {
		t.Fatalf("%s is not a built-in", id)
	}
	m.Stage = analyzer.StagePreview
	return analyzer.SetRuleMetaForTesting(id, m)
}

type exportGoldenCase struct {
	name   string
	report *models.AnalysisReport
	doc    *models.FlowDocument
	opts   Options // SARIF, JUnit and JSON (Policy) only; the other formats take none
}

// normalizeExportDoc replaces the parser's per-parse UUIDs with positional IDs
// so the exported bytes are stable.
func normalizeExportDoc(doc *models.FlowDocument, id string) {
	doc.ID = id
	doc.Metadata.ParsedAt = time.Time{}
	ids := map[string]string{}
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		ids[sf.ID] = fmt.Sprintf("sf-%d", i+1)
		sf.ID = ids[sf.ID]
	}
	n := 0
	var walk func(bs []models.Block)
	walk = func(bs []models.Block) {
		for j := range bs {
			n++
			ids[bs[j].ID] = fmt.Sprintf("b-%03d", n)
			bs[j].ID = ids[bs[j].ID]
			walk(bs[j].Children)
		}
	}
	var fix func(bs []models.Block)
	fix = func(bs []models.Block) {
		for j := range bs {
			if bs[j].ParentID != "" {
				bs[j].ParentID = ids[bs[j].ParentID]
			}
			if bs[j].SubflowID != "" {
				bs[j].SubflowID = ids[bs[j].SubflowID]
			}
			fix(bs[j].Children)
		}
	}
	for i := range doc.Subflows {
		walk(doc.Subflows[i].Blocks)
	}
	for i := range doc.Subflows {
		fix(doc.Subflows[i].Blocks)
	}
	doc.RebuildIndexes()
}

func analyzeForExport(t *testing.T, doc *models.FlowDocument) *models.AnalysisReport {
	t.Helper()
	report := analyzer.RunAnalysis(doc, analyzer.AllRules(), models.DefaultSettings(), nil)
	report.GeneratedAt = exportGoldenTime
	report.DurationMs = 1234
	for i := range report.RuleProfiles {
		report.RuleProfiles[i].DurationMs = 0
	}
	return report
}

func exportGoldenCases(t *testing.T) []exportGoldenCase {
	t.Helper()

	suppressedDoc, err := parser.ParseText(exportSuppressedFlow, "Main.txt", int64(len(exportSuppressedFlow)))
	if err != nil {
		t.Fatal(err)
	}
	normalizeExportDoc(suppressedDoc, "doc-suppressed")
	suppressedDoc.Name = `Invoices <Q3> & "Ops"`
	suppressedDoc.FilePath = "flows/invoices/Main.txt"
	suppressed := analyzeForExport(t, suppressedDoc)
	if suppressed.Stats.Suppressed == 0 {
		t.Fatal("fixture rot: the suppressed case suppresses nothing")
	}

	folderDoc, err := parser.ParseFiles(exportFolderFlow, "=Invoices+Ops")
	if err != nil {
		t.Fatal(err)
	}
	normalizeExportDoc(folderDoc, "doc-folder")
	folderDoc.FilePath = "flows/teamA"
	folder := analyzeForExport(t, folderDoc)
	if !folderDoc.IsFolder || len(folder.Findings) < 3 {
		t.Fatalf("fixture rot: folder case has IsFolder=%t and %d findings", folderDoc.IsFolder, len(folder.Findings))
	}
	zero := 0
	policy := models.Policy{
		ID:           "pol-release",
		Name:         "Release gate <v2>",
		GateSeverity: models.SeverityWarning,
		Rules: []models.PolicyRule{
			{RuleID: "unhandled-error", Enabled: true},
			{RuleID: "sql-injection-risk", Enabled: true, Severity: models.SeverityError},
			{RuleID: "hardcoded-filepath", Enabled: true, MaxCount: &zero},
			{RuleID: "missing-delay", Enabled: true},
		},
		Waivers: []models.PolicyWaiver{{FindingKey: folder.Findings[0].Fingerprint, Reason: "accepted"}},
	}

	unhandled := models.Finding{
		ID: "F-004", RuleID: "unhandled-error", Severity: models.SeverityWarning,
		Confidence: models.ConfidenceMedium, AutoFix: analyzer.RuleAutoFix("unhandled-error"),
		Title: "Unhandled error in fallible action", Description: "This action can fail at runtime.",
		Suggestion: "Wrap it in ON BLOCK ERROR.", BlockID: "blk-3", SubflowID: "sf-1",
		Fingerprint: "unhandled-error:8899aabbccddeeff",
	}
	handmade := &models.AnalysisReport{
		FlowID:      "flow-handmade",
		FlowName:    "Hand-made <flow>",
		GeneratedAt: exportGoldenTime,
		Findings: []models.Finding{
			{
				ID: "F-001", RuleID: "org-no-msgbox", Severity: models.SeverityWarning, Category: "Custom",
				Title:       `=HYPERLINK("http://x")`,
				Description: "Line one\nLine two <b>&amp;</b> 'quoted' \"dq\"",
				Suggestion:  "@risk: use a log action",
				BlockID:     "blk-1", SubflowID: "sf-1",
				Metadata: map[string]interface{}{"property": "Message"},
			},
			{
				ID: "F-002", RuleID: "parse-error", Severity: models.SeverityError, Category: "Syntax",
				Title: "Parse error (line 3)", Description: "malformed line", Fingerprint: "parse-error:",
			},
			{
				ID: "F-003", RuleID: "todo-in-comment", Severity: models.SeverityInfo,
				Title: "TODO in comment", Description: "Ünïcödé — 日本語 TODO",
				BlockID: "blk-2", SubflowID: "sf-1", Fingerprint: "todo-in-comment:0011223344556677",
			},
			unhandled,
			{
				ID: "F-005", RuleID: "org-legacy", Severity: models.Severity("critical"),
				Title: "Legacy severity", Description: "A severity no consumer knows.",
				BlockID: "blk-4", SubflowID: "sf-2", Fingerprint: "org-legacy:1234",
			},
		},
		Stats: models.AnalysisStats{Errors: 1, Warnings: 2, Info: 1, BlocksAnalyzed: 7, RulesRun: 46, Suppressed: 2},
	}

	clean := &models.AnalysisReport{
		FlowID:      "flow-clean",
		GeneratedAt: exportGoldenTime,
		Findings:    []models.Finding{},
		Stats:       models.AnalysisStats{BlocksAnalyzed: 3, RulesRun: 45},
		DurationMs:  7,
		Metrics:     &models.FlowMetrics{HealthScore: 100, SubflowCount: 1, TotalBlocks: 3},
	}

	cases := []exportGoldenCase{
		{name: "suppressed", report: suppressed, doc: suppressedDoc},
		{name: "folder_policy", report: folder, doc: folderDoc, opts: Options{
			Policy:        analyzer.EvaluatePolicy(folder, policy),
			SourceRoot:    "flows/teamA",
			SourceRootURI: "file:///home/runner/work/baki/baki",
		}},
		{name: "handmade_nodoc", report: handmade},
		{name: "clean", report: clean, doc: &models.FlowDocument{Name: "Clean", FilePath: "flows/clean.txt"}},
	}
	// The flip of exportPreviewFlipped lasts until the calling test ends, so
	// no other case may have a finding of that rule.
	for _, c := range cases {
		for _, f := range c.report.Findings {
			if f.RuleID == exportPreviewFlipped || f.Stage != "" {
				t.Fatalf("fixture rot: case %s has a %s finding with stage %q", c.name, f.RuleID, f.Stage)
			}
		}
	}
	return append(cases, previewGoldenCase(t))
}

// previewGoldenCase analyzes exportPreviewFlow with exportPreviewStamped in
// preview, then flips exportPreviewFlipped to preview until t ends, and
// evaluates a policy over both and one stable rule.
func previewGoldenCase(t *testing.T) exportGoldenCase {
	t.Helper()
	doc, err := parser.ParseText(exportPreviewFlow, "Main.txt", int64(len(exportPreviewFlow)))
	if err != nil {
		t.Fatal(err)
	}
	normalizeExportDoc(doc, "doc-preview")
	doc.Name = "Preview rules"
	doc.FilePath = "flows/preview/Main.txt"

	restore := setPreviewForTesting(t, exportPreviewStamped)
	report := analyzeForExport(t, doc)
	restore()
	t.Cleanup(setPreviewForTesting(t, exportPreviewFlipped))

	stamped, flipped := 0, 0
	for _, f := range report.Findings {
		switch {
		case f.RuleID == exportPreviewStamped && f.Stage == "preview":
			stamped++
		case f.RuleID == exportPreviewFlipped && f.Stage == "":
			flipped++
		case f.Stage != "":
			t.Fatalf("fixture rot: %s finding stamped %q", f.RuleID, f.Stage)
		}
	}
	if stamped == 0 || flipped == 0 {
		t.Fatalf("fixture rot: %d stamped %s and %d unstamped %s findings", stamped, exportPreviewStamped, flipped, exportPreviewFlipped)
	}
	policy := models.Policy{
		ID:           "pol-preview",
		Name:         "Preview never gates",
		GateSeverity: models.SeverityInfo,
		Rules: []models.PolicyRule{
			{RuleID: exportPreviewStamped, Enabled: true},
			{RuleID: exportPreviewFlipped, Enabled: true, Severity: models.SeverityError},
			{RuleID: "unhandled-error", Enabled: true},
		},
	}
	res := analyzer.EvaluatePolicy(report, policy)
	if res.Preview != stamped+flipped {
		t.Fatalf("policy set aside %d preview findings, want %d", res.Preview, stamped+flipped)
	}
	return exportGoldenCase{name: "preview", report: report, doc: doc, opts: Options{Policy: res}}
}

// exportGoldenFormats renders one case per format. PDF is pinned through its
// public entry point with fpdf's documented test defaults (sorted catalogs,
// fixed creation and modification dates; see withDeterministicPDF), and stored
// as a digest of the bytes plus the decoded page content streams.
var exportGoldenFormats = []struct {
	ext    string
	render func(c exportGoldenCase) ([]byte, error)
}{
	{"csv", func(c exportGoldenCase) ([]byte, error) { return ReportToCSV(c.report, c.doc) }},
	{"html", func(c exportGoldenCase) ([]byte, error) { return []byte(ReportToHTML(c.report, c.doc)), nil }},
	{"junit", func(c exportGoldenCase) ([]byte, error) { return ReportToJUnitWithOptions(c.report, c.doc, c.opts) }},
	{"md", func(c exportGoldenCase) ([]byte, error) { return []byte(ReportToMarkdown(c.report, c.doc)), nil }},
	{"sarif", func(c exportGoldenCase) ([]byte, error) { return ReportToSARIFWithOptions(c.report, c.doc, c.opts) }},
	{"pdf", func(c exportGoldenCase) ([]byte, error) {
		b, err := ReportToPDF(c.report, c.doc)
		if err != nil {
			return nil, err
		}
		return pdfGoldenText(b)
	}},
	// json has no ReportTo* function: it was the CLI's own encoder until the
	// registry, so its golden is rendered through the registry.
	{"json", func(c exportGoldenCase) ([]byte, error) {
		return Bytes("json", Input{Report: c.report, Doc: c.doc}, c.opts)
	}},
}

// withDeterministicPDF sets fpdf's process-wide defaults that make its output
// reproducible: production never sets them, so ReportToPDF stamps the clock
// and emits font resources in map order.
func withDeterministicPDF(t *testing.T) {
	fpdf.SetDefaultCatalogSort(true)
	fpdf.SetDefaultCreationDate(exportGoldenTime)
	fpdf.SetDefaultModificationDate(exportGoldenTime)
	t.Cleanup(func() {
		fpdf.SetDefaultCatalogSort(false)
		fpdf.SetDefaultCreationDate(time.Time{})
		fpdf.SetDefaultModificationDate(time.Time{})
	})
}

var (
	goldenPDFPages  = regexp.MustCompile(`(?s)\d+ 0 obj\n<</Type /Page\n.*?/Contents (\d+) 0 R>>`)
	goldenPDFString = regexp.MustCompile(`\(((?:\\.|[^\\)])*)\)`)
	// goldenPDFUnescape undoes fpdf's escape (backslash, parentheses, CR).
	goldenPDFUnescape = strings.NewReplacer(`\\`, `\`, `\(`, `(`, `\)`, `)`, `\r`, "\r")
)

// pdfGoldenText renders a PDF as its size and digest plus every page's
// decompressed content stream, with string literals decoded from fpdf's
// UTF-16BE (every font here is a UTF-8 font) and Go-quoted.
func pdfGoldenText(b []byte) ([]byte, error) {
	var sb strings.Builder
	sum := sha256.Sum256(b)
	fmt.Fprintf(&sb, "bytes: %d sha256: %s\n", len(b), hex.EncodeToString(sum[:]))
	pages := goldenPDFPages.FindAllSubmatch(b, -1)
	fmt.Fprintf(&sb, "pages: %d\n", len(pages))
	for i, m := range pages {
		obj := string(m[1])
		head := []byte("\n" + obj + " 0 obj\n<</Filter /FlateDecode /Length ")
		at := bytes.Index(b, head)
		if at < 0 {
			return nil, fmt.Errorf("page %d: content object %s not found", i+1, obj)
		}
		rest := b[at+len(head):]
		gt := bytes.Index(rest, []byte(">>\nstream\n"))
		if gt < 0 {
			return nil, fmt.Errorf("page %d: malformed stream header", i+1)
		}
		n, err := strconv.Atoi(string(rest[:gt]))
		if err != nil {
			return nil, err
		}
		body := rest[gt+len(">>\nstream\n"):]
		zr, err := zlib.NewReader(bytes.NewReader(body[:n]))
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		decoded := goldenPDFString.ReplaceAllFunc(content, func(lit []byte) []byte {
			s := []byte(goldenPDFUnescape.Replace(string(goldenPDFString.FindSubmatch(lit)[1])))
			if len(s)%2 != 0 {
				return []byte(strconv.Quote(string(s)))
			}
			u := make([]uint16, 0, len(s)/2)
			for k := 0; k+1 < len(s); k += 2 {
				u = append(u, uint16(s[k])<<8|uint16(s[k+1]))
			}
			return []byte(strconv.Quote(string(utf16.Decode(u))))
		})
		fmt.Fprintf(&sb, "--- page %d\n%s\n", i+1, decoded)
	}
	return []byte(sb.String()), nil
}

func TestExportGolden(t *testing.T) {
	withDeterministicPDF(t)
	want := map[string]bool{}
	for _, c := range exportGoldenCases(t) {
		for _, f := range exportGoldenFormats {
			name := c.name + "." + f.ext + ".golden"
			want[name] = true
			t.Run(c.name+"/"+f.ext, func(t *testing.T) {
				got, err := f.render(c)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(exportGoldenDir, name)
				if *updateExportGolden {
					if err := os.MkdirAll(exportGoldenDir, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, got, 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				wantBytes, err := os.ReadFile(path) // #nosec G304 -- test golden
				if err != nil {
					t.Fatalf("%v (run with -update-export-golden to create it)", err)
				}
				// A CRLF checkout of an LF golden still compares equal; the
				// fixtures contain no CR of their own.
				wantBytes = bytes.ReplaceAll(wantBytes, []byte("\r\n"), []byte("\n"))
				if !bytes.Equal(wantBytes, got) {
					t.Errorf("%s output differs from %s (refresh with -update-export-golden only for an intended change):\n%s",
						f.ext, path, firstByteDiff(wantBytes, got))
				}
			})
		}
	}
	entries, err := os.ReadDir(exportGoldenDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !want[e.Name()] {
			if *updateExportGolden {
				_ = os.Remove(filepath.Join(exportGoldenDir, e.Name()))
				continue
			}
			t.Errorf("stale golden %s has no case (run with -update-export-golden to remove it)", e.Name())
		}
	}
}

// firstByteDiff shows the first differing line of two renderings.
func firstByteDiff(want, got []byte) string {
	w, g := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d\n- %q\n+ %q", i+1, wl, gl)
		}
	}
	return fmt.Sprintf("same lines, %d vs %d bytes", len(want), len(got))
}
