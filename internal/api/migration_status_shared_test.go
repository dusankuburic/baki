package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/migration"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// sharedLocker is the deployment's one cross-replica lock table, handed to every replica of a test.
type sharedLocker struct {
	mu   sync.Mutex
	held map[int64]bool
}

func (l *sharedLocker) TryGlobalLock(_ context.Context, key int64) (func(), bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[int64]bool{}
	}
	if l.held[key] {
		return nil, false, nil
	}
	l.held[key] = true
	return func() {
		l.mu.Lock()
		delete(l.held, key)
		l.mu.Unlock()
	}, true, nil
}

// gatedDestination is a destination whose Ping waits for the gate: a run that stays in flight until the test opens it.
type gatedDestination struct {
	*testutil.FakeBackend
	pinged chan struct{}
	gate   chan struct{}
	once   sync.Once
	fail   error
}

func (d *gatedDestination) Ping(ctx context.Context) error {
	d.once.Do(func() { close(d.pinged) })
	select {
	case <-d.gate:
		return d.fail
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newGated() *gatedDestination {
	return &gatedDestination{FakeBackend: testutil.NewFakeBackend(), pinged: make(chan struct{}), gate: make(chan struct{})}
}

// replicaPair is two replicas of one deployment: separate runners (separate in-memory state), one job store, one lock
// table. Only the first has a destination it can run against.
func replicaPair(t *testing.T, dst *gatedDestination) (store *testutil.FakeBackend, a, b *MigrationRunner) {
	t.Helper()
	store = testutil.NewFakeBackend()
	locks := &sharedLocker{}
	newRunner := func(d storageif.StorageBackend) *MigrationRunner {
		r := NewMigrationRunner(migration.New(legacySource(t, validFlow("f1", "Flow One")), d)).
			WithLocker(locks).WithJobStore(store)
		t.Cleanup(r.Stop)
		return r
	}
	a, b = newRunner(dst), newRunner(testutil.NewFakeBackend())
	b.instance = "replica-b" // the runners share this process's instance id, which the job state records as the holder
	return store, a, b
}

// A replica that did not start the migration answers running:false, result:null (and forgets the result at every
// restart): the status is the database's, which every replica reads.
func TestMigrationStatus_AnotherReplicaSeesTheRunAndItsResult(t *testing.T) {
	dst := newGated()
	_, a, b := replicaPair(t, dst)

	if err := a.StartAs("admin-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-dst.pinged:
	case <-time.After(3 * time.Second):
		t.Fatal("the run did not start")
	}
	st := b.Status()
	if !st.Configured || !st.Running || st.StartedAt == nil || st.Result != nil {
		t.Fatalf("other replica during the run: %+v, want configured, running, started, no result", st)
	}
	// And it cannot start a second one: the lock is held, the answer 409.
	if err := b.Start(); !errors.Is(err, ErrMigrationRunning) {
		t.Errorf("Start on the other replica during the run = %v, want ErrMigrationRunning", err)
	}

	close(dst.gate)
	waitDone(t, a, 5*time.Second)
	waitDone(t, b, 5*time.Second)
	want, got := a.Status(), b.Status()
	if got.Result == nil || got.Result.FlowsMigrated != 1 || got.Running || got.Error != nil {
		t.Fatalf("other replica after the run: %+v, want the result of 1 flow", got)
	}
	wj, _ := json.Marshal(want)
	gj, _ := json.Marshal(got)
	if string(wj) != string(gj) {
		t.Errorf("the replicas disagree:\n runner  %s\n other   %s", wj, gj)
	}
}

// A failed run is the status of every replica too: its error, and the result so far.
func TestMigrationStatus_AnotherReplicaSeesAFailedRun(t *testing.T) {
	dst := newGated()
	dst.fail = errors.New("destination unreachable")
	close(dst.gate)
	_, a, b := replicaPair(t, dst)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, a, 5*time.Second)
	waitDone(t, b, 5*time.Second)
	st := b.Status()
	if st.Running || st.Error == nil || !strings.Contains(*st.Error, "destination unreachable") {
		t.Fatalf("other replica after a failed run: %+v, want the error", st)
	}
	// A later success leaves the failure as history only: the status is the latest run's.
	dst.fail = nil
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, a, 5*time.Second)
	waitDone(t, b, 5*time.Second)
	if st := b.Status(); st.Error != nil || st.Result == nil || st.Result.FlowsMigrated != 1 {
		t.Errorf("other replica after the retry: %+v, want a clean result", st)
	}
}

