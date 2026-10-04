package service

import (
	"context"
	"testing"

	"pad-core/models"
)

// TestAnalyzeFlowFresh_ReportsCacheHits pins the freshness the analyze
// handler keys its per-run side effects on: a first run is fresh, re-analysing
// unchanged content is the cached report (not fresh), and a content change
// (new doc UUID, same path — the reload workflow) is fresh again. Freshness
// tracks the report pair, so AnalyzeFlow keeps seeing the same reports.
func TestAnalyzeFlowFresh_ReportsCacheHits(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil)
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	ctx := context.Background()

	doc1 := makeAnalyzableDoc("uuid-fresh-1",
		models.Block{ID: "b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show", SubflowID: "main", LineNumber: 1},
	)
	doc1.FilePath = `C:\flows\fresh-test.txt`

	r1, fresh, err := svc.AnalyzeFlowFresh(ctx, doc1)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if !fresh {
		t.Error("run 1: first analysis must be fresh")
	}

	r2, fresh, err := svc.AnalyzeFlowFresh(ctx, doc1)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if fresh {
		t.Error("run 2: unchanged content is a cache hit and must not be fresh")
	}
	if r2 != r1 {
		t.Error("run 2: a cache hit must return the run-1 report")
	}
	if cur, ok := svc.CurrentReport(doc1); !ok || cur != r1 {
		t.Errorf("CurrentReport after the cache hit = (%p, %v), want run-1 report %p", cur, ok, r1)
	}

	doc2 := makeAnalyzableDoc("uuid-fresh-2",
		models.Block{ID: "b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show changed", SubflowID: "main", LineNumber: 1},
	)
	doc2.FilePath = doc1.FilePath
	r3, fresh, err := svc.AnalyzeFlowFresh(ctx, doc2)
	if err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if !fresh {
		t.Error("run 3: changed content must be fresh")
	}
	if r3 == r1 {
		t.Error("run 3: changed content must produce a new report")
	}
	if prev, has := svc.PreviousReport(doc2); !has || prev != r1 {
		t.Errorf("PreviousReport after run 3 = (%p, %v), want run-1 report %p", prev, has, r1)
	}

	// The non-fresh variants still agree with the fresh one.
	if r4, err := svc.AnalyzeFlow(ctx, doc2); err != nil || r4 != r3 {
		t.Errorf("AnalyzeFlow after run 3 = (%p, %v), want run-3 report %p", r4, err, r3)
	}
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc2); err != nil || fresh {
		t.Errorf("AnalyzeFlowFresh after run 3 = (fresh=%v, %v), want a cache hit", fresh, err)
	}
}

// TestAnalyzeFlowFresh_ClaimsReportsOtherCallersProduced: freshness is the dashboard-record
// claim, not "new run". The governance scanner (AnalyzeFlowBackground) and the triage /
// policy / comment handlers (AnalyzeFlow) record the report pair too but never record to
// the dashboard, so after one of them analysed the flow first the analyze handler's call
// is a cache hit that is STILL fresh — once. Keying on the pair's current report left
// flow_analysis stale for every flow the scanner reached first after a restart (and the
// scanner's own health-drop check reads that row).
func TestAnalyzeFlowFresh_ClaimsReportsOtherCallersProduced(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil)
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	ctx := context.Background()
	doc := makeAnalyzableDoc("uuid-claim-1",
		models.Block{ID: "b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show", SubflowID: "main", LineNumber: 1},
	)
	doc.FilePath = `C:\flows\claim-test.txt`

	bg, err := svc.AnalyzeFlowBackground(ctx, doc)
	if err != nil {
		t.Fatalf("background run: %v", err)
	}
	if _, err := svc.AnalyzeFlow(ctx, doc); err != nil {
		t.Fatalf("AnalyzeFlow run: %v", err)
	}

	r, fresh, err := svc.AnalyzeFlowFresh(ctx, doc)
	if err != nil {
		t.Fatalf("AnalyzeFlowFresh: %v", err)
	}
	if r != bg {
		t.Fatal("AnalyzeFlowFresh after the background run must be a cache hit on its report")
	}
	if !fresh {
		t.Error("a report the scanner produced first is unrecorded and must be fresh for the analyze handler")
	}
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc); err != nil || fresh {
		t.Errorf("second AnalyzeFlowFresh = (fresh=%v, %v), want the claim already taken", fresh, err)
	}
	// Other recording callers do not take or disturb the claim.
	if _, err := svc.AnalyzeFlowBackground(ctx, doc); err != nil {
		t.Fatalf("background rerun: %v", err)
	}
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc); err != nil || fresh {
		t.Errorf("AnalyzeFlowFresh after a background rerun = (fresh=%v, %v), want still claimed", fresh, err)
	}
	if cur, ok := svc.CurrentReport(doc); !ok || cur != bg {
		t.Errorf("CurrentReport = (%p, %v), want the background report %p untouched", cur, ok, bg)
	}
}

// TestReleaseRecord_ReopensTheClaim: a recorder that could not run the
// dashboard write (bounded runner saturated or draining) gives the claim
// back, so the next analyze retries; a stale release (an older report) is a
// no-op.
func TestReleaseRecord_ReopensTheClaim(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil)
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	ctx := context.Background()
	doc := makeAnalyzableDoc("uuid-release-1",
		models.Block{ID: "b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show", SubflowID: "main", LineNumber: 1},
	)
	doc.FilePath = `C:\flows\release-test.txt`

	r1, fresh, err := svc.AnalyzeFlowFresh(ctx, doc)
	if err != nil || !fresh {
		t.Fatalf("first AnalyzeFlowFresh = (fresh=%v, %v), want fresh", fresh, err)
	}
	svc.ReleaseRecord(doc, r1)
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc); err != nil || !fresh {
		t.Errorf("AnalyzeFlowFresh after ReleaseRecord = (fresh=%v, %v), want fresh again", fresh, err)
	}
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc); err != nil || fresh {
		t.Errorf("AnalyzeFlowFresh after the retry = (fresh=%v, %v), want claimed", fresh, err)
	}

	// A release of a report that no longer holds the claim changes nothing.
	doc2 := makeAnalyzableDoc("uuid-release-2",
		models.Block{ID: "b1", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show changed", SubflowID: "main", LineNumber: 1},
	)
	doc2.FilePath = doc.FilePath
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc2); err != nil || !fresh {
		t.Fatalf("changed content = (fresh=%v, %v), want fresh", fresh, err)
	}
	svc.ReleaseRecord(doc2, r1)
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc2); err != nil || fresh {
		t.Errorf("AnalyzeFlowFresh after a stale release = (fresh=%v, %v), want still claimed", fresh, err)
	}
	svc.ReleaseRecord(doc2, nil)
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc2); err != nil || fresh {
		t.Errorf("AnalyzeFlowFresh after a nil release = (fresh=%v, %v), want still claimed", fresh, err)
	}
}
