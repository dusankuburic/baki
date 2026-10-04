package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/cache"
)

// TestLoadFlowFiles_CloudModeKeepsNoASTCache: in cloud mode (a storage
// backend is wired) an upload is parsed and persisted, never parked in the
// AST cache — the stored library is the source of truth there, and the cache
// entry (a second full copy of the document, held for 24 h) had no reader
// but an identical re-upload.
func TestLoadFlowFiles_CloudModeKeepsNoASTCache(t *testing.T) {
	astCache := cache.NewSizedLRU(1 << 30) // no cost func: one unit per entry
	backend := testutil.NewFakeBackend()
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, nil, astCache)
	ctx := context.Background()
	files := map[string]string{"Main.txt": uploadFlowText}

	doc, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if doc == nil || len(doc.Subflows) == 0 {
		t.Fatal("upload returned no document")
	}
	if got := astCache.Len(); got != 0 {
		t.Errorf("cloud-mode upload parked %d document(s) in the AST cache, want 0", got)
	}
	if _, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice"); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if got := astCache.Len(); got != 0 {
		t.Errorf("cloud-mode re-upload parked %d document(s) in the AST cache, want 0", got)
	}
}

// TestLoadFlowFiles_LocalModeCachesTheParse keeps the desktop shortcut: with
// no storage backend the parsed document is cached under its content hash.
func TestLoadFlowFiles_LocalModeCachesTheParse(t *testing.T) {
	astCache := cache.NewSizedLRU(1 << 30)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), nil, nil, astCache)
	ctx := context.Background()

	if _, err := svc.LoadFlowFiles(ctx, map[string]string{"Main.txt": uploadFlowText}, "Payroll", ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := astCache.Len(); got != 1 {
		t.Errorf("local-mode load cached %d document(s), want 1", got)
	}
}
