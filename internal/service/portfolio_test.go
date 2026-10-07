package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

func TestAssemblePortfolio(t *testing.T) {
	docs := []*storageif.FlowDocument{
		{ID: "f1", Name: "Alpha", OwnerID: "u1"},
		{ID: "f2", Name: "Bravo", OwnerID: "u2"},
		{ID: "f3", Name: "Charlie", OwnerID: "u1"}, // never analyzed
	}
	now := time.Now()
	health := map[string]*storageif.HealthSnapshot{
		"f1": {HealthScore: 90, Errors: 0, Warnings: 1, AnalyzedAt: now},
		"f2": {HealthScore: 40, Errors: 3, Warnings: 2, AnalyzedAt: now},
	}
	names := map[string]string{"u1": "u1@example.com", "u2": "u2@example.com"}

	p := assemblePortfolio(docs, health, names, PortfolioSortRisk, 0, 0)

	if p.TotalFlows != 3 || p.AnalyzedFlows != 2 {
		t.Fatalf("totals: total=%d analyzed=%d, want 3/2", p.TotalFlows, p.AnalyzedFlows)
	}
	if p.AvgHealth != 65 { // (90+40)/2
		t.Errorf("avg health = %d, want 65", p.AvgHealth)
	}
	if p.Errors != 3 || p.Warnings != 3 {
		t.Errorf("rollup errors=%d warnings=%d, want 3/3", p.Errors, p.Warnings)
	}

	// Ranking: worst health first, unanalyzed last.
	if p.Entries[0].FlowID != "f2" {
		t.Errorf("entries[0] = %s, want f2 (worst health)", p.Entries[0].FlowID)
	}
	if p.Entries[1].FlowID != "f1" {
		t.Errorf("entries[1] = %s, want f1", p.Entries[1].FlowID)
	}
	if p.Entries[2].FlowID != "f3" || p.Entries[2].Analyzed {
		t.Errorf("entries[2] should be the unanalyzed f3, got %+v", p.Entries[2])
	}
	if p.Entries[2].AnalyzedAt != nil {
		t.Error("unanalyzed entry must have nil AnalyzedAt")
	}
	if p.Entries[0].OwnerName != "u2@example.com" {
		t.Errorf("owner name not resolved: %q", p.Entries[0].OwnerName)
	}
}

func TestAssemblePortfolio_SortAndLimit(t *testing.T) {
	earlier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	docs := []*storageif.FlowDocument{
		{ID: "f1", Name: "Delta"},
		{ID: "f2", Name: "Alpha"},
		{ID: "f3", Name: "Charlie"},
		{ID: "f4", Name: "Bravo"}, // never analyzed
	}
	health := map[string]*storageif.HealthSnapshot{
		"f1": {HealthScore: 90, Errors: 0, Warnings: 5, AnalyzedAt: earlier},
		"f2": {HealthScore: 40, Errors: 2, Warnings: 0, AnalyzedAt: earlier},
		"f3": {HealthScore: 70, Errors: 2, Warnings: 3, AnalyzedAt: later},
	}
	cases := []struct {
		sort  PortfolioSort
		limit int
		want  []string
	}{
		{PortfolioSortRisk, 0, []string{"f2", "f3", "f1", "f4"}},
		{PortfolioSortHealth, 0, []string{"f1", "f3", "f2", "f4"}},
		// Equal error counts break on warnings.
		{PortfolioSortErrors, 0, []string{"f3", "f2", "f1", "f4"}},
		// Name ignores analysis state entirely.
		{PortfolioSortName, 0, []string{"f2", "f4", "f3", "f1"}},
		// Equal timestamps fall back to name.
		{PortfolioSortAnalyzed, 0, []string{"f3", "f2", "f1", "f4"}},
		{PortfolioSortRisk, 2, []string{"f2", "f3"}},
		{PortfolioSortRisk, 10, []string{"f2", "f3", "f1", "f4"}},
	}
	for _, tc := range cases {
		p := assemblePortfolio(docs, health, map[string]string{}, tc.sort, tc.limit, 0)
		got := make([]string, len(p.Entries))
		for i, e := range p.Entries {
			got[i] = e.FlowID
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("sort=%s limit=%d: entries %v, want %v", tc.sort, tc.limit, got, tc.want)
		}
		// Trimming the entries must not shrink the rollup.
		if p.TotalFlows != 4 || p.AnalyzedFlows != 3 || p.Errors != 4 {
			t.Errorf("sort=%s limit=%d: totals %+v cover less than the whole fleet", tc.sort, tc.limit, p)
		}
	}
}

