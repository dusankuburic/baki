package analyzer

import (
	"fmt"
	"testing"

	"pad-core/models"
)

// TestAnalysisCache_GetRefreshesRecency: eviction drops the least recently
// USED entry. Get never refreshed recency, so the cache was FIFO: a flow read
// on every request was evicted as soon as maxSize newer flows were analyzed.
func TestAnalysisCache_GetRefreshesRecency(t *testing.T) {
	c := NewAnalysisCache(3)
	for _, id := range []string{"a", "b", "c"} {
		c.Put(id, "h", &models.AnalysisReport{FlowID: id})
	}
	if c.Get("a", "h") == nil {
		t.Fatal("a missing before eviction")
	}
	c.Put("d", "h", &models.AnalysisReport{FlowID: "d"})

	if c.Get("a", "h") == nil {
		t.Error("recently read entry a was evicted (FIFO, not LRU)")
	}
	if c.Get("b", "h") != nil {
		t.Error("least recently used entry b survived; want it evicted")
	}
	for _, id := range []string{"c", "d"} {
		if c.Get(id, "h") == nil {
			t.Errorf("entry %s evicted unexpectedly", id)
		}
	}
	if n := len(c.AllReports()); n != 3 {
		t.Errorf("cache holds %d entries, want the cap of 3", n)
	}
}

// TestAnalysisCache_DefaultCapUnchanged: DefaultCache keeps its 50-entry cap
// and PutWithPath still replaces a flow's prior hash in place.
func TestAnalysisCache_DefaultCapUnchanged(t *testing.T) {
	c := NewAnalysisCache(0)
	for i := 0; i < 60; i++ {
		c.Put(fmt.Sprintf("f%d", i), "h", &models.AnalysisReport{})
	}
	if n := len(c.AllReports()); n != 50 {
		t.Fatalf("cache holds %d entries, want 50", n)
	}
	c.Put("f59", "h2", &models.AnalysisReport{})
	if c.Get("f59", "h") != nil || c.Get("f59", "h2") == nil {
		t.Fatal("PutWithPath no longer replaces a flow's prior hash")
	}
	if n := len(c.AllReports()); n != 50 {
		t.Fatalf("re-put of an existing flow changed the size to %d", n)
	}
}
