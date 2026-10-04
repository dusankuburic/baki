package lifecycle

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
)

func TestBounded_CapsConcurrencyAndDropsWhenFull(t *testing.T) {
	const task = "test_cap"
	b := NewBounded(task, 3)
	release := make(chan struct{})
	var running, peak atomic.Int32
	started := make(chan struct{}, 3)
	work := func() {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		running.Add(-1)
	}

	for i := 0; i < 3; i++ {
		if !b.Go(work) {
			t.Fatalf("task %d refused below the cap", i)
		}
	}
	for i := 0; i < 3; i++ {
		<-started
	}
	dropped := metrics.BackgroundDroppedCount(task, "saturated")
	for i := 0; i < 5; i++ {
		if b.Go(work) {
			t.Fatal("a task started past the cap")
		}
	}
	if got := metrics.BackgroundDroppedCount(task, "saturated") - dropped; got != 5 {
		t.Errorf("saturated drops counted = %v, want 5", got)
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if p := peak.Load(); p != 3 {
		t.Errorf("peak concurrency = %d, want 3", p)
	}
}

func TestBounded_PanicReleasesSlot(t *testing.T) {
	b := NewBounded("test_panic", 1)
	if !b.Go(func() { panic("boom") }) {
		t.Fatal("first task refused")
	}
	// The panicking task must give its slot back, or the runner is dead.
	deadline := time.Now().Add(2 * time.Second)
	ran := make(chan struct{})
	for !b.Go(func() { close(ran) }) {
		if time.Now().After(deadline) {
			t.Fatal("slot never released after a panicking task")
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-ran
}

func TestBounded_DrainWaitsThenRefuses(t *testing.T) {
	const task = "test_drain"
	b := NewBounded(task, 4)
	var finished atomic.Bool
	b.Go(func() {
		time.Sleep(50 * time.Millisecond)
		finished.Store(true)
	})
	if err := b.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !finished.Load() {
		t.Error("Drain returned before the running task finished")
	}

	closed := metrics.BackgroundDroppedCount(task, "closed")
	if b.Go(func() { t.Error("task ran after Drain") }) {
		t.Error("Go accepted a task after Drain")
	}
	if got := metrics.BackgroundDroppedCount(task, "closed") - closed; got != 1 {
		t.Errorf("closed drops counted = %v, want 1", got)
	}
	if err := b.Drain(context.Background()); err != nil {
		t.Errorf("second Drain: %v", err)
	}
}

func TestBounded_DrainRespectsContext(t *testing.T) {
	b := NewBounded("test_drain_ctx", 1)
	release := make(chan struct{})
	defer close(release)
	b.Go(func() { <-release })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := b.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Drain ignored its context: took %v", elapsed)
	}
}
