package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/platform/lock"
	"pad-core/logger"
)

// MigrationRunner wraps a *migration.Migrator with run-state tracking so the
// admin HTTP endpoint can start a migration asynchronously and report its
// progress without blocking the request — a large library may take minutes to
// copy. At most one migration runs at a time; a second start is rejected.
//
// A nil Migrator (source/dst not configured) renders the runner disabled and
// the start endpoint reports 503.
type MigrationRunner struct {
	migrator *migration.Migrator
	// leaser, when wired (WithLocker, WithLeaser), extends the single-run
	// guarantee across replicas: the in-process guard below only covers one
	// pod, so without it two pods could both pass the guard and run Migrate
	// concurrently (the migrator's skip-if-present check is check-then-act).
	leaser lock.Leaser

	// job runs the migration (built at the first start, once the lock is wired): on one replica at a time,
	// bounded by migrationRunTimeout, and cancelled at shutdown (lifecycle.DrainAll) or by Stop.
	jobOnce sync.Once
	job     *lifecycle.Singleton

	mu        sync.Mutex
	running   bool
	startedAt time.Time
	last      *migration.Result
	lastErr   error
}

// migrationLockKey namespaces the advisory lock for the local→cloud data
// migration. Arbitrary but stable — every replica must use the same key.
const migrationLockKey int64 = 0x62616b696d696772 // "bakimigr"

// migrationJob is the legacy migration's identity across replicas.
var migrationJob = lifecycle.DeclareJob(lifecycle.JobDecl{
	Name:      "legacy_migration",
	LegacyKey: migrationLockKey,
	Schedule:  "on demand (admin migration start; cloud with PAD_DATA_DIR)",
})

// migrationLockTimeout bounds the advisory-lock acquisition round-trip.
const migrationLockTimeout = 5 * time.Second

// migrationRunTimeout bounds the whole run generously; most migrations finish far sooner, and the per-flow timeout
// inside the migrator is the primary guard.
const migrationRunTimeout = 1 * time.Hour

var (
	// ErrMigrationDisabled: no migrator is wired (503).
	ErrMigrationDisabled = errors.New("migration not configured")
	// ErrMigrationRunning: a migration is running, in this process or on another replica (409).
	ErrMigrationRunning = errors.New("migration already in progress")
	// ErrMigrationLockUnavailable: the cross-replica lock could not be asked for, or the server is shutting down;
	// starting without the lock would drop the single-run guarantee (503).
	ErrMigrationLockUnavailable = errors.New("migration lock unavailable")
)

// NewMigrationRunner wraps a migrator. A nil migrator yields a disabled runner.
func NewMigrationRunner(m *migration.Migrator) *MigrationRunner {
	return &MigrationRunner{migrator: m}
}

// WithLocker wires the cross-replica lock (nil without a database).
func (r *MigrationRunner) WithLocker(l lock.GlobalLocker) *MigrationRunner {
	r.leaser = lock.AsLeaser(l)
	return r
}

// WithLeaser wires the cross-replica lock as leases, which cancel a run whose lock was lost; it replaces WithLocker.
func (r *MigrationRunner) WithLeaser(l lock.Leaser) *MigrationRunner {
	r.leaser = l
	return r
}

// Enabled reports whether a migrator is wired (source + dst configured).
func (r *MigrationRunner) Enabled() bool { return r != nil && r.migrator != nil }

// StartAs begins a run whose orphaned flows are adopted by operatorID — the
// admin who asked for the migration. Users are not migrated, so legacy flows
// name owners the destination does not have; stored as-is they are invisible in
// every library view while the run reports them migrated. It returns
// ErrMigrationDisabled, ErrMigrationRunning (here or on another replica) or
// ErrMigrationLockUnavailable when the run did not start.
func (r *MigrationRunner) StartAs(operatorID string) error { return r.start(&operatorID) }

// Start begins a run without an adopting operator; see StartAs.
func (r *MigrationRunner) Start() error { return r.start(nil) }

// Stop cancels a run in flight and refuses later starts. Shutdown does the same through lifecycle.DrainAll.
func (r *MigrationRunner) Stop() {
	if r.Enabled() {
		r.jobRunner().Stop()
	}
}

func (r *MigrationRunner) jobRunner() *lifecycle.Singleton {
	r.jobOnce.Do(func() {
		r.job = lifecycle.NewSingleton(lifecycle.Job{Spec: migrationJob, Timeout: migrationRunTimeout, Run: r.run}, r.leaser, nil)
	})
	return r.job
}

// start claims the run, then sets the adopting owner: a start rejected because a run is going must not change who
// adopts that run's flows.
func (r *MigrationRunner) start(owner *string) error {
	if !r.Enabled() {
		return ErrMigrationDisabled
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return ErrMigrationRunning
	}
	r.running = true
	if owner != nil {
		r.migrator.WithDefaultOwner(*owner)
	}
	r.startedAt = time.Now()
	r.last = nil
	r.lastErr = nil
	r.mu.Unlock()

	// Cross-replica guard: the job takes its lock before launching. A failed acquisition (held elsewhere, or the
	// lock round-trip errored) rejects the start — a migration that can't even reach the DB for the lock would fail
	// anyway, and proceeding without the lock would silently drop the single-run guarantee.
	lctx, cancel := context.WithTimeout(context.Background(), migrationLockTimeout)
	err := r.jobRunner().RunNow(lctx)
	cancel()
	if err == nil {
		return nil
	}
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
	if errors.Is(err, lifecycle.ErrJobHeld) {
		logger.Info("migration: already running on another replica; start rejected")
		return ErrMigrationRunning
	}
	return fmt.Errorf("%w: %w", ErrMigrationLockUnavailable, err)
}

// run is the job's run: one full migration. The final result/error (including panics) are captured for the status
// endpoint.
func (r *MigrationRunner) run(ctx context.Context) error {
	res, err := r.runMigrate(ctx)

	r.mu.Lock()
	r.running = false
	r.last = &res
	r.lastErr = err
	r.mu.Unlock()
	return err
}

// runMigrate isolates the panic recovery from the mutex bookkeeping.
func (r *MigrationRunner) runMigrate(ctx context.Context) (res migration.Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error("migration panicked", "err", rec)
			err = fmt.Errorf("migration panicked: %v", rec)
		}
	}()
	return r.migrator.Migrate(ctx)
}

// Status reports the current run state for the status endpoint.
func (r *MigrationRunner) Status() MigrationStatus {
	if !r.Enabled() {
		return MigrationStatus{Configured: false}
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	st := MigrationStatus{Configured: true, Running: r.running}
	if r.running {
		st.StartedAt = &r.startedAt
	}
	if r.last != nil {
		st.Result = r.last
	}
	if r.lastErr != nil {
		msg := r.lastErr.Error()
		st.Error = &msg
	}
	return st
}

// MigrationStatus is the JSON shape returned by GET /api/admin/migration/status.
type MigrationStatus struct {
	Configured bool              `json:"configured"`
	Running    bool              `json:"running"`
	StartedAt  *time.Time        `json:"startedAt,omitempty"`
	Result     *migration.Result `json:"result,omitempty"`
	Error      *string           `json:"error,omitempty"`
}
