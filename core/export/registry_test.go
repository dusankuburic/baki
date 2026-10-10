package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pad-core/models"
)

// TestRegistry_Builtins pins the registered set, its order (the CLI help
// line keeps text, json, sarif, junit, csv, html first) and every field.
func TestRegistry_Builtins(t *testing.T) {
	want := []string{"json", "sarif", "junit", "csv", "html", "markdown", "pdf"}
	if got := IDs(); !slices.Equal(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	list := List()
	if len(list) != len(want) {
		t.Fatalf("List() has %d formats, want %d", len(list), len(want))
	}
	ids, exts, orders := map[string]bool{}, map[string]bool{}, map[int]bool{}
	for i, f := range list {
		if f.ID != want[i] {
			t.Errorf("List()[%d] = %q, want %q", i, f.ID, want[i])
		}
		if f.ID == "" || f.ID != strings.ToLower(f.ID) || ids[f.ID] {
			t.Errorf("%q: ID must be non-empty, lower-case and unique", f.ID)
		}
		if f.Label == "" || f.MIME == "" || f.Render == nil {
			t.Errorf("%q: Label, MIME and Render are required: %+v", f.ID, f)
		}
		if !strings.HasPrefix(f.Ext, ".") || len(f.Ext) < 2 || exts[f.Ext] {
			t.Errorf("%q: Ext %q must be a unique extension with the dot", f.ID, f.Ext)
		}
		if f.Order <= 0 || orders[f.Order] || (i > 0 && f.Order <= list[i-1].Order) {
			t.Errorf("%q: Order %d must be positive, unique and ascending", f.ID, f.Order)
		}
		if f.Binary != (f.ID == "pdf") {
			t.Errorf("%q: Binary = %t; only pdf is binary", f.ID, f.Binary)
		}
		ids[f.ID], exts[f.Ext], orders[f.Order] = true, true, true
		if got, ok := Lookup(f.ID); !ok || got.ID != f.ID || got.MIME != f.MIME {
			t.Errorf("Lookup(%q) = %+v, %t", f.ID, got, ok)
		}
	}
	// The wire values today's routes and downloads use.
	mimes := map[string]string{
		"json": "application/json", "sarif": "application/sarif+json", "junit": "application/xml; charset=utf-8",
		"csv": "text/csv; charset=utf-8", "html": "text/html; charset=utf-8",
		"markdown": "text/markdown; charset=utf-8", "pdf": "application/pdf",
	}
	extOf := map[string]string{
		"json": ".json", "sarif": ".sarif", "junit": ".xml", "csv": ".csv", "html": ".html", "markdown": ".md", "pdf": ".pdf",
	}
	for _, f := range list {
		if f.MIME != mimes[f.ID] || f.Ext != extOf[f.ID] {
			t.Errorf("%q: MIME %q Ext %q, want %q %q", f.ID, f.MIME, f.Ext, mimes[f.ID], extOf[f.ID])
		}
	}
	for _, id := range []string{"text", "SARIF", "md", ""} {
		if _, ok := Lookup(id); ok {
			t.Errorf("Lookup(%q) found a format", id)
		}
	}
	list[0].ID = "mutated"
	if IDs()[0] != "json" {
		t.Error("List() returned the registry's own slice")
	}
}

// cliJSONReport is cmd/bakicli's cliReport as it was before the registry.
type cliJSONReport struct {
	*models.AnalysisReport
	Policy *models.PolicyResult `json:"policy,omitempty"`
}

// legacyBytes is what each caller wrote before the registry: the ReportTo*
// output, plus the newline every caller appended to SARIF and JUnit, and the
// CLI's json.Encoder for json.
func legacyBytes(t *testing.T, id string, c exportGoldenCase) []byte {
	t.Helper()
	var b []byte
	var err error
	switch id {
	case "json":
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		err = enc.Encode(cliJSONReport{AnalysisReport: c.report, Policy: c.opts.Policy})
		b = buf.Bytes()
	case "sarif":
		b, err = ReportToSARIFWithOptions(c.report, c.doc, c.opts)
		b = append(b, '\n')
	case "junit":
		b, err = ReportToJUnitWithOptions(c.report, c.doc, c.opts)
		b = append(b, '\n')
	case "csv":
		b, err = ReportToCSV(c.report, c.doc)
	case "html":
		b = []byte(ReportToHTML(c.report, c.doc))
	case "markdown":
		b = []byte(ReportToMarkdown(c.report, c.doc))
	case "pdf":
		b, err = ReportToPDF(c.report, c.doc)
	default:
		t.Fatalf("no legacy renderer for %q: add the new format here", id)
	}
	if err != nil {
		t.Fatalf("%s legacy render: %v", id, err)
	}
	return b
}

// TestRegistry_BytesMatchLegacy proves the registry renders exactly what the
// callers wrote before it, through both Bytes and Render, for every format.
func TestRegistry_BytesMatchLegacy(t *testing.T) {
	withDeterministicPDF(t)
	for _, c := range exportGoldenCases(t) {
		in := Input{Report: c.report, Doc: c.doc}
		for _, f := range List() {
			t.Run(c.name+"/"+f.ID, func(t *testing.T) {
				want := legacyBytes(t, f.ID, c)
				got, err := Bytes(f.ID, in, c.opts)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("Bytes differs from the legacy output:\n%s", firstByteDiff(want, got))
				}
				var buf bytes.Buffer
				if err := f.Render(&buf, in, c.opts); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(buf.Bytes(), want) {
					t.Fatalf("Render differs from the legacy output:\n%s", firstByteDiff(want, buf.Bytes()))
				}
				if !f.Binary && !bytes.HasSuffix(got, []byte("\n")) {
					t.Error("a text format must end with a newline")
				}
			})
		}
	}
}

