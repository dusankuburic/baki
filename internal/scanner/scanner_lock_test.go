package scanner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/lifecycle"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// lockingBackend is the in-memory backend and a cross-replica lock that
// behaves like the Postgres advisory lock, so the guard in ScanOnce is exercised.
type lockingBackend struct {
	*healthBackend
	held     bool
	err      error // the lock cannot be asked for (a database error)
	released atomic.Int32
	key      int64
}

func (b *lockingBackend) TryGlobalLock(_ context.Context, key int64) (func(), bool, error) {
	b.key = key
	if b.err != nil {
		return nil, false, b.err
	}
	if b.held {
		return nil, false, nil
	}
	return func() { b.released.Add(1) }, true, nil
}

// countingAnalyze records how many flows a sweep analysed.
func countingAnalyze(n *atomic.Int32) AnalyzeFunc {
	return func(_ context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		n.Add(1)
		return &models.AnalysisReport{FlowID: doc.ID}, nil
	}
}

// TestScanOnce_SkipsWhileAnotherReplicaHoldsTheLock pins the multi-replica
// guard: every pod runs its own ticker, and without the lock N replicas each
// re-analysed every flow and dispatched the same alert N times.
func TestScanOnce_SkipsWhileAnotherReplicaHoldsTheLock(t *testing.T) {
	b := &lockingBackend{healthBackend: newBackend(), held: true}
	seedFlow(t, b, "f1")
	var analysed atomic.Int32

	s := New(b, b, countingAnalyze(&analysed), nil, 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	if got := analysed.Load(); got != 0 {
		t.Fatalf("a replica without the lock must not sweep, analysed %d flow(s)", got)
	}
	if b.key != scanLockKey {
		t.Errorf("sweep must ask for scanLockKey, asked for %#x", b.key)
	}
}

// TestScanOnce_SweepsAndReleasesWhenLockAcquired: the holder sweeps normally
// and releases the session lock exactly once afterwards.
func TestScanOnce_SweepsAndReleasesWhenLockAcquired(t *testing.T) {
	b := &lockingBackend{healthBackend: newBackend()}
	seedFlow(t, b, "f1")
	var analysed atomic.Int32

	s := New(b, b, countingAnalyze(&analysed), nil, 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	if got := analysed.Load(); got != 1 {
		t.Fatalf("lock holder should analyse the stored flow once, analysed %d", got)
	}
	if got := b.released.Load(); got != 1 {
		t.Errorf("lock must be released exactly once, released %d times", got)
	}
}

// TestScanOnce_LockErrorSkips: a lock that cannot be asked for (the database blipped) fails closed. Sweeping
// without it let every replica sweep at once and dispatch every alert once per replica (R3).
func TestScanOnce_LockErrorSkips(t *testing.T) {
	b := &lockingBackend{healthBackend: newBackend(), err: errors.New("connection refused")}
	seedFlow(t, b, "f1")
	var analysed atomic.Int32

	s := New(b, b, countingAnalyze(&analysed), nil, 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	if got := analysed.Load(); got != 0 {
		t.Fatalf("a sweep without its lock must not run, analysed %d flow(s)", got)
	}
}

// TestScanNow_ReportsWhyTheSweepDidNotStart: the admin "scan now" learns what became of its sweep, which ScanOnce
// only logs: a sweep running on another replica, a lock that cannot be asked for, a stopped scanner, a scanner with
// nothing to sweep with. Nothing is analysed then.
func TestScanNow_ReportsWhyTheSweepDidNotStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		held    bool
		lockErr error
		stopped bool
		want    error
	}{
		{"another replica sweeps", true, nil, false, lifecycle.ErrJobHeld},
		{"the lock cannot be asked for", false, errors.New("connection refused"), false, lifecycle.ErrJobLockUnavailable},
		{"the scanner stopped", false, nil, true, lifecycle.ErrJobStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &lockingBackend{healthBackend: newBackend(), held: tc.held, err: tc.lockErr}
			seedFlow(t, b, "f1")
			var analysed atomic.Int32
			s := New(b, b, countingAnalyze(&analysed), nil, 0, loopbackChannels(b))
			t.Cleanup(s.Stop)
			if tc.stopped {
				s.Stop()
			}
			if err := s.ScanNow(context.Background()); !errors.Is(err, tc.want) {
				t.Fatalf("ScanNow = %v, want %v", err, tc.want)
			}
			time.Sleep(20 * time.Millisecond)
			if got := analysed.Load(); got != 0 {
				t.Errorf("a sweep that did not start analysed %d flow(s)", got)
			}
		})
	}
	if err := New(nil, nil, nil, nil, 0, nil).ScanNow(context.Background()); !errors.Is(err, ErrScanUnavailable) {
		t.Errorf("ScanNow without a store = %v, want ErrScanUnavailable", err)
	}
}

