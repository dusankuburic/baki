package di

import (
	"context"
	"testing"

	"pad-analyzer/internal/service"
	"pad-core/cache"
	"pad-core/models"
)

// TestProvideASTCache_IsByteBudgeted: the parsed-document cache is bounded by
// the heap its documents pin — 96 MiB, each charged by the memory-audit cost
// model — not by a count of 100 documents of any size.
func TestProvideASTCache_IsByteBudgeted(t *testing.T) {
	t.Parallel()

	c, err := ProvideASTCache()
	if err != nil {
		t.Fatalf("ProvideASTCache() error = %v", err)
	}
	sized, ok := c.(*cache.SizedLRU)
	if !ok {
		t.Fatalf("ProvideASTCache() = %T, want a byte-budgeted *cache.SizedLRU", c)
	}
	if got := sized.Budget(); got != 96<<20 {
		t.Errorf("budget = %d, want %d (96 MiB)", got, 96<<20)
	}
	ctx := context.Background()
	doc := &models.FlowDocument{Metadata: models.FlowMetadata{BlockCount: 1000}, Source: "x"}
	c.Set(ctx, "doc", doc, 0)
	if got, want := sized.Cost(), service.FlowDocumentCost(doc); got != want {
		t.Errorf("a 1000-block document is charged %d, want FlowDocumentCost = %d", got, want)
	}
	if _, ok := c.Get(ctx, "doc"); !ok {
		t.Error("a document within budget was not cached")
	}
	huge := &models.FlowDocument{Metadata: models.FlowMetadata{BlockCount: 200_000}} // ~195 MiB
	c.Set(ctx, "huge", huge, 0)
	if _, ok := c.Get(ctx, "huge"); ok {
		t.Error("a document larger than the whole budget was cached")
	}
	if _, ok := c.Get(ctx, "doc"); !ok {
		t.Error("an oversize document evicted a cached one")
	}
}
