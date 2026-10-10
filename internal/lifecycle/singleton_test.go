package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/lock"
)

// Jobs the tests run: their names label the metrics, so each test asserts its own deltas.
var (
	testJobAlone   = DeclareJob(JobDecl{Name: "test_alone", Schedule: "tests only"})
	testJobLocks   = DeclareJob(JobDecl{Name: "test_locks", Schedule: "tests only"})
	testJobPanic   = DeclareJob(JobDecl{Name: "test_panic", Schedule: "tests only"})
	testJobLease   = DeclareJob(JobDecl{Name: "test_lease", Schedule: "tests only"})
	testJobOverdue = DeclareJob(JobDecl{Name: "test_overdue", Schedule: "tests only"})
	testJobStop    = DeclareJob(JobDecl{Name: "test_stop", Schedule: "tests only"})
	testJobState   = DeclareJob(JobDecl{Name: "test_state", Schedule: "tests only"})
	testJobRetry   = DeclareJob(JobDecl{Name: "test_retry", Schedule: "tests only"})
)

// fakeLease is a lease the test loses on demand, or whose Release finds the lock gone.
type fakeLease struct {
	lost         chan struct{}
	once         sync.Once
	released     atomic.Int32
	goneAtUnlock bool
}

func newFakeLease() *fakeLease { return &fakeLease{lost: make(chan struct{})} }

func (l *fakeLease) Lost() <-chan struct{} { return l.lost }
func (l *fakeLease) lose()                 { l.once.Do(func() { close(l.lost) }) }
func (l *fakeLease) Release() {
	l.released.Add(1)
	if l.goneAtUnlock {
		l.lose()
	}
}

// fakeLeaser answers each TryLease with the next scripted outcome (the last one repeats).
type fakeLeaser struct {
	mu       sync.Mutex
	outcomes []leaseOutcome
	asked    atomic.Int32
}

type leaseOutcome struct {
	lease *fakeLease // nil with err nil: held elsewhere
	err   error
}

func (f *fakeLeaser) TryLease(context.Context, int64) (lock.Lease, bool, error) {
	f.asked.Add(1)
	f.mu.Lock()
	o := f.outcomes[0]
	if len(f.outcomes) > 1 {
		f.outcomes = f.outcomes[1:]
	}
	f.mu.Unlock()
	switch {
	case o.err != nil:
		return nil, false, o.err
	case o.lease == nil:
		return nil, false, nil
	}
	return o.lease, true, nil
}

// fakeState is an in-memory JobState.
type fakeState struct {
	mu          sync.Mutex
	starts      []string // holders
	successes   int
	failures    []string
	lastSuccess time.Time
	err         error
}

func (s *fakeState) RecordJobStart(_ context.Context, _, holder string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, holder)
	return s.err
}

func (s *fakeState) RecordJobEnd(_ context.Context, _ string, at time.Time, failure string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if failure == "" {
		s.successes++
		s.lastSuccess = at
	} else {
		s.failures = append(s.failures, failure)
	}
	return s.err
}

func (s *fakeState) JobLastSuccess(context.Context, string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSuccess, s.err
}

// captureLogs sends slog's default logger to a buffer for the test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) count(msg string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.b.String(), "msg=\""+msg+"\"")
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSingleton_WithoutALeaserRunsAlone(t *testing.T) {
	var runs atomic.Int32
	s := (&Group{}).NewSingleton(Job{Spec: testJobAlone, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}}, nil, nil)
	defer s.Stop()
	if err := s.RunOnce(context.Background()); err != nil || runs.Load() != 1 {
		t.Fatalf("RunOnce without a leaser = %v after %d runs, want one run", err, runs.Load())
	}
	boom := errors.New("boom")
	s2 := (&Group{}).NewSingleton(Job{Spec: testJobAlone, Run: func(context.Context) error { return boom }}, nil, nil)
	defer s2.Stop()
	if err := s2.RunOnce(context.Background()); !errors.Is(err, boom) {
		t.Errorf("RunOnce = %v, want the run's error", err)
	}
}

