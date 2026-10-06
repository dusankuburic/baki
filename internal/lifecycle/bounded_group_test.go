package lifecycle

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestGroupDrain_KeepsHandOffsBetweenRunners: a task on one runner may hand work to another (an alert fan-out
// enqueues its email). Drain waits for every runner to go idle before closing any, so the hand-off is not
// dropped even when the receiving runner was created (and would otherwise be closed) first.
func TestGroupDrain_KeepsHandOffsBetweenRunners(t *testing.T) {
	g := &Group{}
	emails := g.NewBounded("test_email", 1) // created first, as in NewAnalysisHandler
	alerts := g.NewBounded("test_alert", 1)

	var sent atomic.Bool
	release := make(chan struct{})
	if !alerts.Go(func() {
		<-release
		if !emails.Go(func() { sent.Store(true) }) {
			t.Error("the alert's email hand-off was dropped")
		}
	}) {
		t.Fatal("alert not started")
	}

	drained := make(chan error, 1)
	go func() { drained <- g.Drain(context.Background()) }()
	time.Sleep(30 * time.Millisecond) // Drain is waiting for the alert
	close(release)

	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return")
	}
	if !sent.Load() {
		t.Error("Drain returned before the handed-off email ran")
	}
	if emails.Go(func() {}) || alerts.Go(func() {}) {
		t.Error("runners still accept work after Drain")
	}
}

// TestGroupDrain_GivesUpAtTheDeadline: a task still running when ctx ends is abandoned, and Drain says so.
func TestGroupDrain_GivesUpAtTheDeadline(t *testing.T) {
	g := &Group{}
	stuck := g.NewBounded("test_stuck", 1)
	block := make(chan struct{})
	defer close(block)
	stuck.Go(func() { <-block })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain = %v, want the deadline", err)
	}
}

// TestBounded_PanicIsRecoveredAndCounted: a panicking task neither crashes the process nor stays "in flight".
func TestBounded_PanicIsRecoveredAndCounted(t *testing.T) {
	g := &Group{}
	b := g.NewBounded("test_panic", 1)
	if !b.Go(func() { panic("boom") }) {
		t.Fatal("not started")
	}
	if err := g.Drain(context.Background()); err != nil {
		t.Fatalf("Drain after a panic: %v", err)
	}
	if n := b.inflight.Load(); n != 0 {
		t.Errorf("in-flight count = %d after the task panicked, want 0", n)
	}
}
