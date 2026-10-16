package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/export"
	"pad-core/models"
	"pad-core/parser"
)

func TestExportService_Export_NilDocAndReport(t *testing.T) {
	svc := NewExportService(NilNotifier{}, nil, nil)
	for _, id := range []string{"markdown", "html", "pdf"} {
		if _, _, err := svc.Export(id, nil, &models.AnalysisReport{}, ""); err == nil || err.Error() != "no flow loaded" {
			t.Errorf("%s: nil doc: %v", id, err)
		}
		if _, _, err := svc.Export(id, &models.FlowDocument{}, nil, ""); err == nil || !strings.Contains(err.Error(), "run analysis first") {
			t.Errorf("%s: nil report: %v", id, err)
		}
	}
}

// Export renders through the registry: the bytes are the format's, and a path (desktop) gets
// exactly the returned bytes.
func TestExportService_Export_RendersAndWrites(t *testing.T) {
	svc := NewExportService(NilNotifier{}, nil, nil)
	doc := &models.FlowDocument{ID: "test", Name: "Test Flow"}
	report := &models.AnalysisReport{FlowID: "test", FlowName: "Test Flow"}
	for _, id := range []string{"markdown", "html", "pdf"} {
		t.Run(id, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "report"+map[string]string{"markdown": ".md", "html": ".html", "pdf": ".pdf"}[id])
			content, f, err := svc.Export(id, doc, report, out)
			if err != nil {
				t.Fatal(err)
			}
			if f.ID != id || len(content) == 0 {
				t.Fatalf("format %q, %d bytes", f.ID, len(content))
			}
			if id != "pdf" { // PDF bytes carry the render time
				want, err := export.Bytes(id, export.Input{Report: report, Doc: doc}, export.Options{})
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(content, want) {
					t.Errorf("Export differs from the registry's %s rendering", id)
				}
			}
			written, err := os.ReadFile(out) // #nosec G304 -- test temp dir
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(written, content) {
				t.Error("file content should match returned content")
			}
		})
	}
}

func TestExportService_Export_RejectsABadPath(t *testing.T) {
	svc := NewExportService(NilNotifier{}, nil, nil)
	if _, _, err := svc.Export("markdown", &models.FlowDocument{}, &models.AnalysisReport{}, "report\x00.md"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("a path with a NUL byte: %v, want ErrInvalidPath", err)
	}
}

// Render stamps this build's version into SARIF (the init()-time global it replaces did the same),
// an explicit version wins, and an unknown format is invalid input.
func TestExportService_Render(t *testing.T) {
	driverVersion := func(b []byte) string {
		t.Helper()
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
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "9.8.7"

	var svc *ExportService // stateless: a nil service renders
	b, f, err := svc.Render("sarif", export.Input{}, export.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != "sarif" || f.MIME != "application/sarif+json" {
		t.Errorf("format = %+v", f)
	}
	if got := driverVersion(b); got != "9.8.7" {
		t.Errorf("tool.driver.version = %q, want the build version", got)
	}
	if b, _, _ = svc.Render("sarif", export.Input{}, export.Options{ToolVersion: "1.2.3"}); driverVersion(b) != "1.2.3" {
		t.Errorf("an explicit ToolVersion was overridden: %q", driverVersion(b))
	}

	if _, _, err := svc.Render("docx", export.Input{}, export.Options{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown format: %v, want ErrInvalidInput", err)
	}
}

// The save allowlist names only extensions of registered formats, so every file the findings
// panel saves is a format the server itself can produce.
func TestClientExportExts_AreRegisteredFormats(t *testing.T) {
	exts := map[string]string{}
	for _, f := range export.List() {
		exts[f.Ext] = f.ID
	}
	for ext := range clientExportExts {
		if _, ok := exts[ext]; !ok {
			t.Errorf("allowlisted %s is no registered format's extension", ext)
		}
	}
}

func TestExportService_CompareCurrentWith_NilDoc(t *testing.T) {
	svc := NewExportService(NilNotifier{}, nil, nil)
	_, err := svc.CompareCurrentWith(nil, "/some/path")
	if err == nil {
		t.Error("expected error for nil doc")
	}
}

func TestExportService_CompareCurrentWith_InvalidPath(t *testing.T) {
	svc := NewExportService(NilNotifier{}, nil, nil)
	doc := &models.FlowDocument{ID: "test"}
	_, err := svc.CompareCurrentWith(doc, "")
	if err == nil {
		t.Error("expected error for empty path")
	}
}

func TestExportService_CompareCurrentWith_FileNotFound(t *testing.T) {
	svc := NewExportService(NilNotifier{}, nil, nil)
	doc := &models.FlowDocument{ID: "test"}
	_, err := svc.CompareCurrentWith(doc, "/nonexistent/path/to/file.txt")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestExportService_CompareCurrentWith_ValidFile(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.txt")
	oldContent := "HTTP Get \"https://api.example.com\"\n"
	if err := os.WriteFile(oldPath, []byte(oldContent), 0644); err != nil {
		t.Fatal(err)
	}

	svc := NewExportService(NilNotifier{}, nil, nil)
	newDoc, err := parser.ParseText(oldContent, "old.txt", int64(len(oldContent)))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	diff, err := svc.CompareCurrentWith(newDoc, oldPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == nil {
		t.Error("expected non-nil diff")
	}
}

// TestExportService_Export_PathValidationBoundsTheWrite: the desktop write
// path must be absolute with a report extension — the native save dialog
// always returns that shape, and anything else is a caller (or a drive-by
// cross-origin POST) trying to plant a file somewhere interesting.
func TestExportService_Export_PathValidationBoundsTheWrite(t *testing.T) {
	svc := NewExportService(NilNotifier{}, nil, nil)
	doc := &models.FlowDocument{ID: "test", Name: "T"}
	report := &models.AnalysisReport{FlowID: "test"}

	dir := t.TempDir()
	for name, path := range map[string]string{
		"relative path":            "report.md",
		"wrong extension":          filepath.Join(dir, "report.bat"),
		"no extension":             filepath.Join(dir, "report"),
		"dotfile in the home root": filepath.Join(dir, ".bashrc"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := svc.Export("markdown", doc, report, path); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("Export path %q: err = %v, want ErrInvalidInput", path, err)
			}
		})
	}
	if _, _, err := svc.Export("markdown", doc, report, filepath.Join(dir, "ok.MD")); err != nil {
		t.Errorf("uppercase extension must be accepted: %v", err)
	}
}
