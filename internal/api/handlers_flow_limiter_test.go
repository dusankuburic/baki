package api

import (
	"context"
	"sync"
	"testing"
)

// TestUploadLimiter_PruneOnRelease is the regression test for the unbounded
// growth of uploadLimiter.sems: every distinct userID that ever uploaded left a
// permanent entry in the map (monotonic memory leak in multi-tenant cloud).
// After the fix, releasing the last slot for a user must delete its entry, so
// the map returns to empty once all uploads finish.
func TestUploadLimiter_PruneOnRelease(t *testing.T) {
	l := newUploadLimiter()

	for i := 0; i < 50; i++ {
		uid := "user-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		rel, ok := l.acquire(context.Background(), uid)
		if !ok {
			t.Fatalf("acquire for %s failed", uid)
		}
		rel()
	}

	if got := len(l.sems); got != 0 {
		t.Errorf("sems map leaked %d entries after all releases; want 0 (map=%v)", got, l.sems)
	}
}

// TestUploadLimiter_KeepsEntryWhileInUse confirms the entry is NOT pruned while
// a concurrent upload for the user is still in flight (only the LAST release
// may delete), and that re-acquiring after pruning works (a fresh channel is
// recreated).
func TestUploadLimiter_KeepsEntryWhileInUse(t *testing.T) {
	l := newUploadLimiter()

	// Two concurrent holds for the same user.
	rel1, _ := l.acquire(context.Background(), "alice")
	rel2, _ := l.acquire(context.Background(), "alice")

	rel1()
	if _, ok := l.sems["alice"]; !ok {
		t.Error("entry pruned while a hold is still in flight (alice still has rel2)")
	}

	rel2()
	if _, ok := l.sems["alice"]; ok {
		t.Error("entry not pruned after the last release")
	}

	// Re-acquire works after pruning (recreates the channel).
	rel3, ok := l.acquire(context.Background(), "alice")
	if !ok {
		t.Fatal("re-acquire after pruning failed")
	}
	rel3()
	if _, ok := l.sems["alice"]; ok {
		t.Error("entry not pruned after final re-acquire+release")
	}
}

// TestUploadLimiter_ConcurrentNoLeak exercises many concurrent acquire/release
// cycles across distinct users under the race detector and asserts no entries
// leak (every released user is eventually pruned) and no goroutine deadlocks.
func TestUploadLimiter_ConcurrentNoLeak(t *testing.T) {
	l := newUploadLimiter()
	const workers = 16
	const iters = 50

	var wg sync.WaitGroup
	wg.Add(workers)
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < iters; i++ {
				// Each worker uses a stable userID so holds overlap across
				// workers for the same user (exercises the last-release path).
				uid := "user-" + string(rune('a'+w))
				rel, ok := l.acquire(context.Background(), uid)
				if ok {
					rel()
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	// Every worker finished; all entries must be pruned.
	if got := len(l.sems); got != 0 {
		t.Errorf("sems leaked %d entries after concurrent test (map=%v)", got, l.sems)
	}
}
