package di

import (
	"testing"

	"pad-analyzer/internal/service"
	"pad-core/models"
)

// TestProvideASTCache_IsByteBudgeted: the parsed-document cache is bounded by
// the heap its documents pin — 96 MiB, each charged by the memory-audit cost
// model — not by a count of 100 documents of any size.
func TestProvideASTCache_IsByteBudgeted(t *testing.T) {
	t.Parallel()

	c := ProvideASTCache()
	if got := c.Budget(); got != 96<<20 {
		t.Errorf("budget = %d, want %d (96 MiB)", got, 96<<20)
	}
	if got := c.Spec().Name(); got != "flow.ast" {
		t.Errorf("spec = %q, want the declared flow.ast", got)
	}
	// The service charges each document FlowDocumentCost; the cache must hold one within budget and refuse one
	// larger than the whole budget without evicting the rest.
	doc := &models.FlowDocument{Metadata: models.FlowMetadata{BlockCount: 1000}, Source: "x"}
	huge := &models.FlowDocument{Metadata: models.FlowMetadata{BlockCount: 200_000}} // ~195 MiB
	if service.FlowDocumentCost(doc) > c.MaxEntryCost() {
		t.Errorf("a 1000-block document (%d) exceeds the per-entry ceiling %d", service.FlowDocumentCost(doc), c.MaxEntryCost())
	}
	if service.FlowDocumentCost(huge) <= c.MaxEntryCost() {
		t.Errorf("a ~195 MiB document (%d) fits under the per-entry ceiling %d", service.FlowDocumentCost(huge), c.MaxEntryCost())
	}
}