// A replica that died mid-run leaves started > ended in the row for good: that reads as running only for as long as a
// run may last, then it is a run that never ended.
func TestMigrationStatus_ARunThatNeverEndedIsNotRunningForever(t *testing.T) {
	store, _, b := replicaPair(t, newGated())
	ctx := context.Background()
	if err := store.RecordJobStart(ctx, "legacy_migration", "dead-replica", time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); !st.Running {
		t.Errorf("a run started 10 minutes ago by another replica: %+v, want running", st)
	}
	if err := store.RecordJobStart(ctx, "legacy_migration", "dead-replica", time.Now().Add(-migrationRunTimeout-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); st.Running {
		t.Errorf("a run started over %s ago: %+v, want it not running", migrationRunTimeout, st)
	}
}

// The end of this replica's own run that could not be written leaves the row at "started": this replica knows better.
func TestMigrationStatus_OwnFinishedRunIsNotRunningWhateverTheRowSays(t *testing.T) {
	store, _, b := replicaPair(t, newGated())
	b.instance = "" // this very replica
	if err := store.RecordJobStart(context.Background(), "legacy_migration", lifecycleInstance(), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); st.Running {
		t.Errorf("a row started by this very replica, which runs nothing: %+v, want not running", st)
	}
}

// An unreadable job state falls back to what this replica knows (nothing about a run elsewhere).
func TestMigrationStatus_FallsBackToTheLocalStateWhenTheStoreFails(t *testing.T) {
	store, a, _ := replicaPair(t, newGated())
	store.SetJobRunsError(errors.New("connection refused"))
	st := a.Status()
	if !st.Configured || st.Running || st.Result != nil {
		t.Errorf("status with the job state unreadable: %+v, want the local, idle one", st)
	}
}

// The run is recorded like any job: start, end, holder; and the detail document is the result.
func TestMigrationRunner_RecordsItsRunAndResult(t *testing.T) {
	dst := newGated()
	close(dst.gate)
	store, a, _ := replicaPair(t, dst)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, a, 5*time.Second)
	run, err := store.GetJobRun(context.Background(), "legacy_migration")
	if err != nil {
		t.Fatalf("GetJobRun: %v", err)
	}
	if run.LastStartedAt.IsZero() || run.LastSuccessAt.IsZero() || run.Holder == "" || len(run.Detail) == 0 {
		t.Fatalf("job_runs row after the run: %+v, want start, success, holder and detail", run)
	}
	var res migration.Result
	if err := json.Unmarshal(run.Detail, &res); err != nil || res.FlowsMigrated != 1 {
		t.Errorf("detail %s: %v, want the result with 1 flow", run.Detail, err)
	}
}

// A new run clears the previous result at once: a replica asked during the run must not show the last run's result.
func TestMigrationRunner_ClearsThePreviousResultWhenARunStarts(t *testing.T) {
	dst := newGated()
	store, a, b := replicaPair(t, dst)
	_ = store.RecordJobDetail(context.Background(), "legacy_migration", json.RawMessage(`{"FlowsMigrated":99}`))
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	<-dst.pinged
	if st := b.Status(); st.Result != nil {
		t.Errorf("during a run the other replica shows %+v, want no result", st.Result)
	}
	close(dst.gate)
	waitDone(t, a, 5*time.Second)
}

// The endpoint's answer is the shape it always was: the same keys, now from the database.
func TestMigrationStatus_EndpointShapeIsUnchanged(t *testing.T) {
	rt := newJWTTestRouter(t)
	dst := newGated()
	close(dst.gate)
	_, a, b := replicaPair(t, dst)
	moduleOf[*AdminHandler](t, rt).runner = b
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, a, 5*time.Second)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/migration/status", bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"configured", "running", "result"} {
		if _, ok := body[k]; !ok {
			t.Errorf("response lacks %q: %s", k, rr.Body.String())
		}
	}
	for k := range body {
		switch k {
		case "configured", "running", "startedAt", "result", "error":
		default:
			t.Errorf("response has a new key %q: %s", k, rr.Body.String())
		}
	}
}

func lifecycleInstance() string { return lifecycle.InstanceID() }
