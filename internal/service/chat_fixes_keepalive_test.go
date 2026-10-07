package service

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// waitForGoroutines polls until the live goroutine count is at most want, so
// the keepalive tests can observe a goroutine actually returning (Go decrements
// the count at return, independent of GC).
func waitForGoroutines(t *testing.T, want int, deadline time.Duration) bool {
	t.Helper()
	exp := time.Now().Add(deadline)
	for time.Now().Before(exp) {
		if runtime.NumGoroutine() <= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return runtime.NumGoroutine() <= want
}

// TestBeginApplyKeepalive_EndsWhenTheCloseRunsDeferredPastAPanic reproduces
// the leak's trigger shape: the apply path panics and the window's close is
// reached only as a deferred call during the unwind (the stream worker's
// recover is the next frame up). The keepalive goroutine must return.
func TestBeginApplyKeepalive_EndsWhenTheCloseRunsDeferredPastAPanic(t *testing.T) {
	ap := &chatFixApplier{touch: func() {}}
	base := runtime.NumGoroutine()
	applyDone := ap.beginApplyKeepalive(context.Background())
	if !waitForGoroutines(t, base+1, 2*time.Second) {
		t.Fatalf("keepalive goroutine did not start (base=%d now=%d)", base, runtime.NumGoroutine())
	}

	// Exactly the ordering ApplyFixWithApproval/ApplyFixesWithApproval use:
	// defer close(applyDone) registered before the apply call that panics.
	func() {
		defer func() { _ = recover() }()
		defer close(applyDone)
		panic("apply boom")
	}()

	if !waitForGoroutines(t, base, 2*time.Second) {
		t.Fatalf("keepalive goroutine leaked after a panicking apply (base=%d now=%d)", base, runtime.NumGoroutine())
	}
}

// TestBeginApplyKeepalive_EndsOnCtxCancel: a hung apply must not keep the
// goroutine touching the stream's activity clock past the stream context.
func TestBeginApplyKeepalive_EndsOnCtxCancel(t *testing.T) {
	ap := &chatFixApplier{touch: func() {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := runtime.NumGoroutine()
	applyDone := ap.beginApplyKeepalive(ctx)
	if !waitForGoroutines(t, base+1, 2*time.Second) {
		t.Fatalf("keepalive goroutine did not start (base=%d now=%d)", base, runtime.NumGoroutine())
	}

	cancel()

	if !waitForGoroutines(t, base, 2*time.Second) {
		t.Fatalf("keepalive goroutine outlived the stream context (base=%d now=%d)", base, runtime.NumGoroutine())
	}
	// The caller's deferred close still runs afterwards and must be safe (the
	// goroutine is gone; a closed, unread channel is collectable, but the
	// close itself must not panic).
	close(applyDone)
}

// TestApplyFixWithApproval_PanicInApplyEndsTheKeepalive drives the production path: the
// decision is approved, the apply hook panics, and the panic is recovered one frame up
// (where the stream worker's recover sits in production). The keepalive goroutine started
// for the apply window must not survive it — before the defer-close fix it was stranded
// forever, touching the dead stream control every fixDecisionKeepalive and pinning the
// closure graph, answer buffer included.
func TestApplyFixWithApproval_PanicInApplyEndsTheKeepalive(t *testing.T) {
	doc := fixTestDoc()
	var hookRuns int32
	svc, _ := newFixTestService(doc, func(_ context.Context, _ *models.FlowDocument, _, _, _, _, _ string) (*models.FlowDocument, error) {
		atomic.AddInt32(&hookRuns, 1)
		panic("apply boom")
	})
	rec := &fixEventRecorder{}
	var touches int32
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, func() { atomic.AddInt32(&touches, 1) }, nil)
	prop := fixTestProposal()

	// Approve as soon as the proposal is registered (same shape as the other
	// applier tests).
	go func() {
		for i := 0; i < 1000; i++ {
			if _, ok := svc.pendingFixDecisions.Load(prop.ProposalID); ok {
				_ = svc.ResolveFixDecision("stream-1", prop.ProposalID, true, nil)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	base := runtime.NumGoroutine()
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the apply panic must still propagate to the stream worker's recover")
			}
		}()
		_, _ = ap.ApplyFixWithApproval(context.Background(), prop)
	}()

	if atomic.LoadInt32(&hookRuns) == 0 {
		t.Fatal("apply hook never ran — the window was not reached")
	}
	if !waitForGoroutines(t, base, 2*time.Second) {
		t.Fatalf("keepalive goroutine leaked after a panicking apply (base=%d now=%d)", base, runtime.NumGoroutine())
	}
}

// TestApplyFixesWithApproval_NilAnalysisCacheIsGraceful: the batch prepare
// step must bail with the sibling single-fix path's graceful shape when no
// analysis cache is wired, instead of panicking inside the apply window.
func TestApplyFixesWithApproval_NilAnalysisCacheIsGraceful(t *testing.T) {
	doc := fixTestDoc()
	notifier := &testutil.CountingNotifier{}
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	lfs := NewFlowService(notifier, nil, ldp, nil, nil, nil)
	svc := &ChatService{
		notifier:      notifier,
		flowCache:     lfs,
		analysisCache: nil, // the missing-cache wiring under test
	}
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	props := []ai.FixProposal{fixTestProposal()}

	go func() {
		for i := 0; i < 1000; i++ {
			found := false
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				if st := v.(*fixDecisionState); st.streamID == "stream-1" {
					found = true
					_ = svc.ResolveFixDecision("stream-1", k.(string), true, nil)
				}
				return false
			})
			if found {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	out, err := ap.ApplyFixesWithApproval(context.Background(), props)
	if err != nil {
		t.Fatalf("nil analysis cache must be a graceful outcome, got error %v", err)
	}
	if !strings.Contains(out, "error") {
		t.Errorf("model summary must carry the error outcome, got %q", out)
	}
	if evs := rec.snapshot(); evs[len(evs)-1] != "fix_decision:error" {
		t.Errorf("last event must be the error decision, got %v", evs)
	}
}
