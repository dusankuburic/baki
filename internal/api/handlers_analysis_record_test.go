package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// analysisRecordCountingBackend counts the dashboard's flow_analysis writes.
// The shared fake has no counter for them, so the observable is the storage
// call the handler's detached record ends in.
type analysisRecordCountingBackend struct {
	*testutil.FakeBackend
	saves atomic.Int32
}

func (b *analysisRecordCountingBackend) SaveFlowAnalysis(ctx context.Context, fa *interfaces.FlowAnalysis) error {
	b.saves.Add(1)
	return b.FakeBackend.SaveFlowAnalysis(ctx, fa)
}

// TestHandleAnalyzeFlow_RecordsOnlyFreshRuns: POST /api/analysis/analyze used
// to run DashboardService.RecordAnalysis (the flow_analysis upsert plus a
// history row) on EVERY call, including the cache hits that return the
// previous run's report unchanged — one duplicate history row and one
// re-notification per findings-panel open. Only a fresh run records: twice
// for an unchanged flow is one write, a content change is one more. The
// record is detached (bounded, after the response), so the count is read
// after DrainBackground has waited for it.
func TestHandleAnalyzeFlow_RecordsOnlyFreshRuns(t *testing.T) {
	backend := &analysisRecordCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlowForAuthz(t, backend.FakeBackend)
	rt := newTestRouter(backend, true)
	tok := jwtBearer(t, rt, "u1", "u1@example.com")

	analyze := func(step string) {
		t.Helper()
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/analyze", tok, map[string]string{"flowId": "f1"})
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: analyze = %d: %s", step, rr.Code, rr.Body.String())
		}
	}

	analyze("first run")
	analyze("unchanged re-fetch")

	// The flow's content changes (a re-save bumps the version the cloud
	// document provider keys on), so the next analyze is a fresh run.
	if err := backend.SaveFlow(context.Background(), &interfaces.FlowDocument{
		ID:      "f1",
		Name:    "Flow One",
		OwnerID: "u1",
		Content: []byte(`{"id":"f1","name":"Flow One","subflows":[{"id":"main","name":"Main","blocks":[{"id":"b1","type":"action","rawType":"Display.ShowMessage","name":"Show","subflowId":"main","lineNumber":1}]}]}`),
	}); err != nil {
		t.Fatalf("re-save flow: %v", err)
	}
	analyze("after content change")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := moduleOf[*AnalysisHandler](t, rt).DrainBackground(ctx); err != nil {
		t.Fatalf("DrainBackground: %v", err)
	}
	if got := backend.saves.Load(); got != 2 {
		t.Errorf("SaveFlowAnalysis calls = %d, want 2 (first run + content change; the unchanged re-fetch is a cache hit and must not record)", got)
	}
}

