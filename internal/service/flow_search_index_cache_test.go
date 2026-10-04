package service

import (
	"fmt"
	"strings"
	"testing"
	"weak"

	"pad-core/cache"
	"pad-core/models"
	"pad-core/parser"
)

// parseFlowOfBlocks parses a single-subflow flow of n message actions whose
// text differs per seed, so each parse is a distinct document.
func parseFlowOfBlocks(t *testing.T, seed string, n int) *models.FlowDocument {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("#Region \"Main\"\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "Display.ShowMessage Message: $'''%s message %d'''\n", seed, i)
	}
	sb.WriteString("#End Region\n")
	doc, err := parser.ParseText(sb.String(), seed+".txt", int64(sb.Len()))
	if err != nil {
		t.Fatalf("ParseText(%s): %v", seed, err)
	}
	if doc.Metadata.BlockCount != n {
		t.Fatalf("fixture rot: %s parsed to %d blocks, want %d", seed, doc.Metadata.BlockCount, n)
	}
	return doc
}

// TestSearchIndexCache_EvictsByBytes: cached search indexes are charged by
// block count against a byte budget — past it the least recently used index
// is rebuilt on its next search — and an index that cannot fit the budget is
// built per search rather than cached.
func TestSearchIndexCache_EvictsByBytes(t *testing.T) {
	docs := make([]*models.FlowDocument, 4)
	for i := range docs {
		docs[i] = parseFlowOfBlocks(t, fmt.Sprintf("flow%d", i), 20)
	}
	unit := int64(20) * searchIndexCostPerBlock
	svc := &FlowService{}
	idxCache := cache.NewSizedLRU(3*unit, cache.WithMaxEntries(maxSearchIndexCache))
	svc.idxCache = idxCache

	first := svc.searchIndexFor(docs[0])
	if svc.searchIndexFor(docs[0]) != first {
		t.Fatal("a repeat search on the same document rebuilt its index")
	}
	if got := idxCache.Cost(); got != unit {
		t.Fatalf("a 20-block index is charged %d, want %d (the cost model this test is built on)", got, unit)
	}
	svc.searchIndexFor(docs[1])
	svc.searchIndexFor(docs[2])
	if idxCache.Len() != 3 {
		t.Fatalf("cached %d indexes within budget, want 3", idxCache.Len())
	}
	svc.searchIndexFor(docs[3])
	if idxCache.Len() != 3 || idxCache.Cost() != 3*unit {
		t.Errorf("after a fourth index: %d cached costing %d, want 3 within the %d budget", idxCache.Len(), idxCache.Cost(), 3*unit)
	}
	if svc.searchIndexFor(docs[0]) == first {
		t.Error("the least recently used index should have been evicted and rebuilt")
	}
	for _, i := range []int{2, 3} {
		before := svc.searchIndexFor(docs[i])
		if svc.searchIndexFor(docs[i]) != before {
			t.Errorf("index %d within budget was not reused", i)
		}
	}

	// Oversize: four units in a three-unit budget.
	big := parseFlowOfBlocks(t, "big", 80)
	len0, cost0 := idxCache.Len(), idxCache.Cost()
	first, second := svc.searchIndexFor(big), svc.searchIndexFor(big)
	if first == second {
		t.Error("an index above the budget was cached")
	}
	if idxCache.Len() != len0 || idxCache.Cost() != cost0 {
		t.Errorf("an oversize index changed the cache: %d/%d → %d/%d", len0, cost0, idxCache.Len(), idxCache.Cost())
	}
}

// indexAndDrop builds (and caches) the index for a fresh document and returns
// only a weak reference to the document.
func indexAndDrop(t *testing.T, svc *FlowService, seed string) weak.Pointer[models.FlowDocument] {
	t.Helper()
	doc := parseFlowOfBlocks(t, seed, 50)
	svc.searchIndexFor(doc)
	return weak.Make(doc)
}

// TestSearchIndexCache_ReleasesTheDocument: the cached index must not keep
// its (weakly held) document alive — the whole point of holding the document
// weakly, defeated for as long as the index pinned every block of it.
func TestSearchIndexCache_ReleasesTheDocument(t *testing.T) {
	svc := NewFlowService(nil, nil, NewLocalDocumentProvider(), nil, nil, nil)
	wp := indexAndDrop(t, svc, "released")
	if got := svc.idxCache.(*cache.SizedLRU).Len(); got != 1 {
		t.Fatalf("index not cached: %d entries", got)
	}
	if !weakCollected(wp) {
		t.Error("the document stayed reachable while its index was cached: the index cache pins superseded documents")
	}
}
