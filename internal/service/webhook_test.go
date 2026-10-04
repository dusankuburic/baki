package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/notify"
	"pad-core/models"
)

// TestWebhookNotifier_CapsInFlightDispatches guards the unbounded fan-out:
// NotifyAnalysis ran `go Dispatch(...)` per analyze request, so while a
// channel was slow every request parked one more goroutine for the whole
// delivery. In-flight dispatches are now capped; the excess is dropped and
// counted, and Drain waits for the rest.
func TestWebhookNotifier_CapsInFlightDispatches(t *testing.T) {
	release := make(chan struct{})
	var inFlight, peak, served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		served.Add(1)
	}))
	defer srv.Close()
	// Deferred after srv.Close, so it runs first: the stalled handlers must
	// be let go before the server can close.
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	d, err := notify.New(notify.Config{
		WebhookURL:  srv.URL,
		Policy:      notify.AllowLoopbackTargets,
		Timeout:     10 * time.Second,
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	w := NewWebhookNotifier(d)
	report := &models.AnalysisReport{}

	dropped := metrics.BackgroundDroppedCount("analysis_webhook", "saturated")
	const requests = maxConcurrentAnalysisDispatches + 18
	for i := 0; i < requests; i++ {
		w.NotifyAnalysis("flow", report)
	}

	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < maxConcurrentAnalysisDispatches {
		if time.Now().After(deadline) {
			t.Fatalf("only %d dispatches reached the endpoint, want %d", inFlight.Load(), maxConcurrentAnalysisDispatches)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := metrics.BackgroundDroppedCount("analysis_webhook", "saturated") - dropped; got != 18 {
		t.Errorf("dropped dispatches = %v, want 18", got)
	}

	close(release)
	released = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if p := peak.Load(); p > maxConcurrentAnalysisDispatches {
		t.Errorf("peak in-flight dispatches = %d, want <= %d", p, maxConcurrentAnalysisDispatches)
	}
	if s := served.Load(); s != maxConcurrentAnalysisDispatches {
		t.Errorf("delivered %d, want %d", s, maxConcurrentAnalysisDispatches)
	}
}