// TestRegistry_BytesMatchGoldens renders w0's export goldens through the
// registry: the files are the pre-registry bytes, minus the newline callers
// appended to SARIF and JUnit.
func TestRegistry_BytesMatchGoldens(t *testing.T) {
	withDeterministicPDF(t)
	goldenExt := map[string]string{"markdown": "md"}
	for _, c := range exportGoldenCases(t) {
		for _, f := range List() {
			ext := f.ID
			if e, ok := goldenExt[f.ID]; ok {
				ext = e
			}
			path := filepath.Join(exportGoldenDir, c.name+"."+ext+".golden")
			want, err := os.ReadFile(path) // #nosec G304 -- test golden
			if err != nil {
				t.Fatalf("%s: %v", f.ID, err)
			}
			want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
			got, err := Bytes(f.ID, Input{Report: c.report, Doc: c.doc}, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			switch f.ID {
			case "sarif", "junit":
				got = bytes.TrimSuffix(got, []byte("\n"))
			case "pdf":
				if got, err = pdfGoldenText(got); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: registry output differs from %s:\n%s", f.ID, path, firstByteDiff(want, got))
			}
		}
	}
}

func TestRegistry_ToolVersion(t *testing.T) {
	version := func(opts Options) string {
		t.Helper()
		b, err := Bytes("sarif", Input{}, opts)
		if err != nil {
			t.Fatal(err)
		}
		var log struct {
			Runs []struct {
				Tool struct {
					Driver struct {
						Version string `json:"version"`
					} `json:"driver"`
				} `json:"tool"`
			} `json:"runs"`
		}
		if err := json.Unmarshal(b, &log); err != nil {
			t.Fatal(err)
		}
		return log.Runs[0].Tool.Driver.Version
	}
	if got := version(Options{}); got != "0.1.0" {
		t.Errorf("zero Options: version %q, want the default 0.1.0", got)
	}
	if got := version(Options{ToolVersion: "1.4.2"}); got != "1.4.2" {
		t.Errorf("Options.ToolVersion ignored: %q", got)
	}
	// One render's version is not a process setting: the next zero Options
	// gets the default again.
	if got := version(Options{}); got != "0.1.0" {
		t.Errorf("zero Options after a versioned render: %q, want 0.1.0", got)
	}
}

func TestRegistry_NilInput(t *testing.T) {
	for _, f := range List() {
		if _, err := f.Bytes(Input{}, Options{}); err != nil {
			t.Errorf("%s: nil report: %v", f.ID, err)
		}
	}
}

func TestBytes_UnknownFormat(t *testing.T) {
	_, err := Bytes("docx", Input{}, Options{})
	if !errors.Is(err, ErrUnknownFormat) || !strings.Contains(err.Error(), `"docx"`) {
		t.Fatalf("err = %v, want ErrUnknownFormat naming the id", err)
	}
}

// A format registered from outside the package writes its document into the
// Bytes sink.
func TestFormatBytes_BuffersRender(t *testing.T) {
	f := Format{ID: "x", Render: func(w io.Writer, in Input, _ Options) error {
		_, err := io.WriteString(w, in.Report.FlowID)
		return err
	}}
	got, err := f.Bytes(Input{Report: &models.AnalysisReport{FlowID: "flow-1"}}, Options{})
	if err != nil || string(got) != "flow-1" {
		t.Fatalf("Bytes = %q, %v", got, err)
	}
	f.Render = func(io.Writer, Input, Options) error { return errors.New("boom") }
	if _, err := f.Bytes(Input{}, Options{}); err == nil {
		t.Fatal("a Render error was dropped")
	}
}

// Bytes runs the Format's own Render: one replaced on a looked-up copy (a
// test stub, an instrumenting wrapper) is not bypassed by the built-ins'
// in-memory path, and a wrapper's own writes are kept.
func TestFormatBytes_FollowsReplacedRender(t *testing.T) {
	in := Input{Report: &models.AnalysisReport{FlowID: "flow-1"}}
	f, _ := Lookup("sarif")
	f.Render = func(io.Writer, Input, Options) error { return errors.New("stubbed failure") }
	if got, err := f.Bytes(in, Options{}); err == nil {
		t.Fatalf("Bytes ignored the replaced Render: %d bytes, nil error", len(got))
	}

	csv, _ := Lookup("csv")
	f.Render = csv.Render
	want, err := Bytes("csv", in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.Bytes(in, Options{}); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Bytes with csv's Render = %q, %v; want the csv document", got, err)
	}

	orig, _ := Lookup("sarif")
	f.Render = func(w io.Writer, in Input, opts Options) error {
		if _, err := io.WriteString(w, "prefix:"); err != nil {
			return err
		}
		return orig.Render(w, in, opts)
	}
	plain, err := orig.Bytes(in, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.Bytes(in, Options{}); err != nil || string(got) != "prefix:"+string(plain) {
		t.Fatalf("a wrapping Render lost bytes: %q, %v", got, err)
	}
}

func TestRegister_Rejects(t *testing.T) {
	render := func(io.Writer, Input, Options) error { return nil }
	valid := Format{ID: "yaml", Label: "YAML", MIME: "application/yaml", Ext: ".yaml", Order: 5, Render: render}
	cases := map[string]func(f *Format){
		"empty ID":       func(f *Format) { f.ID = "" },
		"upper-case ID":  func(f *Format) { f.ID = "YAML" },
		"ID with space":  func(f *Format) { f.ID = "ya ml" },
		"reserved text":  func(f *Format) { f.ID = "text" },
		"no label":       func(f *Format) { f.Label = "" },
		"no MIME":        func(f *Format) { f.MIME = "" },
		"ext no dot":     func(f *Format) { f.Ext = "yaml" },
		"ext only dot":   func(f *Format) { f.Ext = "." },
		"ext upper-case": func(f *Format) { f.Ext = ".YAML" },
		"ext with slash": func(f *Format) { f.Ext = "./x" },
		"zero order":     func(f *Format) { f.Order = 0 },
		"nil Render":     func(f *Format) { f.Render = nil },
		"duplicate ID":   func(f *Format) { f.Ext, f.Order = ".yml", 6 },
		"duplicate Ext":  func(f *Format) { f.ID, f.Order = "yml", 6 },
		"duplicate Ord":  func(f *Format) { f.ID, f.Ext = "yml", ".yml" },
	}
	// The duplicate cases differ from valid in one field only, so each must
	// fail on its own check.
	wantMsg := map[string]string{
		"duplicate ID":  "duplicate ID",
		"duplicate Ext": `Ext .yaml is taken by "yaml"`,
		"duplicate Ord": `Order 5 is taken by "yaml"`,
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := &registry{byID: map[string]Format{}}
			if strings.HasPrefix(name, "duplicate") {
				r.register(valid)
			}
			f := valid
			mutate(&f)
			defer func() {
				p := recover()
				if p == nil {
					t.Errorf("register(%+v) did not panic", f)
				} else if msg := fmt.Sprint(p); !strings.Contains(msg, wantMsg[name]) {
					t.Errorf("panic %q, want it to name %q", msg, wantMsg[name])
				}
			}()
			r.register(f)
		})
	}

	t.Run("after first read", func(t *testing.T) {
		r := &registry{byID: map[string]Format{}}
		r.register(valid)
		r.sealed.Store(true)
		defer func() {
			if recover() == nil {
				t.Error("register after the registry was read did not panic")
			}
		}()
		other := valid
		other.ID, other.Order = "toml", 7
		r.register(other)
	})
}
