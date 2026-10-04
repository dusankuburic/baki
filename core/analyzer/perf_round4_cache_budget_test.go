package analyzer

import (
	"testing"

	"pad-core/models"
)

// reportWithFindings is a report with n (empty) findings: ReportCost charges
// per finding, so n is its only size input.
func reportWithFindings(id string, n int) *models.AnalysisReport {
	return &models.AnalysisReport{FlowID: id, Findings: make([]models.Finding, n)}
}

// TestReportCost_Model pins the cost model the caches charge against their
// budgets: a flat base plus 1 KiB per finding (deliberately ~2x the measured
// retained size, see TestAnalysisReport_RetainedBytesPerFinding).
func TestReportCost_Model(t *testing.T) {
	if got := ReportCost(nil); got != 0 {
		t.Errorf("ReportCost(nil) = %d, want 0", got)
	}
	if got := ReportCost(reportWithFindings("e", 0)); got != 4<<10 {
		t.Errorf("ReportCost(empty) = %d, want the 4 KiB base", got)
	}
	if got := ReportCost(reportWithFindings("f", 1000)); got != 4<<10+1000<<10 {
		t.Errorf("ReportCost(1000 findings) = %d, want 4 KiB + 1000 KiB", got)
	}
	if DefaultCacheBudget != 96<<20 {
		t.Errorf("DefaultCacheBudget = %d, want 96 MiB", DefaultCacheBudget)
	}
}

// TestAnalysisCache_ByteBudgetEvictsLeastRecentlyUsed: the cache was bounded
// by entry count only, so 50 finding-dense reports could pin hundreds of MiB.
// Inserting past the byte budget evicts least recently used entries by bytes
// (a lookup of the evicted flow misses), a Get keeps an entry alive, and
// invalidation refunds the bytes.
func TestAnalysisCache_ByteBudgetEvictsLeastRecentlyUsed(t *testing.T) {
	unit := ReportCost(reportWithFindings("", 100))
	c := NewAnalysisCacheSized(50, 3*unit) // the 50-entry cap is never the binding limit here
	for _, id := range []string{"a", "b", "c"} {
		c.Put(id, "h", reportWithFindings(id, 100))
	}
	if got := c.Bytes(); got != 3*unit {
		t.Fatalf("bytes after three puts = %d, want %d", got, 3*unit)
	}
	if c.Get("a", "h") == nil {
		t.Fatal("a missing before any eviction")
	}

	c.Put("d", "h", reportWithFindings("d", 100))
	if c.Get("b", "h") != nil {
		t.Error("least recently used entry b survived the budget; want it evicted")
	}
	for _, id := range []string{"a", "c", "d"} {
		if c.Get(id, "h") == nil {
			t.Errorf("entry %s evicted; want it kept (a was read, c and d are newer)", id)
		}
	}
	if n, b := len(c.AllReports()), c.Bytes(); n != 3 || b != 3*unit {
		t.Errorf("cache holds %d entries / %d bytes, want 3 / %d", n, b, 3*unit)
	}

	// A report worth two units evicts two entries (the two least recently
	// used: the reads above refreshed a, c, d in that order).
	c.Put("e", "h", reportWithFindings("e", 200))
	if c.Get("a", "h") != nil || c.Get("c", "h") != nil {
		t.Error("a and c should have been evicted to make room for e")
	}
	if c.Get("d", "h") == nil || c.Get("e", "h") == nil {
		t.Error("d and e should be cached")
	}
	wantBytes := unit + ReportCost(reportWithFindings("", 200))
	if got := c.Bytes(); got != wantBytes {
		t.Errorf("bytes = %d, want %d", got, wantBytes)
	}

	c.Invalidate("e")
	if got := c.Bytes(); got != unit {
		t.Errorf("bytes after Invalidate(e) = %d, want %d (refund)", got, unit)
	}
	c.Clear()
	if got := c.Bytes(); got != 0 {
		t.Errorf("bytes after Clear = %d, want 0", got)
	}
}

