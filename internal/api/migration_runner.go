package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"pad-analyzer/internal/migration"
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
	// locker, when wired (WithLocker), extends the single-run guarantee across
	// replicas: the in-process mutex below only guards one pod, so without it
	// two pods could both pass the guard and run Migrate concurrently (the
	// migrator's skip-if-present check is check-then-act).
	locker GlobalLocker

	mu        sync.Mutex
	running   bool
	startedAt time.Time
	last      *migration.Result
	lastErr   error
}

// GlobalLocker is an optional capability of the destination backend: a
// cross-replica mutual-exclusion lock (the Postgres backend implements it via
// a session-level advisory lock). acquired=false means another holder has the
// key; release must be called exactly once when acquired.
type GlobalLocker interface {
	TryGlobalLock(ctx context.Context, key int64) (release func(), acquired bool, err error)
}

// migrationLockKey namespaces the advisory lock for the local→cloud data
// migration. Arbitrary but stable — every replica must use the same key.
const migrationLockKey int64 = 0x62616b696d696772 // "bakimigr"

// migrationLockTimeout bounds the advisory-lock acquisition round-trip.
const migrationLockTimeout = 5 * time.Second

// NewMigrationRunner wraps a migrator. A nil migrator yields a disabled runner.
func NewMigrationRunner(m *migration.Migrator) *MigrationRunner {
	return &MigrationRunner{migrator: m}
}

// WithLocker wires the cross-replica lock capability (see GlobalLocker).
func (r *MigrationRunner) WithLocker(l GlobalLocker) *MigrationRunner {
	r.locker = l
	return r
}

// Enabled reports whether a migrator is wired (source + dst configured).
func (r *MigrationRunner) Enabled() bool { return r != nil && r.migrator != nil }

// Start launches a migration asynchronously. It returns false (caller responds
// 409) if a migration is already running — in this process or, when a
// GlobalLocker is wired, on any other replica — or false if not configured
// (503).
func (r *MigrationRunner) Start() bool {
	if !r.Enabled() {
		return false
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return false
	}
	r.running = true
	r.startedAt = time.Now()
	r.last = nil
	r.lastErr = nil
	r.mu.Unlock()

	// Cross-replica guard: acquire the advisory lock before launching. A
	// failed acquisition (held elsewhere, or the lock round-trip errored)
	// rejects the start — a migration that can't even reach the DB for the
	// lock would fail anyway, and proceeding without the lock would silently
	// drop the single-run guarantee.
	var release func()
	if r.locker != nil {
		lctx, cancel := context.WithTimeout(context.Background(), migrationLockTimeout)
		rel, acquired, err := r.locker.TryGlobalLock(lctx, migrationLockKey)
		cancel()
		if err != nil || !acquired {
			if err != nil {
				logger.Warn("migration: advisory lock acquisition failed", "err", err)
			} else {
				logger.Info("migration: already running on another replica; start rejected")
			}
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
			return false
		}
		release = rel
	}

	go r.run(release)
	return true
}

// run executes one full migration. The whole run is bounded so a runaway
// migration can't hold resources indefinitely; per-flow time is bounded inside
// the migrator. The final result/error (including panics) are captured for the
// status endpoint. release (the cross-replica lock, may be nil) is dropped
// only after the run fully finishes.
func (r *MigrationRunner) run(release func()) {
	if release != nil {
		defer release()
	}
	// Bound the whole run generously; most migrations finish far sooner, and
	// the per-flow timeout inside the migrator is the primary guard.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()

	res, err := r.runMigrate(ctx)

	r.mu.Lock()
	r.running = false
	r.last = &res
	r.lastErr = err
	r.mu.Unlock()
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
