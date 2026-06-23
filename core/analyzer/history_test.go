package analyzer

import (
	"os"
	"path/filepath"
	"testing"

	"pad-core/models"
)

func TestHistoryStore_RecordAndLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewHistoryStore(dir)

	report := &models.AnalysisReport{
		FlowID: "flow-1",
		Stats: models.AnalysisStats{
			Errors:   2,
			Warnings: 3,
			Info:     1,
		},
		DurationMs: 150,
	}
	doc := &models.FlowDocument{ID: "flow-1", Name: "Test Flow"}

	store.Record("flow-1", report, doc)

	snapshots := store.Load("flow-1")
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}
	s := snapshots[0]
	if s.FlowID != "flow-1" {
		t.Errorf("expected flow-1, got %s", s.FlowID)
	}
	if s.Errors != 2 {
		t.Errorf("expected 2 errors, got %d", s.Errors)
	}
	if s.Warnings != 3 {
		t.Errorf("expected 3 warnings, got %d", s.Warnings)
	}
	if s.HealthScore != 100 {
		t.Errorf("expected default health 100, got %d", s.HealthScore)
	}
}

func TestHistoryStore_RecordWithMetrics(t *testing.T) {
	dir := t.TempDir()
	store := NewHistoryStore(dir)

	report := &models.AnalysisReport{
		FlowID:  "flow-1",
		Metrics: &models.FlowMetrics{HealthScore: 72},
	}
	doc := &models.FlowDocument{ID: "flow-1"}

	store.Record("flow-1", report, doc)

	snapshots := store.Load("flow-1")
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}
	if snapshots[0].HealthScore != 72 {
		t.Errorf("expected health score 72, got %d", snapshots[0].HealthScore)
	}
}

func TestHistoryStore_DuplicateSnapshotSkipped(t *testing.T) {
	dir := t.TempDir()
	store := NewHistoryStore(dir)

	report := &models.AnalysisReport{FlowID: "flow-1"}
	doc := &models.FlowDocument{ID: "flow-1"}

	store.Record("flow-1", report, doc)
	store.Record("flow-1", report, doc)
	store.Record("flow-1", report, doc)

	snapshots := store.Load("flow-1")
	if len(snapshots) != 1 {
		t.Errorf("expected 1 snapshot (duplicates skipped), got %d", len(snapshots))
	}
}

func TestHistoryStore_DifferentSnapshotsAppended(t *testing.T) {
	dir := t.TempDir()
	store := NewHistoryStore(dir)

	doc := &models.FlowDocument{ID: "flow-1"}

	store.Record("flow-1", &models.AnalysisReport{
		FlowID: "flow-1",
		Stats:  models.AnalysisStats{Errors: 1},
	}, doc)
	store.Record("flow-1", &models.AnalysisReport{
		FlowID: "flow-1",
		Stats:  models.AnalysisStats{Errors: 2},
	}, doc)

	snapshots := store.Load("flow-1")
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}
	if snapshots[0].Errors != 1 {
		t.Errorf("expected first snapshot errors=1, got %d", snapshots[0].Errors)
	}
	if snapshots[1].Errors != 2 {
		t.Errorf("expected second snapshot errors=2, got %d", snapshots[1].Errors)
	}
}

func TestHistoryStore_MaxPerFlowEnforced(t *testing.T) {
	dir := t.TempDir()
	store := NewHistoryStore(dir)
	store.maxPerFlow = 3

	doc := &models.FlowDocument{ID: "flow-1"}

	for i := 0; i < 5; i++ {
		store.Record("flow-1", &models.AnalysisReport{
			FlowID: "flow-1",
			Stats:  models.AnalysisStats{Errors: i},
		}, doc)
	}

	snapshots := store.Load("flow-1")
	if len(snapshots) != 3 {
		t.Fatalf("expected 3 snapshots (trimmed), got %d", len(snapshots))
	}
	if snapshots[0].Errors != 2 {
		t.Errorf("expected oldest snapshot errors=2, got %d", snapshots[0].Errors)
	}
}

func TestHistoryStore_EmptyDirReturnsNil(t *testing.T) {
	store := NewHistoryStore("")
	if snapshots := store.Load("flow-1"); snapshots != nil {
		t.Errorf("expected nil for empty dir, got %v", snapshots)
	}
}

func TestHistoryStore_RecordEmptyDirNoOp(t *testing.T) {
	store := NewHistoryStore("")
	store.Record("flow-1", &models.AnalysisReport{}, &models.FlowDocument{ID: "flow-1"})
}

func TestHistoryStore_LoadNonExistentReturnsNil(t *testing.T) {
	dir := t.TempDir()
	store := NewHistoryStore(dir)
	if snapshots := store.Load("nonexistent"); snapshots != nil {
		t.Errorf("expected nil for non-existent flow, got %v", snapshots)
	}
}

func TestHistoryStore_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()

	store1 := NewHistoryStore(dir)
	store1.Record("flow-1", &models.AnalysisReport{
		FlowID: "flow-1",
		Stats:  models.AnalysisStats{Errors: 5},
	}, &models.FlowDocument{ID: "flow-1"})

	store2 := NewHistoryStore(dir)
	snapshots := store2.Load("flow-1")
	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot across instances, got %d", len(snapshots))
	}
	if snapshots[0].Errors != 5 {
		t.Errorf("expected 5 errors, got %d", snapshots[0].Errors)
	}
}

func TestHistoryStore_FilePathPattern(t *testing.T) {
	dir := t.TempDir()
	store := NewHistoryStore(dir)

	store.Record("flow-42", &models.AnalysisReport{}, &models.FlowDocument{ID: "flow-42"})

	path := filepath.Join(dir, "flow-42.json")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file at %s, got error: %v", path, err)
	}
}
