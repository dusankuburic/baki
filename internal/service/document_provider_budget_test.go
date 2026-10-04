package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// seedFlowWithBlocks stores a flow of exactly blocks top-level blocks and the
// given raw source, so its resolved cost is FlowDocumentCost-predictable:
// docCostBase + blocks × docCostPerBlock + len(source).
func seedFlowWithBlocks(t *testing.T, backend *testutil.FakeBackend, id string, blocks int, source string) {
	t.Helper()
	sf := models.Subflow{ID: id + "-sf", Name: "Main"}
	for i := 0; i < blocks; i++ {
		sf.Blocks = append(sf.Blocks, models.Block{
			ID: fmt.Sprintf("%s-b%d", id, i), Name: fmt.Sprintf("Show message %d", i), Type: models.BlockTypeAction,
			RawType: "Display.ShowMessage", Children: []models.Block{},
		})
	}
	doc := &models.FlowDocument{ID: id, Name: "Flow " + id, Subflows: []models.Subflow{sf}, Metadata: models.FlowMetadata{BlockCount: blocks}}
	content, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	if err := backend.SaveFlow(context.Background(), &interfaces.FlowDocument{
		ID: id, Name: doc.Name, Content: content, Source: source, OwnerID: "u1",
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestCloudDocumentProvider_EvictsByBytes: the document cache is bounded by
// the heap its documents pin, not by their number — once the summed cost
// exceeds the budget the least recently used document goes (its next resolve
// is a backend load again) while the ones within budget keep being served
// from the cache.
func TestCloudDocumentProvider_EvictsByBytes(t *testing.T) {
	backend := testutil.NewFakeBackend()
	for _, id := range []string{"f1", "f2", "f3", "f4"} {
		seedFlowWithBlocks(t, backend, id, 4, "")
	}
	unit := docCostBase + 4*docCostPerBlock // one resolved 4-block document
	p := newCloudDocumentProviderWithBudget(backend, 3*unit, 2*unit)
	ctx := context.Background()

	resolve := func(id string) {
		t.Helper()
		if _, err := p.ResolveDoc(ctx, id); err != nil {
			t.Fatalf("resolve %s: %v", id, err)
		}
	}
	loads := func() int64 { return backend.LoadFlowCalls.Load() }

	resolve("f1")
	if got := p.cache.Cost(); got != unit {
		t.Fatalf("one 4-block document is charged %d, want %d (the cost model this test is built on)", got, unit)
	}
	resolve("f2")
	resolve("f3")
	if p.cache.Len() != 3 || loads() != 3 {
		t.Fatalf("after three resolves within budget: cached=%d loads=%d, want 3/3", p.cache.Len(), loads())
	}
	// Over budget: f1 (least recently used) is evicted for f4.
	resolve("f4")
	if p.cache.Len() != 3 || p.cache.Cost() != 3*unit {
		t.Errorf("after the fourth document: cached=%d cost=%d, want 3 documents within the %d budget", p.cache.Len(), p.cache.Cost(), 3*unit)
	}
	resolve("f3")
	resolve("f4")
	if loads() != 4 {
		t.Errorf("documents within budget must still be served from the cache: %d loads, want 4", loads())
	}
	resolve("f1")
	if loads() != 5 {
		t.Errorf("the evicted document must be loaded from the backend again: %d loads, want 5", loads())
	}
	// ... which in turn evicted f2, the least recently used by then.
	resolve("f2")
	if loads() != 6 {
		t.Errorf("after f1 re-entered, f2 should have been evicted: %d loads, want 6", loads())
	}
	if p.cache.Len() != 3 {
		t.Errorf("cached=%d, want 3", p.cache.Len())
	}
}

// TestCloudDocumentProvider_OversizeDocumentIsServedNotCached: a document
// above the per-entry ceiling is resolved per request — every resolve is a
// backend load — and never displaces the cached ones.
func TestCloudDocumentProvider_OversizeDocumentIsServedNotCached(t *testing.T) {
	backend := testutil.NewFakeBackend()
	seedFlowWithBlocks(t, backend, "small", 4, "")
	seedFlowWithBlocks(t, backend, "big", 12, "")
	unit := docCostBase + 4*docCostPerBlock
	p := newCloudDocumentProviderWithBudget(backend, 10*unit, 2*unit) // big costs ~2.6 units
	ctx := context.Background()

	if _, err := p.ResolveDoc(ctx, "small"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		doc, err := p.ResolveDoc(ctx, "big")
		if err != nil {
			t.Fatalf("resolve big: %v", err)
		}
		if len(doc.BlocksByID) != 12 {
			t.Fatalf("big resolved to %d blocks, want 12", len(doc.BlocksByID))
		}
	}
	if got := backend.LoadFlowCalls.Load(); got != 3 {
		t.Errorf("an oversize document must be loaded on every resolve: %d loads, want 3 (small + big twice)", got)
	}
	if p.cache.Len() != 1 || p.cache.Cost() != unit {
		t.Errorf("cache holds %d entries costing %d, want only the small document (%d)", p.cache.Len(), p.cache.Cost(), unit)
	}
	if _, err := p.ResolveDoc(ctx, "small"); err != nil {
		t.Fatal(err)
	}
	if got := backend.LoadFlowCalls.Load(); got != 3 {
		t.Errorf("the small document should still be cached: %d loads, want 3", got)
	}
}

// TestCloudDocumentProvider_UncachedResolveLeavesTheCacheAlone: the library
// search's resolve path serves a current cached document (reporting so) but
// never inserts what it loads.
func TestCloudDocumentProvider_UncachedResolveLeavesTheCacheAlone(t *testing.T) {
	backend := testutil.NewFakeBackend()
	seedFlowWithBlocks(t, backend, "hot", 4, "")
	seedFlowWithBlocks(t, backend, "scan", 4, "")
	p := NewCloudDocumentProvider(backend)
	ctx := context.Background()

	if _, err := p.ResolveDoc(ctx, "hot"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		doc, cached, err := p.ResolveDocUncached(ctx, "scan")
		if err != nil || doc == nil || doc.ID != "scan" {
			t.Fatalf("uncached resolve: doc=%v cached=%v err=%v", doc, cached, err)
		}
		if cached {
			t.Error("an uncached load reported itself as cached")
		}
	}
	if got := backend.LoadFlowCalls.Load(); got != 3 {
		t.Errorf("two uncached resolves must be two backend loads: %d loads in total, want 3", got)
	}
	if p.cache.Len() != 1 {
		t.Errorf("the uncached resolve inserted into the cache: %d entries, want 1 (hot)", p.cache.Len())
	}
	doc, cached, err := p.ResolveDocUncached(ctx, "hot")
	if err != nil || doc == nil || !cached {
		t.Errorf("a current cached document must be served from the cache: doc=%v cached=%v err=%v", doc, cached, err)
	}
	if got := backend.LoadFlowCalls.Load(); got != 3 {
		t.Errorf("serving the cached document loaded from the backend: %d loads, want 3", got)
	}
	// The version check still governs the uncached path.
	cur, _ := backend.LoadFlow(ctx, "hot")
	cur.Name = "hot (edited)"
	if err := backend.SaveFlow(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, cached, _ := p.ResolveDocUncached(ctx, "hot"); cached {
		t.Error("a stale cached document was served after a version bump")
	}
}

// TestFlowDocumentCost pins the cost model: blocks × 1 KiB + the source, with
// the block count taken from the lookup map or the metadata, whichever is
// larger.
func TestFlowDocumentCost(t *testing.T) {
	if got := FlowDocumentCost(nil); got != 0 {
		t.Errorf("nil document costs %d", got)
	}
	byMeta := &models.FlowDocument{Metadata: models.FlowMetadata{BlockCount: 10}, Source: "0123456789"}
	if got, want := FlowDocumentCost(byMeta), docCostBase+10*docCostPerBlock+10; got != want {
		t.Errorf("metadata-counted document costs %d, want %d", got, want)
	}
	byMap := &models.FlowDocument{Metadata: models.FlowMetadata{BlockCount: 1}}
	byMap.BlocksByID = map[string]*models.Block{"a": {}, "b": {}, "c": {}}
	if got, want := FlowDocumentCost(byMap), docCostBase+3*docCostPerBlock; got != want {
		t.Errorf("map-counted document costs %d, want %d (the larger count wins)", got, want)
	}
	if cloudDocCacheMaxEntry >= cloudDocCacheBudget {
		t.Errorf("per-entry ceiling %d must stay below the budget %d", cloudDocCacheMaxEntry, cloudDocCacheBudget)
	}
}
