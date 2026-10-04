package analyzer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

// TestHistoryStore_ConcurrentRecord_PerFlowLocking is the concurrency gate
// for the per-flow keyed locking: Record's read-merge-write must stay
// atomic PER FLOW — two writers hammering one key must not lose each
// other's entries, the classic lost update the old store-wide mutex
// prevented — while writers on DISTINCT keys proceed in parallel. Every
// snapshot carries a globally unique error count, so the
// consecutive-duplicate skip in Record can never fire and the expected
// counts are exact; a dropped merge therefore shows up as a missing
// snapshot. The shared key writes 2×40 = 80 entries, below the default
// maxPerFlow of 100, so no trimming can mask a loss either.
func TestHistoryStore_ConcurrentRecord_PerFlowLocking(t *testing.T) {
	dir := t.TempDir()
	store := NewHistoryStore(dir)

	const distinctFlows = 8
	const perFlow = 25
	const sharedWriters = 2
	const sharedRecords = 40

	var wg sync.WaitGroup
	for f := 0; f < distinctFlows; f++ {
		wg.Add(1)
		go func(flowIdx int) {
			defer wg.Done()
			flowID := fmt.Sprintf("flow-%d", flowIdx)
			doc := &models.FlowDocument{ID: flowID}
			for i := 0; i < perFlow; i++ {
				store.Record(flowID, &models.AnalysisReport{
					FlowID: flowID,
					Stats:  models.AnalysisStats{Errors: flowIdx*1000 + i},
				}, doc)
			}
		}(f)
	}
	// Two writers on ONE shared key: their interleaving is arbitrary,
	// which is exactly what makes a lost update (a Record reading the file
	// before another writer's merge lands) observable as a missing entry.
	const sharedKey = "shared"
	for w := 0; w < sharedWriters; w++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			doc := &models.FlowDocument{ID: sharedKey}
			for i := 0; i < sharedRecords; i++ {
				store.Record(sharedKey, &models.AnalysisReport{
					FlowID: sharedKey,
					Stats:  models.AnalysisStats{Errors: 10000 + writer*1000 + i},
				}, doc)
			}
		}(w)
	}
	wg.Wait()

	for f := 0; f < distinctFlows; f++ {
		flowID := fmt.Sprintf("flow-%d", f)
		snapshots := store.Load(flowID)
		if len(snapshots) != perFlow {
			t.Errorf("flow %s: expected %d snapshots, got %d (a concurrent record was lost)", flowID, perFlow, len(snapshots))
		}
	}
	shared := store.Load(sharedKey)
	if len(shared) != sharedWriters*sharedRecords {
		t.Errorf("shared key: expected %d snapshots, got %d — same-key merges are not serialized (lost update)", sharedWriters*sharedRecords, len(shared))
	}
}
