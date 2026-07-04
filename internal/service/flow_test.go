package service

import (
	"context"
	"testing"

	"pad-core/cache"
	"pad-core/models"
	"pad-core/parser"
)

// simpleFlow is minimal valid PAD content used as a test fixture.
const simpleFlow = `
SET MyVar TO 'hello'
File.OpenTextFile File: 'C:\data.txt' Encoding: File.TextFileEncoding.UTF8 ReadAs: File.FileReadAs.WholeText => FileContent
`

// twoSubflowFlow has two subflows: Main calls Helper.
const twoSubflowFlow = `
#Region "Main"
CALL Helper
#End Region
#Region "Helper"
SET Result TO 'done'
#End Region
`

// makeTestDoc parses text into a FlowDocument and returns both a FlowService and the parsed doc.
func makeTestDoc(t *testing.T, text string) (*FlowService, *models.FlowDocument) {
	t.Helper()
	doc, err := parser.ParseText(text, "test.txt", int64(len(text)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	svc := &FlowService{}
	svc.idxCache, _ = cache.NewLRUCache(maxSearchIndexCache)
	return svc, doc
}

func TestFlowService_FindBlockByID_empty_id(t *testing.T) {
	svc, doc := makeTestDoc(t, simpleFlow)
	if svc.FindBlockByID(doc, "") != nil {
		t.Fatal("expected nil for empty ID")
	}
}

func TestFlowService_FindBlockByID_nil_doc(t *testing.T) {
	svc := &FlowService{}
	if svc.FindBlockByID(nil, "any-id") != nil {
		t.Fatal("expected nil when doc is nil")
	}
}

func TestFlowService_FindBlockByID_found(t *testing.T) {
	svc, doc := makeTestDoc(t, simpleFlow)
	if len(doc.Subflows) == 0 || len(doc.Subflows[0].Blocks) == 0 {
		t.Skip("no blocks parsed")
	}
	firstBlock := &doc.Subflows[0].Blocks[0]
	found := svc.FindBlockByID(doc, firstBlock.ID)
	if found == nil {
		t.Fatalf("expected to find block %q", firstBlock.ID)
	}
	if found.ID != firstBlock.ID {
		t.Errorf("got block %q, want %q", found.ID, firstBlock.ID)
	}
}

func TestFlowService_FindBlockByID_unknown(t *testing.T) {
	svc, doc := makeTestDoc(t, simpleFlow)
	if svc.FindBlockByID(doc, "does-not-exist") != nil {
		t.Fatal("expected nil for unknown ID")
	}
}

func TestFlowService_FindSubflowForBlock_found(t *testing.T) {
	svc, doc := makeTestDoc(t, simpleFlow)
	if len(doc.Subflows) == 0 || len(doc.Subflows[0].Blocks) == 0 {
		t.Skip("no blocks parsed")
	}
	firstBlock := &doc.Subflows[0].Blocks[0]
	sf := svc.FindSubflowForBlock(doc, firstBlock.ID)
	if sf == nil {
		t.Fatalf("expected subflow for block %q", firstBlock.ID)
	}
}

func TestFlowService_FindSubflowForBlock_unknown(t *testing.T) {
	svc, doc := makeTestDoc(t, simpleFlow)
	if svc.FindSubflowForBlock(doc, "no-such-block") != nil {
		t.Fatal("expected nil for unknown block")
	}
}

func TestFlowService_GetSourceFiles_no_doc(t *testing.T) {
	svc := &FlowService{}
	files, err := svc.GetSourceFiles(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected empty, got %d", len(files))
	}
}

func TestFlowService_GetSourceFiles_returns_subflows(t *testing.T) {
	svc, doc := makeTestDoc(t, twoSubflowFlow)
	files, err := svc.GetSourceFiles(doc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected at least one source file entry")
	}
	for _, f := range files {
		if f.SubflowName == "" {
			t.Error("SubflowName must not be empty")
		}
	}
}

func TestFlowService_RecentFiles_nil_settings(t *testing.T) {
	svc := &FlowService{settings: nil}
	files, err := svc.RecentFiles()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files != nil {
		t.Errorf("expected nil files when no settings, got %v", files)
	}
}

// TestFlowService_SearchIndexCacheIsBounded confirms the search-index cache
// evicts once it reaches its cap, so a long-lived process can't grow it without
// limit (the OOM risk the production-readiness review flagged).
func TestFlowService_SearchIndexCacheIsBounded(t *testing.T) {
	svc := &FlowService{}
	svc.idxCache, _ = cache.NewLRUCache(2)
	ctx := context.Background()

	svc.idxCache.Set(ctx, "a", "a", 0)
	svc.idxCache.Set(ctx, "b", "b", 0)
	// Touch "a" so "b" becomes least-recently-used, then overflow the cap.
	if _, ok := svc.idxCache.Get(ctx, "a"); !ok {
		t.Fatal("expected key a present")
	}
	svc.idxCache.Set(ctx, "c", "c", 0)

	if _, ok := svc.idxCache.Get(ctx, "b"); ok {
		t.Error("expected LRU to evict b after overflow, but it was present")
	}
	if _, ok := svc.idxCache.Get(ctx, "a"); !ok {
		t.Error("expected recently-used a to survive")
	}
	if _, ok := svc.idxCache.Get(ctx, "c"); !ok {
		t.Error("expected newly-added c to be present")
	}
}
