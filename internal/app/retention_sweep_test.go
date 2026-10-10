package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/lifecycle"
	padmetrics "pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/lock"
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

// TryLease takes the same lock, as a lease that cannot report losing it.
func (b *lockingPurgeBackend) TryLease(ctx context.Context, key int64) (lock.Lease, bool, error) {
	return lock.AsLeaser(b).TryLease(ctx, key)
}

// runRetentionSweep is one attempt of the retention job as one replica's schedule makes it: the replica's own
// Singleton over the lock the replicas share (the backend's, or none).
func runRetentionSweep(ctx context.Context, backend storageif.StorageBackend, leaser lock.Leaser, auditRetentionDays int) error {
	job := lifecycle.NewSingleton(retentionJob(backend, 0, auditRetentionDays), leaser, nil)
	defer job.Stop()
	return job.RunOnce(ctx)
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

// lockErrorBackend is the purge backend behind a lock that cannot be asked for (a database blip), in both of the
// lock's forms: the plain lock and the lease.
type lockErrorBackend struct {
	*purgeOnlyBackend
	asked atomic.Int32
}

var errLockUnavailable = errors.New("connection refused")

func (b *lockErrorBackend) TryGlobalLock(context.Context, int64) (func(), bool, error) {
	b.asked.Add(1)
	return nil, false, errLockUnavailable
}

func (b *lockErrorBackend) TryLease(context.Context, int64) (lock.Lease, bool, error) {
	b.asked.Add(1)
	return nil, false, errLockUnavailable
}

// TestRunRetentionSweep_LockErrorSkips (R3): a lock that cannot be asked for skips the sweep, fail closed, and the
// schedule retries it soon. Sweeping without the lock let every replica purge at once.
func TestRunRetentionSweep_LockErrorSkips(t *testing.T) {
	b := &lockErrorBackend{purgeOnlyBackend: &purgeOnlyBackend{}}
	err := runRetentionSweep(context.Background(), b, b, 30)
	if b.asked.Load() == 0 {
		t.Fatal("the sweep never asked for its lock")
	}
	if got := b.purges.Load(); got != 0 {
		t.Fatalf("a lock error purged %d time(s), want none (the sweep fails closed)", got)
	}
	if !errors.Is(err, lifecycle.ErrJobLockUnavailable) {
		t.Errorf("the attempt = %v, want ErrJobLockUnavailable (which the schedule retries after %v)", err, retentionLockRetry)
	}
}

// TestRetentionJob_Schedule: the purge runs at boot and every PAD_RETENTION_PURGE_INTERVAL under its legacy key,
// retries a lock error in minutes rather than a whole interval later, and is overdue once it has succeeded nowhere
// for twice its interval.
func TestRetentionJob_Schedule(t *testing.T) {
	interval := config.Default().Governance.RetentionPurgeInterval.Std()
	job := retentionJob(&purgeOnlyBackend{}, interval, 30)
	if job.Spec.Name() != "retention_purge" || job.Spec.Key() != retentionLockKey {
		t.Errorf("job %s with key %#x, want retention_purge with the legacy key %#x", job.Spec.Name(), job.Spec.Key(), retentionLockKey)
	}
	if job.Every != interval || !job.RunAtStart {
		t.Errorf("every %v, at start %v; want every %v and a sweep at boot", job.Every, job.RunAtStart, interval)
	}
	if job.RetryOnLockError != retentionLockRetry || retentionLockRetry >= interval {
		t.Errorf("lock-error retry %v, want %v, sooner than the interval %v", job.RetryOnLockError, retentionLockRetry, interval)
	}
	if job.OverdueAfter != 2*interval {
		t.Errorf("overdue after %v, want twice the interval (%v)", job.OverdueAfter, 2*interval)
	}
}

// lastSuccessAgo is the persisted job state of a purge that last succeeded age ago, on some replica.
type lastSuccessAgo struct{ age time.Duration }

func (s lastSuccessAgo) RecordJobStart(context.Context, string, string, time.Time) error { return nil }
func (s lastSuccessAgo) RecordJobEnd(context.Context, string, time.Time, string) error   { return nil }
func (s lastSuccessAgo) JobLastSuccess(context.Context, string) (time.Time, error) {
	return time.Now().Add(-s.age), nil
}

// TestRetentionPurge_OverdueAfterTwiceTheInterval: a replica that skips the purge (another holds the lock) while
// some replica purged within twice the interval reports nothing; once no replica has for longer, it logs the fixed
// "job overdue" line and sets pad_job_overdue: personal data is kept past its retention.
func TestRetentionPurge_OverdueAfterTwiceTheInterval(t *testing.T) {
	logs := captureLogs(t)
	const interval = 100 * time.Millisecond
	held := &leaseLocker{held: true}
	recent := lifecycle.NewSingleton(retentionJob(&purgeOnlyBackend{}, interval, 30), held, lastSuccessAgo{interval / 2})
	defer recent.Stop()
	time.Sleep(3 * interval) // the replica has been up for longer than the threshold
	_ = recent.RunOnce(context.Background())
	if padmetrics.JobOverdueValue("retention_purge") != 0 || len(logs.lines(lifecycle.LogJobOverdue)) != 0 {
		t.Fatal("a skip while another replica purges on time reported the purge overdue")
	}

	stale := lifecycle.NewSingleton(retentionJob(&purgeOnlyBackend{}, interval, 30), held, lastSuccessAgo{10 * interval})
	defer stale.Stop()
	time.Sleep(3 * interval)
	_ = stale.RunOnce(context.Background())
	if padmetrics.JobOverdueValue("retention_purge") != 1 {
		t.Error("pad_job_overdue{job=\"retention_purge\"} is not 1 after no purge anywhere for twice the interval")
	}
	if lines := logs.lines(lifecycle.LogJobOverdue); len(lines) != 1 || !strings.Contains(lines[0], "job=retention_purge") {
		t.Errorf("%q lines = %q, want one for retention_purge", lifecycle.LogJobOverdue, lines)
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
