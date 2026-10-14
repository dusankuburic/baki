package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/lock"
	"pad-core/logger"
)

// JobState is the persisted run state a Singleton writes and reads, one record per job name. The storage backend
// provides it (storage/interfaces.JobRunStore has these methods; that package cannot be imported from here, since
// it depends on internal/auth, which uses this package).
type JobState interface {
	// RecordJobStart records that holder started a run of the job at at.
	RecordJobStart(ctx context.Context, name, holder string, at time.Time) error
	// RecordJobEnd records the end of a run at at: a success when failure is "", else a failure with that error.
	RecordJobEnd(ctx context.Context, name string, at time.Time, failure string) error
	// JobLastSuccess returns when the job last succeeded on any replica; zero when it never did.
	JobLastSuccess(ctx context.Context, name string) (time.Time, error)
}

// Job is what a Singleton runs: a declared job, its schedule and its work (ADR 0023).
type Job struct {
	Spec *JobSpec
	// Every > 0 runs the job periodically; 0 runs it only at Start (RunAtStart) and through RunOnce and RunNow.
	Every time.Duration
	// RunAtStart runs the job once as soon as Start is called, before the first period.
	RunAtStart bool
	// Timeout bounds each run the Singleton starts itself (scheduled, RunNow); 0 leaves it to Stop. RunOnce runs
	// under its caller's context instead.
	Timeout time.Duration
	// RetryOnLockError schedules the next attempt this soon after a lock error, when sooner than Every: the job
	// fails closed but need not wait a whole period (or, for a once-per-boot job, the next boot). 0: no retry.
	RetryOnLockError time.Duration
	// OverdueAfter is how long the job may go without a successful run on any replica before it is reported
	// overdue; 0 is twice Every, and a job without a period is never overdue.
	OverdueAfter time.Duration
	// Run does the work. ctx ends on Timeout, on Stop and when the lease is lost (context.Cause: ErrLeaseLost); a
	// run must stop promptly then, since another replica may already be running the job. A panic ends the run, not
	// the schedule.
	Run func(ctx context.Context) error
}

var (
	// ErrJobHeld: another holder has the job's lock (another replica, or a run of this process); the attempt is
	// skipped.
	ErrJobHeld = errors.New("job is already running")
	// ErrJobLockUnavailable: the lock could not be asked for (a database error). The attempt is skipped: a job
	// fails closed, since running without the lock would let every replica run it at once.
	ErrJobLockUnavailable = errors.New("job lock unavailable")
	// ErrJobStopped: the Singleton was stopped; it starts no run any more.
	ErrJobStopped = errors.New("job stopped")
	// ErrLeaseLost is the cause of a run's cancellation when its cross-replica lock was lost while it ran.
	ErrLeaseLost   = errors.New("job lease lost")
	errJobPanicked = errors.New("job panicked")
)

// The log messages operators alert on (KQL over the console logs): fixed strings, never reworded.
const (
	// LogJobOverdue (WARN): no successful run on any replica for longer than the job's overdue threshold. Logged
	// when the job turns overdue, then at most once an hour per replica while it stays overdue.
	LogJobOverdue = "job overdue"
	// LogJobLeaseLost (WARN): a run's cross-replica lock was lost while it ran; the run was cancelled, or the unlock
	// found the lock gone.
	LogJobLeaseLost = "job lease lost"
	// LogJobLockUnavailable (WARN): an attempt was skipped because the lock could not be asked for.
	LogJobLockUnavailable = "job skipped: lock unavailable"
)

const (
	// lockTimeout bounds one lock acquisition.
	lockTimeout = 10 * time.Second
	// stateTimeout bounds one read or write of the persisted job state.
	stateTimeout = 5 * time.Second
	// overdueRelog spaces the repeats of the overdue line while a job stays overdue.
	overdueRelog = time.Hour
	// maxStoredError bounds the error text kept in the job state.
	maxStoredError = 1024
)