// TestSingleton_LockErrorFailsClosed: a lock that cannot be asked for skips the run (counted, logged at WARN with
// the fixed message); running without it would let every replica run the job at once. Held elsewhere skips quietly.
func TestSingleton_LockErrorFailsClosed(t *testing.T) {
	logs := captureLogs(t)
	var runs atomic.Int32
	leaser := &fakeLeaser{outcomes: []leaseOutcome{{err: errors.New("connection refused")}, {}}}
	s := (&Group{}).NewSingleton(Job{Spec: testJobLocks, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}}, leaser, nil)
	defer s.Stop()
	lockErrors := metrics.JobSkippedCount(testJobLocks.Name(), metrics.JobSkipLockError)
	held := metrics.JobSkippedCount(testJobLocks.Name(), metrics.JobSkipHeld)

	if err := s.RunOnce(context.Background()); !errors.Is(err, ErrJobLockUnavailable) {
		t.Fatalf("RunOnce with the lock unavailable = %v, want ErrJobLockUnavailable", err)
	}
	if err := s.RunOnce(context.Background()); !errors.Is(err, ErrJobHeld) {
		t.Fatalf("RunOnce with the lock held elsewhere = %v, want ErrJobHeld", err)
	}
	if runs.Load() != 0 {
		t.Fatalf("the job ran %d times without its lock", runs.Load())
	}
	if got := metrics.JobSkippedCount(testJobLocks.Name(), metrics.JobSkipLockError) - lockErrors; got != 1 {
		t.Errorf("skipped{lock_error} rose by %v, want 1", got)
	}
	if got := metrics.JobSkippedCount(testJobLocks.Name(), metrics.JobSkipHeld) - held; got != 1 {
		t.Errorf("skipped{held} rose by %v, want 1", got)
	}
	if got := logs.count(LogJobLockUnavailable); got != 1 {
		t.Errorf("%q logged %d times, want 1", LogJobLockUnavailable, got)
	}
}

// TestSingleton_OneRunAtATimeInAProcess: also without a database, a second start while a run is going is refused.
func TestSingleton_OneRunAtATimeInAProcess(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	s := (&Group{}).NewSingleton(Job{Spec: testJobAlone, Run: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}, nil, nil)
	defer s.Stop()
	if err := s.RunNow(context.Background()); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	<-started
	if err := s.RunNow(context.Background()); !errors.Is(err, ErrJobHeld) {
		t.Errorf("a second RunNow while running = %v, want ErrJobHeld", err)
	}
	if err := s.RunOnce(context.Background()); !errors.Is(err, ErrJobHeld) {
		t.Errorf("RunOnce while running = %v, want ErrJobHeld", err)
	}
	close(release)
}

// TestSingleton_PanicKeepsLoop: a panic ends its run (counted as a panic, reported), and the schedule goes on. The
// scanner's ticker loop used to end for good on one (R6).
func TestSingleton_PanicKeepsLoop(t *testing.T) {
	var runs atomic.Int32
	s := (&Group{}).NewSingleton(Job{Spec: testJobPanic, Every: 5 * time.Millisecond, RunAtStart: true, Run: func(context.Context) error {
		if runs.Add(1) == 1 {
			panic("first run explodes")
		}
		return nil
	}}, nil, nil)
	before := metrics.JobRunCount(testJobPanic.Name(), metrics.JobOutcomePanic)
	s.Start()
	defer s.Stop()
	waitUntil(t, "a run after the panic", func() bool { return runs.Load() >= 3 })
	if got := metrics.JobRunCount(testJobPanic.Name(), metrics.JobOutcomePanic) - before; got != 1 {
		t.Errorf("runs{panic} rose by %v, want 1", got)
	}
}

// TestSingleton_LeaseLostCancelsTheRun: when the lease is lost mid-run the run's context ends with ErrLeaseLost
// (another replica may already run the job), and the run counts as lease_lost.
func TestSingleton_LeaseLostCancelsTheRun(t *testing.T) {
	logs := captureLogs(t)
	lease := newFakeLease()
	running := make(chan struct{})
	var cause error
	s := (&Group{}).NewSingleton(Job{Spec: testJobLease, Run: func(ctx context.Context) error {
		close(running)
		<-ctx.Done()
		cause = context.Cause(ctx)
		return ctx.Err()
	}}, &fakeLeaser{outcomes: []leaseOutcome{{lease: lease}}}, nil)
	defer s.Stop()
	before := metrics.JobRunCount(testJobLease.Name(), metrics.JobOutcomeLeaseLost)

	done := make(chan error, 1)
	go func() { done <- s.RunOnce(context.Background()) }()
	<-running
	lease.lose()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a run cancelled by its lost lease reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run went on after its lease was lost")
	}
	if !errors.Is(cause, ErrLeaseLost) {
		t.Errorf("the run's context ended with cause %v, want ErrLeaseLost", cause)
	}
	if lease.released.Load() != 1 {
		t.Errorf("the lost lease was released %d times, want 1", lease.released.Load())
	}
	if got := metrics.JobRunCount(testJobLease.Name(), metrics.JobOutcomeLeaseLost) - before; got != 1 {
		t.Errorf("runs{lease_lost} rose by %v, want 1", got)
	}
	if got := logs.count(LogJobLeaseLost); got != 1 {
		t.Errorf("%q logged %d times, want 1", LogJobLeaseLost, got)
	}
}