func TestParsePortfolioSort(t *testing.T) {
	if got, err := ParsePortfolioSort(""); err != nil || got != PortfolioSortRisk {
		t.Errorf("empty sort = %q, %v; want risk", got, err)
	}
	if got, err := ParsePortfolioSort("errors"); err != nil || got != PortfolioSortErrors {
		t.Errorf("errors sort = %q, %v", got, err)
	}
	if _, err := ParsePortfolioSort("updated_desc"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown sort err = %v, want ErrInvalidInput", err)
	}
}

func TestAssemblePortfolio_Empty(t *testing.T) {
	p := assemblePortfolio(nil, map[string]*storageif.HealthSnapshot{}, map[string]string{}, PortfolioSortRisk, 0, 0)
	if p.TotalFlows != 0 || p.AnalyzedFlows != 0 || p.AvgHealth != 0 {
		t.Errorf("empty portfolio should be zeroed, got %+v", p)
	}
	if p.Entries == nil {
		t.Error("Entries must be a non-nil empty slice")
	}
}

// TestBuildPortfolio_LocalModeIsEmpty pins the desktop guard: with no storage
// backend the portfolio used to panic on s.storage.LoadFlowHealthBatch (nil
// receiver) and surface as a 500 from GET /api/library/portfolio. Local mode
// must return an empty portfolio instead.
func TestBuildPortfolio_LocalModeIsEmpty(t *testing.T) {
	svc := NewLibraryService(nil, nil, nil, nil)
	p, err := svc.BuildPortfolio(context.Background(), "user-1", "", PortfolioSortRisk, 0)
	if err != nil {
		t.Fatalf("BuildPortfolio in local mode: %v", err)
	}
	if p == nil {
		t.Fatal("BuildPortfolio returned nil portfolio")
	}
	if p.TotalFlows != 0 || p.AnalyzedFlows != 0 || len(p.Entries) != 0 {
		t.Errorf("expected an empty portfolio, got %+v", p)
	}
	if p.Entries == nil {
		t.Error("Entries must be a non-nil empty slice so the JSON is [] rather than null")
	}
}

// TestAssemblePortfolio_ReportsTheWholeFleetWhenRankingIsCapped: the rollups
// cover the ranked flows only, so a fleet larger than the ranking cap must say
// so. TotalFlows used to be len(docs) — exactly the cap — which read as the
// whole fleet with averages over an arbitrary slice of it.
func TestAssemblePortfolio_ReportsTheWholeFleetWhenRankingIsCapped(t *testing.T) {
	docs := []*storageif.FlowDocument{
		{ID: "f1", Name: "One", OwnerID: "u1"},
		{ID: "f2", Name: "Two", OwnerID: "u1"},
	}
	health := map[string]*storageif.HealthSnapshot{
		"f1": {HealthScore: 40, Errors: 2},
		"f2": {HealthScore: 80},
	}

	p := assemblePortfolio(docs, health, map[string]string{}, PortfolioSortRisk, 0, 500)
	if p.RankedFlows != 2 || p.TotalFlows != 500 || !p.Truncated {
		t.Errorf("ranked=%d total=%d truncated=%v, want 2/500/true", p.RankedFlows, p.TotalFlows, p.Truncated)
	}
	if p.AnalyzedFlows != 2 || p.AvgHealth != 60 || p.Errors != 2 {
		t.Errorf("rollups must cover the ranked flows: %+v", p)
	}

	// A fleet that fits: no truncation, and the total is the ranked count.
	if p := assemblePortfolio(docs, health, map[string]string{}, PortfolioSortRisk, 0, 2); p.Truncated || p.TotalFlows != 2 {
		t.Errorf("total=%d truncated=%v, want 2/false", p.TotalFlows, p.Truncated)
	}
	// Unknown total (0): report the ranked count, as the local-mode and
	// direct-call paths do.
	if p := assemblePortfolio(docs, health, map[string]string{}, PortfolioSortRisk, 0, 0); p.Truncated || p.TotalFlows != 2 {
		t.Errorf("total=%d truncated=%v, want 2/false", p.TotalFlows, p.Truncated)
	}
}