// Singleton runs one job at a time across every replica of the deployment: each attempt takes the job's lease
// (lock.Leaser) and skips when another holder has it or when the lock cannot be asked for (fail closed); without a
// leaser (no database, the desktop) the process runs it alone. A run's context ends when its lease is lost. Each run
// is recorded in the persisted job state (when there is a store), which lets any replica report a job overdue when
// no replica has run it successfully for too long. A panic ends the run, not the schedule.
//
// Build one per job per process with NewSingleton; it joins the process-wide group, so DrainAll stops it at shutdown
// if its owner did not.
type Singleton struct {
	job     Job
	name    string
	leaser  lock.Leaser
	state   JobState
	group   *Group
	created time.Time

	// now is the clock of the overdue check and the recorded times (tests set it).
	now func() time.Time

	// root ends at Stop (cause ErrJobStopped): every run's context derives from it.
	root   context.Context
	cancel context.CancelCauseFunc
	stopCh chan struct{}

	mu       sync.Mutex // guards the fields below; inflight.Add happens under it
	running  bool
	started  bool
	stopped  bool
	loopDone chan struct{}
	inflight sync.WaitGroup

	omu           sync.Mutex // guards the overdue bookkeeping below
	lastSuccess   time.Time  // this process's own last successful run
	misses        int        // attempts of this process since its own last success
	overdue       bool
	overdueLogged time.Time
}

// NewSingleton returns the job's runner in the process-wide group. leaser nil: the process runs the job alone; state
// nil: nothing is persisted and the overdue check sees this process only.
func NewSingleton(job Job, leaser lock.Leaser, state JobState) *Singleton {
	return defaultGroup.NewSingleton(job, leaser, state)
}

// NewSingleton returns the job's runner in g.
func (g *Group) NewSingleton(job Job, leaser lock.Leaser, state JobState) *Singleton {
	if job.Spec == nil || job.Run == nil {
		panic("lifecycle.NewSingleton: a job needs a declared spec and a Run func")
	}
	root, cancel := context.WithCancelCause(context.Background())
	s := &Singleton{
		job:     job,
		name:    job.Spec.Name(),
		leaser:  leaser,
		state:   state,
		group:   g,
		created: time.Now(),
		now:     time.Now,
		root:    root,
		cancel:  cancel,
		stopCh:  make(chan struct{}),
	}
	g.mu.Lock()
	g.jobs = append(g.jobs, s)
	g.mu.Unlock()
	return s
}

// Start launches the schedule: a run at once when RunAtStart, then one every Every. A no-op for a job with nothing
// to schedule, after Stop, and on a second call.
func (s *Singleton) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped || (s.job.Every <= 0 && !s.job.RunAtStart) {
		return
	}
	s.started = true
	s.loopDone = make(chan struct{})
	go s.loop(s.loopDone)
}

func (s *Singleton) loop(done chan struct{}) {
	defer close(done)
	var tick <-chan time.Time
	if s.job.Every > 0 {
		t := time.NewTicker(s.job.Every)
		defer t.Stop()
		tick = t.C
	}
	var retry <-chan time.Time
	attempt := func() {
		retry = nil
		err := s.attempt(s.root, s.job.Timeout)
		if r := s.job.RetryOnLockError; errors.Is(err, ErrJobLockUnavailable) && r > 0 && (s.job.Every <= 0 || r < s.job.Every) {
			retry = time.After(r)
		}
	}
	if s.job.RunAtStart {
		attempt()
	}
	for tick != nil || retry != nil {
		select {
		case <-s.stopCh:
			return
		case <-tick:
		case <-retry:
		}
		attempt()
	}
}

// RunOnce makes one attempt now and waits for it: the run's context derives from ctx (and ends at Stop or when the
// lease is lost). It returns ErrJobHeld or ErrJobLockUnavailable when the attempt was skipped, ErrJobStopped after
// Stop, else the run's error.
func (s *Singleton) RunOnce(ctx context.Context) error {
	return s.attempt(ctx, 0)
}

// RunNow takes the lease under ctx and, when it got it, starts the run in the background, bounded by Timeout and
// ended by Stop, and returns nil: the run's Run is then always called. Otherwise it returns why it did not start
// (ErrJobHeld, ErrJobLockUnavailable, ErrJobStopped, or ctx's error). For an operator's "run it now".
func (s *Singleton) RunNow(ctx context.Context) error {
	if err := s.claim(); err != nil {
		s.skipped(err)
		return err
	}
	lease, err := s.acquire(ctx)
	if err != nil {
		s.unclaim()
		s.skipped(err)
		return err
	}
	go func() {
		defer s.unclaim()
		_ = s.execute(s.root, lease, s.job.Timeout)
	}()
	return nil
}

// Stop ends the schedule, cancels the run in flight (whoever started it) and waits for it, at most the stop drain
// budget: the caller's dependencies (the database pool) must outlive the run's unwinding. Idempotent, and safe
// without Start.
func (s *Singleton) Stop() {
	s.halt()
	ctx, cancel := context.WithTimeout(context.Background(), stopDrainBudget)
	defer cancel()
	if err := s.wait(ctx); err != nil {
		logger.Warn("job did not stop within its budget; abandoning the run", "job", s.name, "budget", stopDrainBudget)
	}
}

