package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// waitDone polls the runner until the migration finishes (or times out), since
// Start runs the migration asynchronously.
func waitDone(t *testing.T, r *MigrationRunner, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !r.Status().Running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("migration did not finish within timeout")
}

func validFlow(id, name string) *storageif.FlowDocument {
	return &storageif.FlowDocument{
		ID:       id,
		Name:     name,
		Content:  []byte("{}"),
		Metadata: storageif.FlowMetadata{BlockCount: 0},
	}
}

// legacySource lays the flows out as a legacy data directory and opens it read-only, as main does.
func legacySource(t *testing.T, flows ...*storageif.FlowDocument) migration.LegacySource {
	t.Helper()
	d := contract.NewLegacyDir(t)
	for _, f := range flows {
		d.Flow(f)
	}
	src, err := filesystem.OpenLegacySource(d.Root)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestMigrationRunner_DisabledWhenNoMigrator(t *testing.T) {
	r := NewMigrationRunner(nil)
	if r.Enabled() {
		t.Fatal("runner with nil migrator should be disabled")
	}
	if err := r.Start(); !errors.Is(err, ErrMigrationDisabled) {
		t.Fatalf("Start on disabled runner = %v, want ErrMigrationDisabled (503 path)", err)
	}
	st := r.Status()
	if st.Configured {
		t.Fatal("disabled runner Status should report Configured=false")
	}
}

func TestMigrationRunner_StartRunsAndCompletes(t *testing.T) {
	src := legacySource(t, validFlow("f1", "Flow One"))
	dst := testutil.NewFakeBackend()

	r := NewMigrationRunner(migration.New(src, dst))
	if !r.Enabled() {
		t.Fatal("runner should be enabled when migrator is wired")
	}

	if err := r.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	waitDone(t, r, 3*time.Second)

	st := r.Status()
	if st.Running {
		t.Fatal("Status.Running should be false after completion")
	}
	if st.Result == nil {
		t.Fatal("Status.Result should be populated after completion")
	}
	if st.Result.FlowsMigrated != 1 {
		t.Errorf("FlowsMigrated = %d, want 1", st.Result.FlowsMigrated)
	}
	if st.Error != nil {
		t.Errorf("unexpected error: %s", *st.Error)
	}

	// The flow should now exist in the destination.
	if got, err := dst.LoadFlow(context.Background(), "f1"); err != nil {
		t.Errorf("flow not present in destination after migration: %v", err)
	} else if got.Name != "Flow One" {
		t.Errorf("destination flow name = %q, want %q", got.Name, "Flow One")
	}
}

func TestMigrationRunner_DoubleStartRejected(t *testing.T) {
	// Seed many flows so the run is still in flight when we re-Start.
	var flows []*storageif.FlowDocument
	for i := 0; i < 200; i++ {
		flows = append(flows, validFlow("f"+strconv.Itoa(i), "Flow "+strconv.Itoa(i)))
	}
	src := legacySource(t, flows...)
	dst := testutil.NewFakeBackend()

	r := NewMigrationRunner(migration.New(src, dst).WithBatchSize(5))
	if err := r.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	// A second Start while the first is running must be rejected (409 path).
	if err := r.Start(); !errors.Is(err, ErrMigrationRunning) {
		t.Fatalf("second Start while running = %v, want ErrMigrationRunning (409 path)", err)
	}
	waitDone(t, r, 10*time.Second)
}

// TestMigrationRunner_RejectedStartAsKeepsTheRunsAdopter: a second admin's start, refused because a run is going,
// must not hand that run's orphaned flows to the second admin.
func TestMigrationRunner_RejectedStartAsKeepsTheRunsAdopter(t *testing.T) {
	var flows []*storageif.FlowDocument
	for i := 0; i < 200; i++ {
		f := validFlow("f"+strconv.Itoa(i), "Flow "+strconv.Itoa(i))
		f.OwnerID = "ghost" // no such user in the destination: the run's operator adopts it
		flows = append(flows, f)
	}
	dst := testutil.NewFakeBackend()
	r := NewMigrationRunner(migration.New(legacySource(t, flows...), dst).WithBatchSize(1))
	if err := r.StartAs("admin-a"); err != nil {
		t.Fatalf("first StartAs: %v", err)
	}
	if err := r.StartAs("admin-b"); !errors.Is(err, ErrMigrationRunning) {
		t.Fatalf("a second StartAs while running = %v, want ErrMigrationRunning", err)
	}
	waitDone(t, r, 10*time.Second)
	for _, f := range flows {
		got, err := dst.LoadFlow(context.Background(), f.ID)
		if err != nil {
			t.Fatalf("LoadFlow(%s): %v", f.ID, err)
		}
		if got.OwnerID != "admin-a" {
			t.Fatalf("flow %s adopted by %q, want the run's operator admin-a", f.ID, got.OwnerID)
		}
	}
}

// fakeLocker simulates the cross-replica advisory lock: configurable
// acquisition outcome, and records whether release was called.
type fakeLocker struct {
	acquired bool
	err      error
	released chan struct{}
}

func (f *fakeLocker) TryGlobalLock(_ context.Context, _ int64) (func(), bool, error) {
	if f.err != nil || !f.acquired {
		return nil, false, f.err
	}
	return func() { close(f.released) }, true, nil
}

func TestMigrationRunner_LockHeldElsewhereRejectsStart(t *testing.T) {
	src := legacySource(t)
	dst := testutil.NewFakeBackend()
	r := NewMigrationRunner(migration.New(src, dst)).WithLocker(&fakeLocker{acquired: false})

	if err := r.Start(); !errors.Is(err, ErrMigrationRunning) {
		t.Fatalf("Start while another replica holds the lock = %v, want ErrMigrationRunning", err)
	}
	// The in-process running flag must be rolled back so a later Start (once
	// the other replica finishes) is not wedged behind a stale guard.
	if r.Status().Running {
		t.Fatal("running flag not rolled back after rejected lock acquisition")
	}
}

func TestMigrationRunner_LockAcquiredRunsAndReleases(t *testing.T) {
	src := legacySource(t, validFlow("f1", "Flow One"))
	dst := testutil.NewFakeBackend()
	lk := &fakeLocker{acquired: true, released: make(chan struct{})}
	r := NewMigrationRunner(migration.New(src, dst)).WithLocker(lk)

	if err := r.Start(); err != nil {
		t.Fatalf("Start with the lock acquired: %v", err)
	}
	waitDone(t, r, 3*time.Second)

	select {
	case <-lk.released:
		// lock dropped after the run — good
	case <-time.After(2 * time.Second):
		t.Fatal("advisory lock was not released after the migration finished")
	}
}

func TestMigrationStatus_JSONShape(t *testing.T) {
	r := NewMigrationRunner(nil)
	out, err := json.Marshal(r.Status())
	if err != nil {
		t.Fatal(err)
	}
	// A disabled runner reports Configured=false and Running=false (always
	// present — consumers rely on it).
	if string(out) != `{"configured":false,"running":false}` {
		t.Errorf("disabled status JSON = %s, want {\"configured\":false,\"running\":false}", out)
	}
}

// TestMigrationRunner_LockErrorIsNotInProgress: a lock that cannot be asked for (a database error) rejects the start
// as unavailable, not as a run in progress, and leaves no run claimed.
func TestMigrationRunner_LockErrorIsNotInProgress(t *testing.T) {
	r := NewMigrationRunner(migration.New(legacySource(t), testutil.NewFakeBackend())).
		WithLocker(&fakeLocker{err: errors.New("connection refused")})
	if err := r.Start(); !errors.Is(err, ErrMigrationLockUnavailable) || errors.Is(err, ErrMigrationRunning) {
		t.Fatalf("Start with the lock unavailable = %v, want ErrMigrationLockUnavailable", err)
	}
	if r.Status().Running {
		t.Fatal("running flag not rolled back after the lock error")
	}
}

// TestMigrationStart_LockErrorAnswers503: the endpoint answered a database error at the lock with 409 "migration
// already in progress", sending the admin to wait for a run that did not exist (R16). It is a 503: try again.
func TestMigrationStart_LockErrorAnswers503(t *testing.T) {
	rt := newJWTTestRouter(t)
	moduleOf[*AdminHandler](t, rt).runner = NewMigrationRunner(migration.New(legacySource(t), testutil.NewFakeBackend())).
		WithLocker(&fakeLocker{err: errors.New("connection refused")})
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/migration/start", bearer, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("lock error: got %d, want 503 (body: %s)", rr.Code, rr.Body.String())
	}
}

