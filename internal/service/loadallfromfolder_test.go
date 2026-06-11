package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadAllFromFolder_CollectsPerFileErrors is the D1 regression test: one
// valid flow plus one non-flow .txt must yield one doc and one load error
// (previously bad files were silently dropped).
func TestLoadAllFromFolder_CollectsPerFileErrors(t *testing.T) {
	dir := t.TempDir()
	good := "#Region \"Main\"\n    Display.ShowMessageBox Message: $'''hi'''\n#EndRegion\n"
	if err := os.WriteFile(filepath.Join(dir, "good.txt"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	// Empty file → parser yields no subflows → must be reported, not dropped.
	if err := os.WriteFile(filepath.Join(dir, "junk.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	svc := &FlowService{}
	docs, loadErrors, err := svc.LoadAllFromFolder(context.Background(), dir)
	if err != nil {
		t.Fatalf("LoadAllFromFolder: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("docs = %d, want 1", len(docs))
	}
	if msg, ok := loadErrors["junk.txt"]; !ok || msg == "" {
		t.Errorf("loadErrors = %v, want entry for junk.txt", loadErrors)
	}
}

// TestLoadAllFromFolder_AllBadFilesIsNotAnError verifies a folder of only
// broken files returns the errors rather than failing the whole call.
func TestLoadAllFromFolder_AllBadFilesIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "junk.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	svc := &FlowService{}
	docs, loadErrors, err := svc.LoadAllFromFolder(context.Background(), dir)
	if err != nil {
		t.Fatalf("want nil error when load errors exist, got %v", err)
	}
	if len(docs) != 0 || len(loadErrors) != 1 {
		t.Errorf("docs=%d loadErrors=%v, want 0 docs and 1 error", len(docs), loadErrors)
	}
}
