package database

import (
	"container/heap"
	"context"
	"sync"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-core/logger"
)

// blobCleaner runs deferred blob-cleanup work (deleting superseded content
// blobs, pruned version snapshots, and whole-flow prefixes) on a small, fixed
// worker pool instead of spawning one goroutine per cleanup. Previously every
// SaveFlow/SaveFlowVersion/DeleteFlow/DeleteUser spawned a detached goroutine —
// each of which slept for the reader-grace delay before deleting — so a burst
// of writes (bulk import, migration reruns) could leave thousands of sleeping
// goroutines alive at once, and a process shutdown abandoned them mid-flight.
//
// The cleaner decouples enqueue (cheap, non-blocking, O(log n) heap push) from
// execution (bounded to blobCleanupWorkers concurrent blob ops). A single
// dispatcher goroutine holds pending jobs in a min-heap ordered by their
// notBefore time and releases each to the workers when its delay elapses.
// Stop() flushes remaining work and waits (bounded) so a graceful shutdown
// doesn't leak the last window of superseded blobs.
type blobCleaner struct {
	mu      sync.Mutex
	pending jobHeap
	stopped bool

	wake chan struct{}        // buffered(1): nudge the dispatcher after an enqueue
	quit chan struct{}        // closed by Stop to begin draining
	exec chan *blobCleanupJob // dispatcher → workers

	workersWG  sync.WaitGroup
	dispatchWG sync.WaitGroup
	stopOnce   sync.Once
}

const (
	blobCleanupWorkers    = 4
	blobCleanupOpTimeout  = 30 * time.Second
	blobCleanupDrainWait  = 15 * time.Second
	blobCleanupExecBuffer = 256
)

// blobCleanupJob is one deferred blob operation. run receives a bounded,
// cancellable context and performs the actual blob call; it must not panic
// out (the worker recovers, but recovery loses the specific failure).
type blobCleanupJob struct {
	notBefore time.Time
	run       func(ctx context.Context)
	desc      string
	index     int // heap position, maintained by jobHeap
}

func newBlobCleaner() *blobCleaner {
	c := &blobCleaner{
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
		exec: make(chan *blobCleanupJob, blobCleanupExecBuffer),
	}
	c.workersWG.Add(blobCleanupWorkers)
	for range blobCleanupWorkers {
		go c.worker()
	}
	c.dispatchWG.Add(1)
	go c.dispatch()
	return c
}

// enqueue schedules a job to run no sooner than notBefore. It returns false if
// the cleaner has stopped (the caller should fall back to inline handling), so
// work is never silently dropped without the caller knowing.
func (c *blobCleaner) enqueue(notBefore time.Time, desc string, run func(ctx context.Context)) bool {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return false
	}
	heap.Push(&c.pending, &blobCleanupJob{notBefore: notBefore, run: run, desc: desc})
	metrics.SetBlobCleanerPending(len(c.pending))
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default: // dispatcher already has a pending wake; nothing to do
	}
	return true
}

func (c *blobCleaner) dispatch() {
	defer c.dispatchWG.Done()
	for {
		c.mu.Lock()
		var ready *blobCleanupJob
		wait := time.Hour // idle poll interval when the heap is empty
		if len(c.pending) > 0 {
			if !c.pending[0].notBefore.After(time.Now()) {
				ready = heap.Pop(&c.pending).(*blobCleanupJob)
			} else {
				wait = time.Until(c.pending[0].notBefore)
			}
		}
		metrics.SetBlobCleanerPending(len(c.pending))
		c.mu.Unlock()

		if ready != nil {
			select {
			case c.exec <- ready:
			case <-c.quit:
				c.flushOnQuit(ready)
				return
			}
			continue
		}

		select {
		case <-c.wake:
		case <-time.After(wait):
		case <-c.quit:
			c.flushOnQuit(nil)
			return
		}
	}
}

// flushOnQuit hands every remaining job (plus a job the dispatcher had already
// popped but not yet sent) to the workers immediately, ignoring notBefore, then
// closes exec so the workers drain and exit. Deleting a superseded blob a little
// early is safe — readers fetch content within milliseconds of reading the row,
// long inside the grace window — and is preferable to leaking it.
func (c *blobCleaner) flushOnQuit(popped *blobCleanupJob) {
	c.mu.Lock()
	c.stopped = true
	jobs := make([]*blobCleanupJob, 0, len(c.pending)+1)
	if popped != nil {
		jobs = append(jobs, popped)
	}
	for len(c.pending) > 0 {
		jobs = append(jobs, heap.Pop(&c.pending).(*blobCleanupJob))
	}
	c.pending = nil
	c.mu.Unlock()

	for _, j := range jobs {
		select {
		case c.exec <- j:
		default:
			// exec is full during shutdown; run inline so the work isn't lost.
			runCleanupJob(j)
		}
	}
	close(c.exec)
}

func (c *blobCleaner) worker() {
	defer c.workersWG.Done()
	for j := range c.exec {
		runCleanupJob(j)
	}
}

func runCleanupJob(j *blobCleanupJob) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("blob cleanup job panicked", "desc", j.desc, "err", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), blobCleanupOpTimeout)
	defer cancel()
	j.run(ctx)
}

// Stop begins draining and waits (bounded by blobCleanupDrainWait) for the
// dispatcher and workers to finish. After Stop returns, enqueue is a no-op that
// returns false. Idempotent.
func (c *blobCleaner) Stop() {
	c.stopOnce.Do(func() {
		// Signal the dispatcher to drain; it sets stopped=true and closes exec.
		close(c.quit)

		done := make(chan struct{})
		go func() {
			c.dispatchWG.Wait()
			c.workersWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(blobCleanupDrainWait):
			logger.Warn("blob cleaner: drain timed out; abandoning pending blob cleanups")
		}
	})
}

// jobHeap is a min-heap of pending cleanup jobs ordered by notBefore.
type jobHeap []*blobCleanupJob

func (h jobHeap) Len() int           { return len(h) }
func (h jobHeap) Less(i, j int) bool { return h[i].notBefore.Before(h[j].notBefore) }
func (h jobHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *jobHeap) Push(x any) {
	j := x.(*blobCleanupJob)
	j.index = len(*h)
	*h = append(*h, j)
}

func (h *jobHeap) Pop() any {
	old := *h
	n := len(old)
	j := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return j
}
