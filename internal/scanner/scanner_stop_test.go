package scanner

import (
	"context"
	"errors"
	"testing"
	"time"

	"pad-core/models"
)

// Stop is one row of the server's stop budget (ADR 0024): the sweep's unwinding and the wait for the deliveries it
// started share one budget (defaultStopBudget), so the scanner's step cannot push the audit flush and the pool close
// past fx.StopTimeout. A delivery still running at the budget is the "notify" runner's, which the server's stop
// drains next.

// A delivery that does not end holds Stop for the budget, not for a budget of its own after the sweep's.
func TestStop_DeliveriesWaitWithinTheStopBudget(t *testing.T) {
	s := New(nil, nil, nil, nil, 0, nil)
	s.stopBudget = 300 * time.Millisecond
	s.deliveryWG.Add(1) // a delivery that has not answered
	t.Cleanup(s.deliveryWG.Done)

	start := time.Now()
	s.Stop()
	if took := time.Since(start); took < 250*time.Millisecond || took > 2*time.Second {
		t.Fatalf("Stop took %v with a delivery that never ends, want about the 300ms budget", took)
	}
}

// A sweep slow to unwind and a delivery that does not end: together they take the one budget, not the sweep's time
// plus a delivery budget.
func TestStop_SweepAndDeliveriesShareOneBudget(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	analyze := func(context.Context, *models.FlowDocument) (*models.AnalysisReport, error) {
		close(entered)
		// Ignores its cancellation for a while, like an analysis of a large flow.
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-release:
		}
		return nil, errors.New("analysis cut short")
	}
	s := New(b, nil, analyze, nil, 0, nil)
	s.stopBudget = 2 * time.Second
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		s.ScanOnce(context.Background())
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep never analyzed the flow")
	}
	s.deliveryWG.Add(1) // a delivery that has not answered
	t.Cleanup(s.deliveryWG.Done)

	start := time.Now()
	s.Stop()
	took := time.Since(start)
	<-swept
	// The sweep's 1.5 s are waited for (the run must end before the pool closes), then the deliveries get the rest
	// of the 2 s; separately they would take 3.5 s.
	if took < 1400*time.Millisecond || took > 2900*time.Millisecond {
		t.Fatalf("Stop took %v with a sweep unwinding for 1.5s and a delivery that never ends, want about the 2s budget", took)
	}
}