// TestSingleton_UnlockFindingTheLockGoneIsALostLease: a run that completed but whose unlock found the lock gone (the
// R4 case) did its work, so its success is recorded, but it ran unguarded for a while: lease_lost, logged.
func TestSingleton_UnlockFindingTheLockGoneIsALostLease(t *testing.T) {
	logs := captureLogs(t)
	lease := newFakeLease()
	lease.goneAtUnlock = true
	state := &fakeState{}
	s := (&Group{}).NewSingleton(Job{Spec: testJobLease, Run: func(context.Context) error { return nil }},
		&fakeLeaser{outcomes: []leaseOutcome{{lease: lease}}}, state)
	defer s.Stop()
	before := metrics.JobRunCount(testJobLease.Name(), metrics.JobOutcomeLeaseLost)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce = %v, want the completed run's nil", err)
	}
	if got := metrics.JobRunCount(testJobLease.Name(), metrics.JobOutcomeLeaseLost) - before; got != 1 {
		t.Errorf("runs{lease_lost} rose by %v, want 1", got)
	}
	if state.successes != 1 {
		t.Errorf("recorded %d successes, want the completed run's 1", state.successes)
	}
	if got := logs.count(LogJobLeaseLost); got != 1 {
		t.Errorf("%q logged %d times, want 1", LogJobLeaseLost, got)
	}
}

// TestSingleton_OverdueAfterSkips: a replica that keeps skipping because another holds the lock is normal while some
// replica succeeds (the persisted last success is fresh). When the last success anywhere is older than the threshold
// the job is overdue: the gauge, and one WARN line with the fixed message, not one per skip; a success clears it.
func TestSingleton_OverdueAfterSkips(t *testing.T) {
	logs := captureLogs(t)
	state := &fakeState{}
	held := &fakeLeaser{outcomes: []leaseOutcome{{}}}
	const every = time.Hour
	s := (&Group{}).NewSingleton(Job{Spec: testJobOverdue, Every: every, Run: func(context.Context) error { return nil }}, held, state)
	defer s.Stop()
	now := s.created
	s.now = func() time.Time { return now }

	// Another replica succeeded half an hour ago: skipping is normal.
	state.lastSuccess = now.Add(-30 * time.Minute)
	now = now.Add(3 * every)
	state.lastSuccess = now.Add(-30 * time.Minute)
	_ = s.RunOnce(context.Background())
	if metrics.JobOverdueValue(testJobOverdue.Name()) != 0 || logs.count(LogJobOverdue) != 0 {
		t.Fatal("a skip while another replica succeeds recently reported the job overdue")
	}

	// No success anywhere for longer than twice the interval: overdue, reported once for several skips.
	state.lastSuccess = now.Add(-2*every - time.Minute)
	for range 3 {
		_ = s.RunOnce(context.Background())
		now = now.Add(time.Minute)
	}
	if metrics.JobOverdueValue(testJobOverdue.Name()) != 1 {
		t.Fatal("pad_job_overdue is not 1 after skips past the threshold")
	}
	if got := logs.count(LogJobOverdue); got != 1 {
		t.Fatalf("%q logged %d times over three skips, want 1", LogJobOverdue, got)
	}
	// Still overdue an hour later: one more line, for alerts over a window.
	now = now.Add(overdueRelog)
	_ = s.RunOnce(context.Background())
	if got := logs.count(LogJobOverdue); got != 2 {
		t.Errorf("%q logged %d times after an hour more, want 2", LogJobOverdue, got)
	}

	// This replica gets the lock and succeeds: no longer overdue.
	held.mu.Lock()
	held.outcomes = []leaseOutcome{{lease: newFakeLease()}}
	held.mu.Unlock()
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if metrics.JobOverdueValue(testJobOverdue.Name()) != 0 {
		t.Error("pad_job_overdue still 1 after a success")
	}
}

