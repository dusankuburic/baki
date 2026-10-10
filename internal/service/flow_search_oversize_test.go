package service

import (
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// TestSearchFlow_OversizeCloudDocumentIndexIsNotCached: a cloud document
// above the provider's per-entry ceiling is resolved anew on every request,
// so an index cached for it could never be reused — searching it must leave
// the index cache untouched (Len and Cost), while a document the provider
// does cache gets its index cached exactly once.
func TestSearchFlow_OversizeCloudDocumentIndexIsNotCached(t *testing.T) {
	backend := testutil.NewFakeBackend()
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, nil, nil)
	idxCache := svc.idxCache

	// Few real blocks (a cheap index), but a block count the provider would
	// refuse to cache: the guard must act on the document's cost, not the
	// index's.
	oversize := parseFlowOfBlocks(t, "oversize", 3)
	oversize.Metadata.BlockCount = int(cloudDocCacheMaxEntry/docCostPerBlock) + 1
	if FlowDocumentCost(oversize) <= cloudDocCacheMaxEntry {
		t.Fatalf("premise: cost %d must exceed the ceiling %d", FlowDocumentCost(oversize), cloudDocCacheMaxEntry)
	}
	for i := 0; i < 2; i++ {
		res, err := svc.SearchFlow(oversize, models.SearchQuery{Text: "message", MaxResults: 10})
		if err != nil || len(res.Results) != 3 {
			t.Fatalf("search %d: results=%v err=%v, want the 3 message blocks", i, res, err)
		}
	}
	if idxCache.Len() != 0 || idxCache.Cost() != 0 {
		t.Errorf("searching an oversize document cached its index: len=%d cost=%d, want 0/0", idxCache.Len(), idxCache.Cost())
	}

	normal := parseFlowOfBlocks(t, "normal", 3)
	for i := 0; i < 2; i++ {
		if _, err := svc.SearchFlow(normal, models.SearchQuery{Text: "message", MaxResults: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if idxCache.Len() != 1 || idxCache.Cost() != searchIndexCost(svc.searchIndexFor(normal)) {
		t.Errorf("a cacheable document's index: len=%d cost=%d, want exactly one entry", idxCache.Len(), idxCache.Cost())
	}

	// Desktop: the provider holds the one current document regardless of
	// size, so its index is cached.
	local := NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), nil, nil, nil)
	if _, err := local.SearchFlow(oversize, models.SearchQuery{Text: "message"}); err != nil {
		t.Fatal(err)
	}
	if got := local.idxCache.Len(); got != 1 {
		t.Errorf("desktop search of a large document cached %d indexes, want 1", got)
	}
}