// halt ends the schedule and cancels the run in flight without waiting, and leaves the group.
func (s *Singleton) halt() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	s.cancel(ErrJobStopped)
	close(s.stopCh)
	s.group.removeJob(s)
}

// wait waits for the schedule and the run in flight to end, or for ctx.
func (s *Singleton) wait(ctx context.Context) error {
	s.mu.Lock()
	loopDone := s.loopDone
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		if loopDone != nil {
			<-loopDone
		}
		s.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attempt is one synchronous attempt: claim the run in this process, take the lease, run.
func (s *Singleton) attempt(ctx context.Context, timeout time.Duration) error {
	if err := s.claim(); err != nil {
		s.skipped(err)
		return err
	}
	defer s.unclaim()
	lease, err := s.acquire(ctx)
	if err != nil {
		s.skipped(err)
		return err
	}
	return s.execute(ctx, lease, timeout)
}

// claim marks a run of this process in flight: one at a time, also without a leaser.
func (s *Singleton) claim() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.stopped:
		return ErrJobStopped
	case s.running:
		return ErrJobHeld
	}
	s.running = true
	s.inflight.Add(1)
	return nil
}

func (s *Singleton) unclaim() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
	s.inflight.Done()
}

// acquire takes the job's lease; nil without a leaser.
func (s *Singleton) acquire(ctx context.Context) (lock.Lease, error) {
	if s.leaser == nil {
		return nil, nil
	}
	lctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	lease, acquired, err := s.leaser.TryLease(lctx, s.job.Spec.Key())
	switch {
	case err != nil && s.root.Err() != nil:
		return nil, ErrJobStopped
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err() // the caller gave up: not the lock's fault
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrJobLockUnavailable, err)
	case !acquired || lease == nil:
		return nil, ErrJobHeld
	}
	return lease, nil
}

// skipped accounts for an attempt that did not run.
func (s *Singleton) skipped(err error) {
	switch {
	case errors.Is(err, ErrJobHeld):
		metrics.RecordJobSkipped(s.name, metrics.JobSkipHeld)
		logger.Debug("job skipped: running elsewhere", "job", s.name)
	case errors.Is(err, ErrJobLockUnavailable):
		metrics.RecordJobSkipped(s.name, metrics.JobSkipLockError)
		logger.Warn(LogJobLockUnavailable, "job", s.name, "err", err)
	default: // stopped, or the caller's context ended
		return
	}
	s.miss()
}

// execute runs the job under its lease and records the run.
func (s *Singleton) execute(parent context.Context, lease lock.Lease, timeout time.Duration) error {
	runCtx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	stopWatch := context.AfterFunc(s.root, func() { cancel(context.Cause(s.root)) })
	defer stopWatch()
	if lease != nil {
		if lost := lease.Lost(); lost != nil {
			go func() {
				select {
				case <-lost:
					cancel(ErrLeaseLost)
				case <-runCtx.Done():
				}
			}()
		}
	}
	ctx := runCtx
	if timeout > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(runCtx, timeout)
		defer cancelTimeout()
	}

	started := time.Now()
	s.recordStart(s.now())
	panicked, err := s.call(ctx)
	took := time.Since(started)
	lost := false
	if lease != nil {
		lost = closed(lease.Lost())
		lease.Release()
		lost = lost || closed(lease.Lost())
	}

	outcome := metrics.JobOutcomeOK
	switch {
	case panicked:
		outcome = metrics.JobOutcomePanic
	case lost:
		outcome = metrics.JobOutcomeLeaseLost
	case err != nil:
		outcome = metrics.JobOutcomeFailed
	}
	metrics.RecordJobRun(s.name, outcome, took)
	if lost {
		logger.Warn(LogJobLeaseLost, "job", s.name, "took", took.Round(time.Millisecond), "err", err)
	}
	end := s.now()
	if err == nil {
		s.recordEnd(end, "")
		s.succeeded(end)
		return nil
	}
	switch {
	case panicked || lost:
		// Reported above (the panic by errreport).
	case errors.Is(context.Cause(runCtx), ErrJobStopped):
		logger.Info("job run interrupted by shutdown", "job", s.name, "took", took.Round(time.Millisecond))
	default:
		logger.Warn("job run failed", "job", s.name, "took", took.Round(time.Millisecond), "err", err)
	}
	s.recordEnd(end, err.Error())
	s.miss()
	return err
}

