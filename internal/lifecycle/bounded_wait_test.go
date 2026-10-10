package lifecycle

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
)

// TestBounded_GoWaitBlocksAtTheCap: a sweep's delivery waits for a slot instead of being dropped, gives up when its
// context ends, and is refused once the runner drains.
func TestBounded_GoWaitBlocksAtTheCap(t *testing.T) {
	const task = "test_wait"
	g := &Group{}
	b := g.NewBounded(task, 1)
	release := make(chan struct{})
	if !b.GoWait(context.Background(), func() { <-release }) {
		t.Fatal("first task refused")
	}

	var second atomic.Bool
	started := make(chan bool, 1)
	go func() { started <- b.GoWait(context.Background(), func() { second.Store(true) }) }()
	select {
	case <-started:
		t.Fatal("GoWait returned while the only slot was taken")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if !<-started {
		t.Fatal("the waiting task was dropped instead of started")
	}

	hold := make(chan struct{})
	if !b.GoWait(context.Background(), func() { <-hold }) {
		t.Fatal("third task refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cancelled := metrics.BackgroundDroppedCount(task, "cancelled")
	if b.GoWait(ctx, func() {}) {
		t.Error("GoWait started a task after its context ended")
	}
	if got := metrics.BackgroundDroppedCount(task, "cancelled") - cancelled; got != 1 {
		t.Errorf("cancelled drops = %v, want 1", got)
	}
	close(hold)

	dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dcancel()
	if err := b.Drain(dctx); err != nil {
		t.Fatal(err)
	}
	if !second.Load() {
		t.Error("the waiting task never ran")
	}
	if b.GoWait(context.Background(), func() {}) {
		t.Error("GoWait started a task on a drained runner")
	}
}
