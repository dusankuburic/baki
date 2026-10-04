package service

import (
	"context"
	"runtime"
	"testing"
	"weak"

	"pad-analyzer/internal/ai"
)

// queueProbeJob queues a refresh for key (every worker slot must be busy) and
// returns only a weak pointer to its entries.
func queueProbeJob(r *azureListingRefresher, key string) weak.Pointer[ai.AzureRegisteredModel] {
	entries := []ai.AzureRegisteredModel{{EntryID: key, Kind: "chat", Deployment: "gpt-4o"}}
	r.trigger(azureRefreshJob{key: key, entries: entries})
	return weak.Make(&entries[0])
}

// TestAzureListingRefresher_PoppedJobUnreachable: a worker popping the queue
// must release the popped job — reslicing past it (queue[1:]) kept the job's
// entries and credential-reading closure in the backing array.
func TestAzureListingRefresher_PoppedJobUnreachable(t *testing.T) {
	r := newAzureListingRefresher(func(context.Context, string, []ai.AzureRegisteredModel) error { return nil })
	// Every worker slot busy: trigger queues instead of starting a worker.
	r.mu.Lock()
	r.running = azureRefreshParallel
	r.mu.Unlock()
	first := queueProbeJob(r, "a")
	queueProbeJob(r, "b")
	queueProbeJob(r, "c")

	// Drain on this goroutine exactly as a worker would: run the job it was
	// started with, then pop the queue until it is empty. (A job without a
	// credential reader is skipped by run, but still popped.)
	r.wg.Add(1)
	r.work(azureRefreshJob{key: "running"})
	r.mu.Lock()
	queued, inflight := len(r.queue), len(r.inflight)
	r.mu.Unlock()
	if queued != 0 || inflight != 0 {
		t.Fatalf("queue = %d, inflight = %d after the drain; want 0, 0", queued, inflight)
	}

	if !weakCollected(first) {
		t.Fatal("a popped refresh job is still reachable (pinned by the queue's backing array)")
	}
	// The refresher (and so its queue) must outlive the GC polling above —
	// otherwise the whole thing is garbage and the check proves nothing.
	runtime.KeepAlive(r)
}
