package service

import (
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/models"
	"pad-analyzer/internal/parser"
)

func TestExportService_ExportMarkdown_NilDoc(t *testing.T) {
	svc := NewExportService(nil, NilNotifier{}, nil, nil)
	_, err := svc.ExportMarkdown(nil, &models.AnalysisReport{}, "")
	if err == nil {
		t.Error("expected error for nil doc")
	}
}

func TestExportService_ExportMarkdown_NilReport(t *testing.T) {
	svc := NewExportService(nil, NilNotifier{}, nil, nil)
	_, err := svc.ExportMarkdown(&models.FlowDocument{}, nil, "")
	if err == nil {
		t.Error("expected error for nil report")
	}
}

func TestExportService_ExportMarkdown_GeneratesContent(t *testing.T) {
	svc := NewExportService(nil, NilNotifier{}, nil, nil)

	doc := &models.FlowDocument{ID: "test", Name: "Test Flow"}
	report := &models.AnalysisReport{FlowID: "test", FlowName: "Test Flow"}

	content, err := svc.ExportMarkdown(doc, report, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(content) == 0 {
		t.Error("expected non-empty markdown content")
	}
}

func TestExportService_ExportMarkdown_WritesToFile(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "report.md")

	svc := NewExportService(nil, NilNotifier{}, nil, nil)
	doc := &models.FlowDocument{ID: "test", Name: "Test Flow"}
	report := &models.AnalysisReport{FlowID: "test", FlowName: "Test Flow"}

	content, err := svc.ExportMarkdown(doc, report, outPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	written, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(written) != string(content) {
		t.Error("file content should match returned content")
	}
}

func TestExportService_ExportPDF_NilDoc(t *testing.T) {
	svc := NewExportService(nil, NilNotifier{}, nil, nil)
	_, err := svc.ExportPDF(nil, &models.AnalysisReport{}, "")
	if err == nil {
		t.Error("expected error for nil doc")
	}
}

func TestExportService_ExportPDF_NilReport(t *testing.T) {
	svc := NewExportService(nil, NilNotifier{}, nil, nil)
	_, err := svc.ExportPDF(&models.FlowDocument{}, nil, "")
	if err == nil {
		t.Error("expected error for nil report")
	}
}

func TestExportService_CompareCurrentWith_NilDoc(t *testing.T) {
	svc := NewExportService(nil, NilNotifier{}, nil, nil)
	_, err := svc.CompareCurrentWith(nil, "/some/path")
	if err == nil {
		t.Error("expected error for nil doc")
	}
}

func TestExportService_CompareCurrentWith_InvalidPath(t *testing.T) {
	svc := NewExportService(nil, NilNotifier{}, nil, nil)
	doc := &models.FlowDocument{ID: "test"}
	_, err := svc.CompareCurrentWith(doc, "")
	if err == nil {
		t.Error("expected error for empty path")
	}
}

func TestExportService_CompareCurrentWith_FileNotFound(t *testing.T) {
	svc := NewExportService(nil, NilNotifier{}, nil, nil)
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

	svc := NewExportService(nil, NilNotifier{}, nil, nil)
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
