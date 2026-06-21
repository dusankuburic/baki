package service

import (
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

	p := assemblePortfolio(docs, health, names)

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

func TestAssemblePortfolio_Empty(t *testing.T) {
	p := assemblePortfolio(nil, map[string]*storageif.HealthSnapshot{}, map[string]string{})
	if p.TotalFlows != 0 || p.AnalyzedFlows != 0 || p.AvgHealth != 0 {
		t.Errorf("empty portfolio should be zeroed, got %+v", p)
	}
	if p.Entries == nil {
		t.Error("Entries must be a non-nil empty slice")
	}
}
