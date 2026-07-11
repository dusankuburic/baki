package api

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"pad-analyzer/internal/migration"
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

func TestMigrationRunner_DisabledWhenNoMigrator(t *testing.T) {
	r := NewMigrationRunner(nil)
	if r.Enabled() {
		t.Fatal("runner with nil migrator should be disabled")
	}
	if r.Start() {
		t.Fatal("Start on disabled runner should return false (503 path)")
	}
	st := r.Status()
	if st.Configured {
		t.Fatal("disabled runner Status should report Configured=false")
	}
}

func TestMigrationRunner_StartRunsAndCompletes(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()

	if err := src.SaveFlow(context.Background(), validFlow("f1", "Flow One")); err != nil {
		t.Fatal(err)
	}

	r := NewMigrationRunner(migration.New(src, dst))
	if !r.Enabled() {
		t.Fatal("runner should be enabled when migrator is wired")
	}

	if !r.Start() {
		t.Fatal("first Start should succeed")
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
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()
	// Seed many flows so the run is still in flight when we re-Start.
	for i := 0; i < 200; i++ {
		_ = src.SaveFlow(context.Background(), validFlow(
			"f"+strconv.Itoa(i), "Flow "+strconv.Itoa(i)))
	}

	r := NewMigrationRunner(migration.New(src, dst).WithBatchSize(5))
	if !r.Start() {
		t.Fatal("first Start should succeed")
	}
	// A second Start while the first is running must be rejected (409 path).
	if r.Start() {
		t.Fatal("second Start while running should return false (409 path)")
	}
	waitDone(t, r, 10*time.Second)
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
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()
	r := NewMigrationRunner(migration.New(src, dst)).WithLocker(&fakeLocker{acquired: false})

	if r.Start() {
		t.Fatal("Start should be rejected when another replica holds the lock")
	}
	// The in-process running flag must be rolled back so a later Start (once
	// the other replica finishes) is not wedged behind a stale guard.
	if r.Status().Running {
		t.Fatal("running flag not rolled back after rejected lock acquisition")
	}
}

func TestMigrationRunner_LockAcquiredRunsAndReleases(t *testing.T) {
	src := testutil.NewFakeBackend()
	dst := testutil.NewFakeBackend()
	if err := src.SaveFlow(context.Background(), validFlow("f1", "Flow One")); err != nil {
		t.Fatal(err)
	}
	lk := &fakeLocker{acquired: true, released: make(chan struct{})}
	r := NewMigrationRunner(migration.New(src, dst)).WithLocker(lk)

	if !r.Start() {
		t.Fatal("Start should succeed when the lock is acquired")
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