// TestHandleAnalyzeFlow_RecordDoesNotBlockResponse: the dashboard record runs
// AFTER the response. A SaveFlowAnalysis that blocks until released must not
// hold the analyze response; DrainBackground then waits for it.
func TestHandleAnalyzeFlow_RecordDoesNotBlockResponse(t *testing.T) {
	backend := &blockingAnalysisRecordBackend{FakeBackend: testutil.NewFakeBackend(), release: make(chan struct{}), entered: make(chan struct{}, 1)}
	seedCloudFlowForAuthz(t, backend.FakeBackend)
	rt := newTestRouter(backend, true)
	tok := jwtBearer(t, rt, "u1", "u1@example.com")

	done := make(chan int, 1)
	go func() {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/analyze", tok, map[string]string{"flowId": "f1"})
		done <- rr.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("analyze = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("analyze response waited on the dashboard record")
	}
	select {
	case <-backend.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the dashboard record never started")
	}

	analysis := moduleOf[*AnalysisHandler](t, rt)
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- analysis.DrainBackground(ctx)
	}()
	select {
	case err := <-drained:
		t.Fatalf("DrainBackground returned (%v) while the record was still running", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(backend.release)
	if err := <-drained; err != nil {
		t.Fatalf("DrainBackground: %v", err)
	}
}

// blockingAnalysisRecordBackend parks the first SaveFlowAnalysis until
// release is closed.
type blockingAnalysisRecordBackend struct {
	*testutil.FakeBackend
	release chan struct{}
	entered chan struct{}
}

func (b *blockingAnalysisRecordBackend) SaveFlowAnalysis(ctx context.Context, fa *interfaces.FlowAnalysis) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return b.FakeBackend.SaveFlowAnalysis(ctx, fa)
}

// TestHandleAnalyzeFlow_RecordsReportsTheScannerProducedFirst: after a
// restart the governance scanner analyses most flows before any user does;
// its AnalyzeFlowBackground fills the report pair but never records to the
// dashboard. The user's first analyze is then a cache hit — and must still
// record (once), or flow_analysis stays stale for every flow the scanner
// reached first.
func TestHandleAnalyzeFlow_RecordsReportsTheScannerProducedFirst(t *testing.T) {
	backend := &analysisRecordCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlowForAuthz(t, backend.FakeBackend)
	rt := newTestRouter(backend, true)
	tok := jwtBearer(t, rt, "u1", "u1@example.com")
	h := moduleOf[*AnalysisHandler](t, rt)

	// Scanner-style: resolve the flow and analyse it in the background.
	doc, err := h.flowSvc.GetAuthorized(userActorCtx("u1"), "f1", service.FlowRead)
	if err != nil {
		t.Fatalf("resolve flow: %v", err)
	}
	if _, err := h.analysisSvc.AnalyzeFlowBackground(context.Background(), doc); err != nil {
		t.Fatalf("background analyze: %v", err)
	}

	for _, step := range []string{"first user analyze (cache hit on the scanner's report)", "second user analyze"} {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/analyze", tok, map[string]string{"flowId": "f1"})
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: analyze = %d: %s", step, rr.Code, rr.Body.String())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.DrainBackground(ctx); err != nil {
		t.Fatalf("DrainBackground: %v", err)
	}
	if got := backend.saves.Load(); got != 1 {
		t.Errorf("SaveFlowAnalysis calls = %d, want 1 (the scanner's report is unrecorded until the first user analyze; the second is a repeat)", got)
	}
}

// TestHandleAnalyzeFlow_DroppedRecordIsRetried: when the bounded runner
// refuses the detached record (saturated, or draining as here), the claim
// is released so the next analyze of the flow is fresh again instead of the
// row staying stale until the content changes.
func TestHandleAnalyzeFlow_DroppedRecordIsRetried(t *testing.T) {
	backend := &analysisRecordCountingBackend{FakeBackend: testutil.NewFakeBackend()}
	seedCloudFlowForAuthz(t, backend.FakeBackend)
	rt := newTestRouter(backend, true)
	tok := jwtBearer(t, rt, "u1", "u1@example.com")
	h := moduleOf[*AnalysisHandler](t, rt)

	// A drained runner refuses every task — the drop path without having to
	// saturate 32 slots.
	if err := h.DrainBackground(context.Background()); err != nil {
		t.Fatalf("DrainBackground: %v", err)
	}
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/analyze", tok, map[string]string{"flowId": "f1"})
	if rr.Code != http.StatusOK {
		t.Fatalf("analyze = %d: %s", rr.Code, rr.Body.String())
	}
	if got := backend.saves.Load(); got != 0 {
		t.Fatalf("SaveFlowAnalysis calls = %d after a refused record, want 0", got)
	}

	doc, err := h.flowSvc.GetAuthorized(userActorCtx("u1"), "f1", service.FlowRead)
	if err != nil {
		t.Fatalf("resolve flow: %v", err)
	}
	if _, fresh, err := h.analysisSvc.AnalyzeFlowFresh(context.Background(), doc); err != nil || !fresh {
		t.Errorf("AnalyzeFlowFresh after the dropped record = (fresh=%v, %v), want the claim released so the next analyze retries", fresh, err)
	}
}