// TestAnalysisCache_OversizedReportServedNotStored: a report that cannot fit
// the budget alone is not retained (storing it would evict everything for one
// flow that re-analyzes on its next request anyway) and leaves the other
// entries untouched; one above half the budget is retained, displacing older
// entries as needed.
func TestAnalysisCache_OversizedReportServedNotStored(t *testing.T) {
	unit := ReportCost(reportWithFindings("", 50))
	budget := 4 * unit
	c := NewAnalysisCacheSized(50, budget)
	c.Put("a", "h", reportWithFindings("a", 50))
	c.Put("b", "h", reportWithFindings("b", 50))

	huge := reportWithFindings("huge", 400) // 404 KiB > 216 KiB budget
	if ReportCost(huge) <= budget {
		t.Fatalf("test setup: huge must exceed the budget (%d <= %d)", ReportCost(huge), budget)
	}
	c.Put("huge", "h", huge)
	if c.Get("huge", "h") != nil {
		t.Error("a report larger than the whole budget was stored")
	}
	if c.Get("a", "h") == nil || c.Get("b", "h") == nil {
		t.Error("an unstorable report evicted other entries")
	}
	if got := c.Bytes(); got != 2*unit {
		t.Errorf("bytes = %d, want %d", got, 2*unit)
	}

	big := reportWithFindings("big", 150) // 154 KiB: over half the budget, under all of it
	if cost := ReportCost(big); cost <= budget/2 || cost > budget {
		t.Fatalf("test setup: big must be in (budget/2, budget], got %d of %d", cost, budget)
	}
	c.Put("big", "h", big)
	if c.Get("big", "h") != big {
		t.Error("a report over half the budget must still be cached")
	}
	if c.Get("a", "h") != nil {
		t.Error("the least recently used entry a should have made room for big")
	}
	if c.Get("b", "h") == nil {
		t.Error("b still fits beside big and must be kept")
	}
}

// TestAnalysisCache_ReplacingAFlowRefundsItsBytes: PutWithPath replaces a
// flow's prior hash in place; the replaced entry's bytes are refunded.
func TestAnalysisCache_ReplacingAFlowRefundsItsBytes(t *testing.T) {
	c := NewAnalysisCache(10)
	c.Put("f", "h1", reportWithFindings("f", 100))
	c.Put("f", "h2", reportWithFindings("f", 10))
	if c.Get("f", "h1") != nil || c.Get("f", "h2") == nil {
		t.Fatal("PutWithPath no longer replaces a flow's prior hash")
	}
	if got, want := c.Bytes(), ReportCost(reportWithFindings("", 10)); got != want {
		t.Errorf("bytes = %d, want %d (the replaced report refunded)", got, want)
	}
}

// TestLookupCachedReport_ProbesWithoutAnalyzing: the service layer probes the
// cache before taking a large-analysis slot, so a hit never queues behind
// running walks. The probe must miss without storing anything, hit the exact
// report CachedAnalysis stored, and honour the rule set in the key.
func TestLookupCachedReport_ProbesWithoutAnalyzing(t *testing.T) {
	DefaultCache.Clear()
	t.Cleanup(DefaultCache.Clear)
	b := makeBlock("b1", "Set X", models.BlockTypeAction, "SetVariable.Set", 0)
	b.SubflowID = "sf1"
	doc := &models.FlowDocument{ID: "probe", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
	rules := AllRules()

	if LookupCachedReport(doc, rules, nil) != nil {
		t.Fatal("probe hit on an empty cache")
	}
	if n := len(DefaultCache.AllReports()); n != 0 {
		t.Fatalf("probe stored %d report(s)", n)
	}
	r := CachedAnalysis(doc, rules, nil, nil)
	if got := LookupCachedReport(doc, rules, nil); got != r {
		t.Errorf("probe after CachedAnalysis = %p, want the cached report %p", got, r)
	}
	if LookupCachedReport(doc, nil, nil) != nil {
		t.Error("probe with a different rule set must miss (the rule set is part of the key)")
	}
}