// blockingDestination is a destination whose first call, the migrator's Ping, waits for the run's context: a run
// that stays in flight until something cancels it.
type blockingDestination struct {
	*testutil.FakeBackend
	pinged chan struct{}
}

func (d *blockingDestination) Ping(ctx context.Context) error {
	close(d.pinged)
	<-ctx.Done()
	return ctx.Err()
}

// TestMigrationRunner_StopCancelsTheRun: the hour-long run was a bare goroutine nothing cancelled, so shutdown
// closed the pool under it (R16). Stop (and shutdown's lifecycle.DrainAll) cancels it, waits for it to record its
// end, and refuses later starts.
func TestMigrationRunner_StopCancelsTheRun(t *testing.T) {
	dst := &blockingDestination{FakeBackend: testutil.NewFakeBackend(), pinged: make(chan struct{})}
	r := NewMigrationRunner(migration.New(legacySource(t), dst))
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-dst.pinged:
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not start")
	}
	r.Stop()
	st := r.Status()
	if st.Running || st.Error == nil {
		t.Fatalf("after Stop: running %v error %v, want a cancelled run that has ended", st.Running, st.Error)
	}
	if err := r.Start(); !errors.Is(err, ErrMigrationLockUnavailable) {
		t.Errorf("Start after Stop = %v, want it refused", err)
	}
}
