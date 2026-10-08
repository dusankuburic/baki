package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// purgeOnlyBackend is a StorageBackend whose only live method is
// PurgeExpiredData: the embedded nil interface satisfies the rest (and panics
// if anything else is called, which runRetentionSweep must never do).
type purgeOnlyBackend struct {
	storageif.StorageBackend
	purges  atomic.Int32
	release chan struct{} // when non-nil, PurgeExpiredData blocks until closed
	entered chan struct{} // closed once PurgeExpiredData has started
	ctxs    chan context.Context
	panics  bool
}

func (b *purgeOnlyBackend) PurgeExpiredData(ctx context.Context, _ int) (*storageif.PurgeResult, error) {
	b.purges.Add(1)
	if b.ctxs != nil {
		b.ctxs <- ctx
	}
	if b.entered != nil {
		close(b.entered)
	}
	if b.panics {
		panic("boom")
	}
	if b.release != nil {
		<-b.release
	}
	return &storageif.PurgeResult{}, nil
}

// lockingPurgeBackend is also the cross-replica lock, modelled the way the
// Postgres advisory lock behaves: one holder at a time, released exactly once.
type lockingPurgeBackend struct {
	*purgeOnlyBackend
	held     atomic.Bool
	released atomic.Int32
	keys     []int64
	mu       sync.Mutex
}

func (b *lockingPurgeBackend) TryGlobalLock(_ context.Context, key int64) (func(), bool, error) {
	b.mu.Lock()
	b.keys = append(b.keys, key)
	b.mu.Unlock()
	if !b.held.CompareAndSwap(false, true) {
		return nil, false, nil
	}
	return func() {
		b.released.Add(1)
		b.held.Store(false)
	}, true, nil
}

// TestRunRetentionSweep_OverlappingSweepsPurgeOnce pins the multi-replica
// guard: two sweeps racing on the same backend (two pods ticking at once)
// must purge once, and the holder must release the lock exactly once.
func TestRunRetentionSweep_OverlappingSweepsPurgeOnce(t *testing.T) {
	inner := &purgeOnlyBackend{release: make(chan struct{}), entered: make(chan struct{})}
	b := &lockingPurgeBackend{purgeOnlyBackend: inner}

	first := make(chan struct{})
	go func() {
		defer close(first)
		runRetentionSweep(context.Background(), b, b, 30)
	}()
	select {
	case <-inner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first sweep never reached PurgeExpiredData")
	}

	// Second sweep while the first is still deleting: must skip, not wait.
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRetentionSweep(context.Background(), b, b, 30)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second sweep blocked instead of skipping while the lock was held")
	}
	if got := inner.purges.Load(); got != 1 {
		t.Fatalf("overlapping sweeps purged %d time(s), want 1", got)
	}
	if got := b.released.Load(); got != 0 {
		t.Fatalf("lock released %d time(s) while the holder was still sweeping", got)
	}

	close(inner.release)
	<-first
	if got := b.released.Load(); got != 1 {
		t.Fatalf("lock released %d time(s) after the holder finished, want exactly 1", got)
	}
	if b.held.Load() {
		t.Fatal("lock still held after the sweep returned")
	}
	for _, k := range b.keys {
		if k != retentionLockKey {
			t.Errorf("sweep asked for key %#x, want retentionLockKey %#x", k, retentionLockKey)
		}
	}

	// A later sweep (next tick) acquires the freed lock and purges again.
	runRetentionSweep(context.Background(), b, b, 30)
	if got := inner.purges.Load(); got != 2 {
		t.Fatalf("sweep after release purged %d time(s) total, want 2", got)
	}
}

// TestRunRetentionSweep_LockKeyIsDistinct: the key must not collide with the
// scanner's or the migration runner's, or holding one would starve the other.
func TestRunRetentionSweep_LockKeyIsDistinct(t *testing.T) {
	const scanLockKey int64 = 0x62616b697363616e      // internal/scanner
	const migrationLockKey int64 = 0x62616b696d696772 // internal/api
	if retentionLockKey == scanLockKey || retentionLockKey == migrationLockKey {
		t.Fatalf("retentionLockKey %#x collides with another loop's key", retentionLockKey)
	}
}

// TestRunRetentionSweep_NoLockerSweeps: without a locker (no database) the
// sweep runs alone.
func TestRunRetentionSweep_NoLockerSweeps(t *testing.T) {
	b := &purgeOnlyBackend{}
	runRetentionSweep(context.Background(), b, nil, 30)
	if got := b.purges.Load(); got != 1 {
		t.Fatalf("purged %d time(s), want 1", got)
	}
}

// TestRunRetentionSweep_BudgetIsMinutesNotSeconds: the sweep's deadline must
// be well past the 30 s per-statement timeout, or a sweep whose first table
// takes most of that budget never finishes the rest.
func TestRunRetentionSweep_BudgetIsMinutesNotSeconds(t *testing.T) {
	b := &purgeOnlyBackend{ctxs: make(chan context.Context, 1)}
	start := time.Now()
	runRetentionSweep(context.Background(), b, nil, 30)
	ctx := <-b.ctxs
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("sweep context has no deadline; a wedged DELETE would block shutdown")
	}
	if budget := dl.Sub(start); budget < 5*time.Minute || budget > retentionSweepTimeout+time.Second {
		t.Fatalf("sweep budget %v, want about %v", budget, retentionSweepTimeout)
	}
}

// TestRunRetentionSweep_PanicReleasesLock: the panic guard must stay inside
// the lock scope so a panicking sweep does not wedge every later tick on
// every replica until the process restarts.
func TestRunRetentionSweep_PanicReleasesLock(t *testing.T) {
	b := &lockingPurgeBackend{purgeOnlyBackend: &purgeOnlyBackend{panics: true}}
	runRetentionSweep(context.Background(), b, b, 30) // must not propagate
	if got := b.released.Load(); got != 1 {
		t.Fatalf("lock released %d time(s) after a panicking sweep, want 1", got)
	}
	if b.held.Load() {
		t.Fatal("lock still held after a panicking sweep")
	}
}
