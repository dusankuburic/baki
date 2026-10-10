package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/analyzer"
	"pad-core/parser"
)

// TestLoadFlowFiles_CloudModeKeepsNoASTCache: in cloud mode (a storage
// backend is wired) an upload is parsed and persisted, never parked in the
// AST cache — the stored library is the source of truth there, and the cache
// entry (a second full copy of the document, held for 24 h) had no reader
// but an identical re-upload.
func TestLoadFlowFiles_CloudModeKeepsNoASTCache(t *testing.T) {
	astCache := NewASTCache(1 << 30)
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
	astCache := NewASTCache(1 << 30)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), nil, nil, astCache)
	ctx := context.Background()

	if _, err := svc.LoadFlowFiles(ctx, map[string]string{"Main.txt": uploadFlowText}, "Payroll", ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := astCache.Len(); got != 1 {
		t.Errorf("local-mode load cached %d document(s), want 1", got)
	}
}

// B6: the single-file key was the bytes alone, though the parse depends on the file name (the flow's name, the
// implicit subflow's, every subflow's SourceFile): opening b.txt with the bytes of an earlier a.txt showed the
// flow as "a". The name is part of the key now; reopening the same file still hits.
func TestLoadFlowFromPath_SameBytesUnderAnotherNameIsAnotherDocument(t *testing.T) {
	dir := t.TempDir()
	text := "SET X TO 1\nSET Y TO 2\n" // no region: the implicit subflow is named after the file
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, NewASTCache(1<<30))

	a, err := svc.LoadFlowFromPath(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.LoadFlowFromPath(filepath.Join(dir, "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "b" || len(b.Subflows) != 1 || b.Subflows[0].Name != "b" || b.Subflows[0].SourceFile != "b.txt" {
		t.Errorf("b.txt opened as %q with subflow %q from %q, want b's own parse (served a.txt's)", b.Name, b.Subflows[0].Name, b.Subflows[0].SourceFile)
	}
	if b.FilePath != filepath.Join(dir, "b.txt") || a.Name != "a" {
		t.Errorf("paths/names mixed up: a=%q, b at %q", a.Name, b.FilePath)
	}
	again, err := svc.LoadFlowFromPath(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Subflows[0].ID != a.Subflows[0].ID {
		t.Error("reopening the unchanged a.txt re-parsed it instead of hitting the cache")
	}
}

// A desktop "fix all" seeds the cache with its last parse, so the reload is a hit; that parse is now made under
// the file's name, so the seeded document is what a load of the written bytes parses (it used to be parsed under
// the flow's name: a file "my.flow.txt" reloaded as flow "my" with SourceFile "my.flow").
func TestApplyFixBatchResult_DesktopSeedIsTheFilesOwnParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "my.flow.txt")
	if err := os.WriteFile(path, []byte(redundantSetsSrc("Main", 2)), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, NewASTCache(1<<30))
	doc, err := svc.LoadFlowFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	before := astDocs.Info().Stats
	res, err := svc.ApplyFixBatchResult(context.Background(), doc, nil, 0)
	if err != nil || res.Applied == 0 || res.Doc == nil {
		t.Fatalf("fix all: %+v, %v", res, err)
	}
	after := astDocs.Info().Stats
	if after.Hits-before.Hits != 1 || after.Misses != before.Misses {
		t.Errorf("the reload after the fix: %d hits, %d misses; want the seeded parse (1 hit, 0 misses)", after.Hits-before.Hits, after.Misses-before.Misses)
	}
	if res.Doc.Name != "my.flow" || res.Doc.Subflows[0].SourceFile != "my.flow.txt" {
		t.Errorf("reloaded as %q from %q, want my.flow from my.flow.txt", res.Doc.Name, res.Doc.Subflows[0].SourceFile)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := parser.ParseText(string(data), "my.flow.txt", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if analyzer.FlowHash(fresh) != analyzer.FlowHash(res.Doc) {
		t.Error("the seeded document differs from a fresh parse of the written file")
	}
}