// call runs the job, turning a panic into an error.
func (s *Singleton) call(ctx context.Context) (panicked bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			errreport.CapturePanic(ctx, "job."+s.name, r, debug.Stack(), errreport.Attrs{"job": s.name})
			panicked, err = true, fmt.Errorf("%w: %v", errJobPanicked, r)
		}
	}()
	return false, s.job.Run(ctx)
}

func closed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// recordStart and recordEnd write the persisted job state. Best effort, on their own short deadline (the end of a
// run cut by Stop is still recorded): a failed write never fails the job, but it leaves the other replicas a stale
// last success, so it is logged.
func (s *Singleton) recordStart(at time.Time) {
	if s.state == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	if err := s.state.RecordJobStart(ctx, s.name, instanceID(), at); err != nil {
		logger.Warn("job state not recorded", "job", s.name, "err", err)
	}
}

func (s *Singleton) recordEnd(at time.Time, failure string) {
	if s.state == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	if err := s.state.RecordJobEnd(ctx, s.name, at, truncateError(failure)); err != nil {
		logger.Warn("job state not recorded", "job", s.name, "err", err)
	}
}

func truncateError(msg string) string {
	if len(msg) <= maxStoredError {
		return msg
	}
	cut := maxStoredError
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

// succeeded records a successful run of this process: the job is not overdue.
func (s *Singleton) succeeded(at time.Time) {
	s.omu.Lock()
	s.lastSuccess = at
	s.misses = 0
	s.omu.Unlock()
	metrics.SetJobLastSuccess(s.name, at)
	s.setOverdue(false, at, at)
}

// miss records an attempt without a success (skipped, failed) and checks whether the job is overdue: the last
// success on any replica (the persisted state, else what this process saw) is older than the threshold. A process
// counts from its own start, so a replica booting after a long downtime does not report the job overdue before
// anyone had the chance to run it.
func (s *Singleton) miss() {
	s.omu.Lock()
	s.misses++
	last := s.lastSuccess
	s.omu.Unlock()

	after := s.job.OverdueAfter
	if after <= 0 {
		after = 2 * s.job.Every
	}
	if after <= 0 || s.root.Err() != nil {
		return
	}
	if s.state != nil {
		ctx, cancel := context.WithTimeout(s.root, stateTimeout)
		stored, err := s.state.JobLastSuccess(ctx, s.name)
		cancel()
		switch {
		case err != nil:
			logger.Debug("job state unreadable; judging overdue by this replica alone", "job", s.name, "err", err)
		case stored.After(last):
			last = stored
		}
	}
	if !last.IsZero() {
		metrics.SetJobLastSuccess(s.name, last)
	}
	since := last
	if s.created.After(since) {
		since = s.created
	}
	now := s.now()
	s.setOverdue(now.Sub(since) > after, last, now, "overdue_after", after.String())
}

// setOverdue sets the gauge and logs the transitions (and the hourly repeat while overdue).
func (s *Singleton) setOverdue(overdue bool, last, now time.Time, extra ...any) {
	metrics.SetJobOverdue(s.name, overdue)
	s.omu.Lock()
	was, misses := s.overdue, s.misses
	s.overdue = overdue
	report := overdue && (!was || now.Sub(s.overdueLogged) >= overdueRelog)
	if report {
		s.overdueLogged = now
	}
	s.omu.Unlock()
	switch {
	case report:
		lastSuccess := "never"
		if !last.IsZero() {
			lastSuccess = last.UTC().Format(time.RFC3339)
		}
		logger.Warn(LogJobOverdue, append([]any{"job", s.name, "last_success", lastSuccess, "attempts_without_success", misses}, extra...)...)
	case was && !overdue:
		logger.Info("job no longer overdue", "job", s.name)
	}
}

var (
	instanceMu sync.RWMutex
	instance   = defaultInstanceID()
)

// SetInstanceID names this replica in the job state's holder column (the deployment's instance id). Call it at boot,
// before the jobs start; the default is the host name and process id.
func SetInstanceID(id string) {
	if id == "" {
		return
	}
	instanceMu.Lock()
	instance = id
	instanceMu.Unlock()
}

// InstanceID is the name SetInstanceID gave this replica (the holder column of the job state, the admin health page).
func InstanceID() string { return instanceID() }

func instanceID() string {
	instanceMu.RLock()
	defer instanceMu.RUnlock()
	return instance
}

func defaultInstanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}
