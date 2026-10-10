package service

import (
	"context"
	"path/filepath"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

func makeAnalyzableDoc(id string, blocks ...models.Block) *models.FlowDocument {
	return &models.FlowDocument{
		ID:   id,
		Name: id,
		Subflows: []models.Subflow{
			{ID: "main", Name: "Main", Blocks: blocks},
		},
	}
}

// TestAnalyzeFlow_TracksPreviousReport is the H2 regression test: after two
// distinct analyses of the same flow, PreviousReport returns the first run so
// the diff endpoint compares real runs instead of an empty report. The two
// docs deliberately have DIFFERENT UUIDs but the same FilePath — the parser
// mints a fresh ID per load, so tracking must key on the stable path.
func TestAnalyzeFlow_TracksPreviousReport(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil, AnalysisOptions{})
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}

	doc1 := makeAnalyzableDoc("uuid-run-1",
		models.Block{ID: "b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show", SubflowID: "main", LineNumber: 1},
	)
	doc1.FilePath = `C:\flows\diff-test.txt`
	r1, err := svc.AnalyzeFlow(context.Background(), doc1)
	if err != nil {
		t.Fatalf("AnalyzeFlow run 1: %v", err)
	}

	if _, has := svc.PreviousReport(doc1); has {
		t.Fatal("PreviousReport should be empty after a single run")
	}

	// Re-running unchanged content is a cache hit → must NOT shift prev.
	if _, err := svc.AnalyzeFlow(context.Background(), doc1); err != nil {
		t.Fatalf("AnalyzeFlow rerun: %v", err)
	}
	if _, has := svc.PreviousReport(doc1); has {
		t.Fatal("cache-hit rerun must not create a previous report")
	}

	// Run 2: the file was edited and reloaded — new doc UUID, same path,
	// changed content → fresh report; prev should now be run 1.
	doc2 := makeAnalyzableDoc("uuid-run-2",
		models.Block{ID: "b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show changed", SubflowID: "main", LineNumber: 1},
	)
	doc2.FilePath = `C:\flows\diff-test.txt`
	r2, err := svc.AnalyzeFlow(context.Background(), doc2)
	if err != nil {
		t.Fatalf("AnalyzeFlow run 2: %v", err)
	}
	if r2 == r1 {
		t.Fatal("changed content should produce a fresh report")
	}

	prev, has := svc.PreviousReport(doc2)
	if !has || prev != r1 {
		t.Errorf("PreviousReport = (%p, %v), want run-1 report %p across reload", prev, has, r1)
	}

	// DiffReports over (prev, current) must not panic and must keep FlowID.
	diff := svc.DiffReports(prev, r2)
	if diff.FlowID != "uuid-run-2" {
		t.Errorf("diff.FlowID = %q", diff.FlowID)
	}
}

// TestHistoryStore_RecordDedup is the H1 regression test: identical snapshots
// (same content hash + counts) must not be appended twice.
func TestHistoryStore_RecordDedup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	store := analyzer.NewHistoryStore(dir)

	doc := makeAnalyzableDoc("flow-hist",
		models.Block{ID: "b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show", SubflowID: "main", LineNumber: 1},
	)
	report := &models.AnalysisReport{
		FlowID: "flow-hist",
		Stats:  models.AnalysisStats{Errors: 1, Warnings: 2, Info: 3},
	}

	store.Record("flow-hist", report, doc)
	store.Record("flow-hist", report, doc) // duplicate → skipped

	if got := len(store.Load("flow-hist")); got != 1 {
		t.Fatalf("after duplicate Record: %d snapshots, want 1", got)
	}

	// A run with different counts is a real new point.
	report2 := &models.AnalysisReport{
		FlowID: "flow-hist",
		Stats:  models.AnalysisStats{Errors: 0, Warnings: 2, Info: 3},
	}
	store.Record("flow-hist", report2, doc)

	snaps := store.Load("flow-hist")
	if len(snaps) != 2 {
		t.Fatalf("after distinct Record: %d snapshots, want 2", len(snaps))
	}
	if snaps[1].Errors != 0 {
		t.Errorf("latest snapshot errors = %d, want 0", snaps[1].Errors)
	}
}

// caches-2: a desktop folder reload re-parses unchanged text with new ids, so its report is a cache miss. It must
// become the current report (exports need its ids) without pushing the previous distinct run out of the diff.
func TestAnalyzeFlow_UnchangedReparseKeepsThePreviousRun(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil, AnalysisOptions{})
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	load := func(id, name string) *models.FlowDocument {
		doc := makeAnalyzableDoc(id,
			models.Block{ID: id + "-b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: name, SubflowID: "main", LineNumber: 1})
		doc.FilePath = `C:\flows\reload-test.txt`
		return doc
	}
	r1, err := svc.AnalyzeFlow(context.Background(), load("uuid-1", "Show"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.AnalyzeFlow(context.Background(), load("uuid-2", "Show changed"))
	if err != nil {
		t.Fatal(err)
	}
	reload := load("uuid-3", "Show changed")
	r3, err := svc.AnalyzeFlow(context.Background(), reload)
	if err != nil {
		t.Fatal(err)
	}
	if r3 == r2 {
		t.Fatal("premise: the re-parse (new ids) is a cache miss")
	}
	if cur, _ := svc.CurrentReport(reload); cur != r3 {
		t.Error("the current report must be the re-parse's: its ids are the document's")
	}
	if prev, has := svc.PreviousReport(reload); !has || prev != r1 {
		t.Errorf("PreviousReport = (%p, %t), want the last distinct run %p", prev, has, r1)
	}
}