// TestSingleton_OverdueCountsFromTheProcessStart: a replica booting after a long downtime does not report the job
// overdue before anyone had the chance to run it, and one without a persisted state judges by its own runs.
func TestSingleton_OverdueCountsFromTheProcessStart(t *testing.T) {
	logs := captureLogs(t)
	state := &fakeState{lastSuccess: time.Now().Add(-30 * 24 * time.Hour)}
	s := (&Group{}).NewSingleton(Job{Spec: testJobOverdue, Every: time.Hour, Run: func(context.Context) error { return nil }},
		&fakeLeaser{outcomes: []leaseOutcome{{}}}, state)
	defer s.Stop()
	_ = s.RunOnce(context.Background())
	if logs.count(LogJobOverdue) != 0 {
		t.Fatal("a fresh process reported the job overdue from a month-old last success")
	}
	s.now = func() time.Time { return s.created.Add(3 * time.Hour) }
	_ = s.RunOnce(context.Background())
	if logs.count(LogJobOverdue) != 1 {
		t.Error("three hours after the process start, with no success since, the job is not reported overdue")
	}
}

// TestSingleton_StopCancelsAndWaits: Stop cancels a run in flight (cause ErrJobStopped), waits for it, and refuses
// later runs; the group's Drain does the same for the jobs nobody stopped.
func TestSingleton_StopCancelsAndWaits(t *testing.T) {
	for name, stop := range map[string]func(*Group, *Singleton){
		"Stop":  func(_ *Group, s *Singleton) { s.Stop() },
		"Drain": func(g *Group, _ *Singleton) { _ = g.Drain(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			g := &Group{}
			var ended atomic.Bool
			var cause error
			running := make(chan struct{})
			s := g.NewSingleton(Job{Spec: testJobStop, Run: func(ctx context.Context) error {
				close(running)
				<-ctx.Done()
				cause = context.Cause(ctx)
				ended.Store(true)
				return ctx.Err()
			}}, nil, nil)
			if err := s.RunNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			<-running
			stop(g, s)
			if !ended.Load() {
				t.Fatal("stopping returned before the run ended")
			}
			if !errors.Is(cause, ErrJobStopped) {
				t.Errorf("the run's context ended with cause %v, want ErrJobStopped", cause)
			}
			if err := s.RunOnce(context.Background()); !errors.Is(err, ErrJobStopped) {
				t.Errorf("RunOnce after stopping = %v, want ErrJobStopped", err)
			}
			g.mu.Lock()
			left := len(g.jobs)
			g.mu.Unlock()
			if left != 0 {
				t.Errorf("%d stopped jobs left in the group", left)
			}
		})
	}
}

// TestSingleton_RecordsRuns: each run records its start with this replica's instance and its end, success or the
// (bounded) error; a state that cannot be written never fails the job.
func TestSingleton_RecordsRuns(t *testing.T) {
	state := &fakeState{}
	fail := true
	long := strings.Repeat("é", maxStoredError)
	s := (&Group{}).NewSingleton(Job{Spec: testJobState, Run: func(context.Context) error {
		if fail {
			return errors.New(long)
		}
		return nil
	}}, nil, state)
	defer s.Stop()
	SetInstanceID("replica-7")
	defer SetInstanceID(defaultInstanceID())

	_ = s.RunOnce(context.Background())
	fail = false
	_ = s.RunOnce(context.Background())
	if len(state.starts) != 2 || state.starts[0] != "replica-7" {
		t.Errorf("recorded starts %v, want two by replica-7", state.starts)
	}
	if len(state.failures) != 1 || len(state.failures[0]) > maxStoredError+len("…") || !strings.HasSuffix(state.failures[0], "…") {
		t.Errorf("recorded failure of %d bytes, want the error cut at %d bytes", len(state.failures[0]), maxStoredError)
	}
	if state.successes != 1 {
		t.Errorf("recorded %d successes, want 1", state.successes)
	}

	state.err = errors.New("database down")
	if err := s.RunOnce(context.Background()); err != nil {
		t.Errorf("a run whose state cannot be written = %v, want its own nil", err)
	}
}

// TestSingleton_RetriesALockErrorSooner: after a lock error the next attempt comes RetryOnLockError later, not a
// whole period (or, for a once-per-boot job, a boot) later.
func TestSingleton_RetriesALockErrorSooner(t *testing.T) {
	var runs atomic.Int32
	leaser := &fakeLeaser{outcomes: []leaseOutcome{{err: errors.New("connection refused")}, {lease: newFakeLease()}}}
	s := (&Group{}).NewSingleton(Job{Spec: testJobRetry, RunAtStart: true, RetryOnLockError: 5 * time.Millisecond,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		}}, leaser, nil)
	s.Start()
	defer s.Stop()
	waitUntil(t, "the retried run", func() bool { return runs.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Errorf("a once-per-boot job ran %d times after its retry, want 1", got)
	}
	if got := leaser.asked.Load(); got != 2 {
		t.Errorf("the lock was asked for %d times, want 2 (the error, the retry)", got)
	}
}
