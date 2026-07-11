package database

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBlobCleaner_RunsJobsAfterDelay(t *testing.T) {
	c := newBlobCleaner()
	defer c.Stop()

	var ran atomic.Int32
	c.enqueue(time.Now().Add(50*time.Millisecond), "delayed", func(context.Context) {
		ran.Add(1)
	})

	// Not yet due.
	time.Sleep(10 * time.Millisecond)
	if got := ran.Load(); got != 0 {
		t.Fatalf("job ran before its delay: %d", got)
	}
	// Due shortly after.
	deadline := time.Now().Add(2 * time.Second)
	for ran.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("delayed job did not run within timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBlobCleaner_RunsReadyJobsInDelayOrder(t *testing.T) {
	c := newBlobCleaner()
	defer c.Stop()

	// Enqueue out of order; the heap must release them earliest-first. Use a
	// single worker's worth of ordering by spacing delays widely.
	var mu sync.Mutex
	var order []int
	record := func(n int) func(context.Context) {
		return func(context.Context) {
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
		}
	}
	base := time.Now()
	c.enqueue(base.Add(120*time.Millisecond), "3", record(3))
	c.enqueue(base.Add(40*time.Millisecond), "1", record(1))
	c.enqueue(base.Add(80*time.Millisecond), "2", record(2))

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 3 jobs to run, got %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Errorf("jobs ran out of delay order: %v", order)
	}
}

func TestBlobCleaner_StopFlushesPendingImmediately(t *testing.T) {
	c := newBlobCleaner()

	var ran atomic.Int32
	// A job with a long delay must still run on Stop (flush ignores the delay).
	c.enqueue(time.Now().Add(time.Hour), "long", func(context.Context) {
		ran.Add(1)
	})
	c.Stop() // blocks until drained (or drain timeout)

	if got := ran.Load(); got != 1 {
		t.Errorf("Stop did not flush the pending job: ran=%d", got)
	}
	// enqueue after Stop is rejected.
	if c.enqueue(time.Now(), "post-stop", func(context.Context) {}) {
		t.Error("enqueue after Stop should return false")
	}
}

func TestBlobCleaner_StopIsIdempotent(t *testing.T) {
	c := newBlobCleaner()
	c.Stop()
	c.Stop() // must not panic (no double-close)
}

func TestBlobCleaner_PanicInJobIsContained(t *testing.T) {
	c := newBlobCleaner()
	defer c.Stop()

	var ran atomic.Int32
	c.enqueue(time.Now(), "panics", func(context.Context) { panic("boom") })
	c.enqueue(time.Now(), "ok", func(context.Context) { ran.Add(1) })

	deadline := time.Now().Add(2 * time.Second)
	for ran.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("subsequent job did not run after a panicking job")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
