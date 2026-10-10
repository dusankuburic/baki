package service

import (
	"sync"
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
	svc.idxCache = newSearchIndexCache()
	return svc, doc
}

// searchIndexKey is the search-index cache's key of flowID.
func searchIndexKey(t *testing.T, flowID string) cache.Key {
	t.Helper()
	k, err := searchIndexes.Key(cache.Resource(flowID))
	if err != nil {
		t.Fatal(err)
	}
	return k
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
	// Fatal, not Skip: simpleFlow is a fixture in this file, not an
	// environment dependency. If it ever stops yielding blocks the parser has
	// regressed — skipping would retire this test silently at exactly the
	// moment it matters.
	if len(doc.Subflows) == 0 || len(doc.Subflows[0].Blocks) == 0 {
		t.Fatal("fixture parsed to zero blocks — parser regression, or simpleFlow changed")
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
		t.Fatal("fixture parsed to zero blocks — parser regression, or simpleFlow changed")
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
	svc := &FlowService{idxCache: cache.New[*searchIndexEntry](searchIndexes, 1<<30, cache.WithMaxEntries(2))}
	docs := map[string]*models.FlowDocument{}
	for _, id := range []string{"a", "b", "c"} {
		doc, err := parser.ParseText(simpleFlow, "test.txt", int64(len(simpleFlow)))
		if err != nil {
			t.Fatal(err)
		}
		doc.ID = id
		docs[id] = doc
	}
	cached := func(id string) bool {
		e, ok := svc.idxCache.Get(searchIndexKey(t, id))
		return ok && e.doc.Value() == docs[id]
	}

	svc.searchIndexFor(docs["a"])
	svc.searchIndexFor(docs["b"])
	// Touch "a" so "b" becomes least-recently-used, then overflow the cap.
	if !cached("a") {
		t.Fatal("expected a's index present")
	}
	svc.searchIndexFor(docs["c"])

	if cached("b") {
		t.Error("expected LRU to evict b after overflow, but it was present")
	}
	if !cached("a") {
		t.Error("expected recently-used a to survive")
	}
	if !cached("c") {
		t.Error("expected newly-added c to be present")
	}
}

// Concurrent cold searches for one flow must share a single index build
// (idxFlights): each build tokenizes the whole flow, and N racers would also
// overwrite each other's cache entry. Counted through the cache's statistics
// (no test in this package runs in parallel with it).
func TestFlowService_SearchIndex_ConcurrentMissBuildsOnce(t *testing.T) {
	svc, doc := makeTestDoc(t, simpleFlow)
	stores := func() uint64 { return searchIndexes.Info().Stats.Stores }
	before := stores()

	const racers = 24
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := svc.searchDoc(doc, models.SearchQuery{}, true); r == nil {
				t.Error("searchDoc returned nil results")
			}
		}()
	}
	wg.Wait()
	if got := stores() - before; got != 1 {
		t.Errorf("index cached %d times for %d concurrent cold searches, want 1", got, racers)
	}
}