// TestScanNow_StartsTheSweepInTheBackground: with the lock free ScanNow returns as soon as the sweep has started; the
// sweep then analyses the library under the lock, which it gives back, and a second ScanNow meanwhile is refused as
// held.
func TestScanNow_StartsTheSweepInTheBackground(t *testing.T) {
	b := &lockingBackend{healthBackend: newBackend()}
	seedFlow(t, b, "f1")
	analysing, release := make(chan struct{}), make(chan struct{})
	s := New(b, b, func(_ context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		close(analysing)
		<-release
		return &models.AnalysisReport{FlowID: doc.ID}, nil
	}, nil, 0, loopbackChannels(b))
	t.Cleanup(s.Stop)

	if err := s.ScanNow(context.Background()); err != nil {
		t.Fatalf("ScanNow with the lock free = %v, want nil", err)
	}
	select {
	case <-analysing:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep ScanNow reported started never analysed the flow")
	}
	if err := s.ScanNow(context.Background()); !errors.Is(err, lifecycle.ErrJobHeld) {
		t.Errorf("a second ScanNow while the sweep runs = %v, want lifecycle.ErrJobHeld", err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for b.released.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the sweep's lock was released %d times, want once", b.released.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// panickingListBackend panics in the first ListFlows: outside scanFlow's per-flow recovery, in the sweep itself.
type panickingListBackend struct {
	*healthBackend
	lists atomic.Int32
}

func (b *panickingListBackend) ListFlows(ctx context.Context, f storageif.FlowFilter) ([]*storageif.FlowDocument, error) {
	if b.lists.Add(1) == 1 {
		panic("list flows exploded")
	}
	return b.healthBackend.ListFlows(ctx, f)
}

// TestScanner_PanicInASweepKeepsTheLoop: a panic in one sweep ends that sweep, not the scanner. It used to end the
// ticker loop for good, logged once, so governance stopped until the next restart (R6).
func TestScanner_PanicInASweepKeepsTheLoop(t *testing.T) {
	b := &panickingListBackend{healthBackend: newBackend()}
	seedFlow(t, b, "f1")
	var analysed atomic.Int32

	s := New(b, nil, countingAnalyze(&analysed), nil, 20*time.Millisecond, loopbackChannels(b))
	s.Start()
	defer s.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for analysed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no sweep ran after the first one panicked (%d list calls): the loop ended", b.lists.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestScanner_StopCancelsAnAdminSweep: the admin "scan now" (ScanNow) starts a sweep outside the periodic loop. Stop
// used to cancel only the loop's sweeps, so at shutdown a manual sweep kept querying while the pool closed (R17).
func TestScanner_StopCancelsAnAdminSweep(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	analysing, done := make(chan struct{}), make(chan struct{})
	s := New(b, nil, func(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		close(analysing)
		<-ctx.Done()
		close(done)
		return nil, ctx.Err()
	}, nil, time.Hour, loopbackChannels(b))

	if err := s.ScanNow(context.Background()); err != nil {
		t.Fatalf("ScanNow: %v", err)
	}
	<-analysing
	s.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an admin-triggered sweep kept running after Stop")
	}
}

// TestScanOnce_RecordsTheSweepInTheJobState: the sweep's runs are recorded in the job state the backend keeps, which
// every replica reads to tell "another replica sweeps" (normal) from "no replica has swept for too long" (overdue).
func TestScanOnce_RecordsTheSweepInTheJobState(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	var analysed atomic.Int32
	s := New(b, nil, countingAnalyze(&analysed), nil, 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	run, err := b.GetJobRun(context.Background(), scanJob.Name())
	if err != nil {
		t.Fatalf("the sweep left no job state: %v", err)
	}
	if run.LastStartedAt.IsZero() || run.LastSuccessAt.Before(run.LastStartedAt) || run.Holder == "" {
		t.Errorf("job state after a complete sweep: %+v", run)
	}
}
