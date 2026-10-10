package app

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

	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
	"pad-analyzer/internal/platform/secret"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// leaseLocker is the cross-replica lock in both of its forms, so a test tells which one a job takes: TryLease, the
// database's lease, whose heartbeat ends the run when the lock is lost, or TryGlobalLock, the plain lock, which
// cannot tell. It is free, held by another replica (held), or cannot be asked for (err).
type leaseLocker struct {
	held bool
	err  error

	mu        sync.Mutex
	leaseKeys []int64
	lockKeys  []int64
	leases    []*testLease
	released  atomic.Int32
}

func (l *leaseLocker) TryLease(_ context.Context, key int64) (lock.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.leaseKeys = append(l.leaseKeys, key)
	if l.err != nil || l.held {
		return nil, false, l.err
	}
	lease := &testLease{lost: make(chan struct{}), released: &l.released}
	l.leases = append(l.leases, lease)
	return lease, true, nil
}

func (l *leaseLocker) TryGlobalLock(_ context.Context, key int64) (func(), bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lockKeys = append(l.lockKeys, key)
	if l.err != nil || l.held {
		return nil, false, l.err
	}
	return func() { l.released.Add(1) }, true, nil
}

// asked returns the keys asked for as leases and as plain locks.
func (l *leaseLocker) asked() (leases, locks []int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]int64(nil), l.leaseKeys...), append([]int64(nil), l.lockKeys...)
}

// lastLease is the lease handed out last; nil when none was.
func (l *leaseLocker) lastLease() *testLease {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.leases) == 0 {
		return nil
	}
	return l.leases[len(l.leases)-1]
}

// testLease is a lease the test loses on demand, as the heartbeat does when the database dropped the lock.
type testLease struct {
	lost     chan struct{}
	once     sync.Once
	released *atomic.Int32
	release  sync.Once
}

func (l *testLease) Lost() <-chan struct{} { return l.lost }
func (l *testLease) lose()                 { l.once.Do(func() { close(l.lost) }) }
func (l *testLease) Release()              { l.release.Do(func() { l.released.Add(1) }) }

// jobKey is the lock key the registry declares for a job, read from the registry rather than copied by hand.
func jobKey(t *testing.T, name string) int64 {
	t.Helper()
	for _, j := range lifecycle.Jobs() {
		if j.Name == name {
			return j.Key
		}
	}
	t.Fatalf("no job %q is declared", name)
	return 0
}

// onlyLeases fails unless every key asked for was asked as a lease, each the job's.
func onlyLeases(t *testing.T, l *leaseLocker, key int64) {
	t.Helper()
	leases, locks := l.asked()
	if len(locks) != 0 {
		t.Errorf("the job took the plain lock (keys %#x), which cannot tell when the lock is lost; want the lease", locks)
	}
	if len(leases) == 0 {
		t.Fatal("the job never asked for its lease")
	}
	for _, k := range leases {
		if k != key {
			t.Errorf("the job asked for lease %#x, want %#x", k, key)
		}
	}
}

// logCapture is slog's default output during a test.
type logCapture struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// lines returns the lines logged with the message msg.
func (l *logCapture) lines(msg string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.b.String(), "\n") {
		if strings.Contains(line, "msg=\""+msg+"\"") {
			out = append(out, line)
		}
	}
	return out
}

// captureLogs sends slog's default logger, which the jobs log through, to a buffer for the test.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	l := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return l
}

// serverStoreConfig is a deployment on Postgres in mode, with or without accounts.
func serverStoreConfig(mode config.DeploymentMode, accounts bool) *config.Config {
	cfg := config.Default()
	cfg.Mode = mode
	cfg.Auth.Enabled = accounts
	cfg.Storage.Backend = config.StorageDatabase
	return cfg
}

// cancelled records the cause that ended a run's context.
type cancelled struct {
	started chan struct{}
	once    sync.Once
	cause   chan error
}

func newCancelled() *cancelled {
	return &cancelled{started: make(chan struct{}), cause: make(chan error, 1)}
}

// wait blocks like a long run, until its context ends.
func (c *cancelled) wait(ctx context.Context) {
	c.once.Do(func() { close(c.started) })
	<-ctx.Done()
	c.cause <- context.Cause(ctx)
}

// lostEndsTheRun waits for the run to start, loses its lease, and expects the run's context to end for that.
func (c *cancelled) lostEndsTheRun(t *testing.T, l *leaseLocker) {
	t.Helper()
	select {
	case <-c.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never started")
	}
	lease := l.lastLease()
	if lease == nil {
		t.Fatal("the run holds no lease: it took the plain lock, which cannot tell when the lock is lost")
	}
	lease.lose()
	select {
	case cause := <-c.cause:
		if !errors.Is(cause, lifecycle.ErrLeaseLost) {
			t.Errorf("the run's context ended with %v, want lifecycle.ErrLeaseLost", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run went on after its lease was lost: another replica may already run the job")
	}
}

// blockingPurgeBackend is a server store whose retention purge runs until its context ends; the rest is the fake
// backend, which keeps the job's run state.
type blockingPurgeBackend struct {
	*testutil.FakeBackend
	run *cancelled
}

func (b *blockingPurgeBackend) PurgeExpiredData(ctx context.Context, _ int) (*storageif.PurgeResult, error) {
	b.run.wait(ctx)
	return nil, ctx.Err()
}

// cancelledEncryptor is the field encryptor whose backfill and re-seal passes run until their context ends.
type cancelledEncryptor struct {
	fakeResealer
	run *cancelled
}

func (e *cancelledEncryptor) EncryptOrgChannelsAtRest(ctx context.Context) (int, error) {
	e.run.wait(ctx)
	return 0, ctx.Err()
}

func (e *cancelledEncryptor) ResealFields(ctx context.Context) (storagedb.ResealReport, error) {
	e.run.wait(ctx)
	return storagedb.ResealReport{}, ctx.Err()
}

// TestAppJobs_LeaseLostCancelsTheRun: the jobs the app hosts run under the database's lease, whose heartbeat ends
// the run when the lock is lost (a connection cut, a failover): another replica may take the lock and run the job
// beside it. Under the plain lock they ran on to the end.
func TestAppJobs_LeaseLostCancelsTheRun(t *testing.T) {
	t.Run("retention purge", func(t *testing.T) {
		run, l := newCancelled(), &leaseLocker{}
		lc := fxtest.NewLifecycle(t)
		initRetentionPurge(lc, serverStoreConfig(config.ModeCloud, true), &blockingPurgeBackend{FakeBackend: testutil.NewFakeBackend(), run: run}, l)
		lc.RequireStart()
		defer lc.RequireStop()
		run.lostEndsTheRun(t, l)
	})

	t.Run("field encryption backfill", func(t *testing.T) {
		cfg := config.Default()
		cfg.Auth.EncryptionKey = secret.New("current")
		run, l := newCancelled(), &leaseLocker{}
		lc := fxtest.NewLifecycle(t)
		scheduleFieldEncryptionBackfill(lc, cfg, &cancelledEncryptor{run: run}, l, nil)
		lc.RequireStart()
		defer lc.RequireStop()
		run.lostEndsTheRun(t, l)
	})

	t.Run("secret re-seal", func(t *testing.T) {
		cfg := config.Default()
		cfg.Auth.EncryptionKey = secret.New("current")
		cfg.Auth.EncryptionKeyPrevious = []secret.Value{secret.New("previous")}
		run, l := newCancelled(), &leaseLocker{}
		lc := fxtest.NewLifecycle(t)
		scheduleSecretReseal(lc, cfg, &cancelledEncryptor{run: run}, nil, l, nil)
		lc.RequireStart()
		defer lc.RequireStop()
		run.lostEndsTheRun(t, l)
	})
}

// quickEncryptor is the field encryptor whose passes return at once.
type quickEncryptor struct{ fakeResealer }

// TestAppJobs_RecordTheirRunsAndRunAloneWithoutALeaser: each job writes its run to the persisted job state (a
// job_runs row named after it, with this replica as the holder), and without a leaser (no database behind the test
// fakes, or the desktop) the process runs it alone rather than skipping it.
func TestAppJobs_RecordTheirRunsAndRunAloneWithoutALeaser(t *testing.T) {
	state := testutil.NewFakeBackend()
	cfg := serverStoreConfig(config.ModeCloud, true)
	cfg.Auth.EncryptionKey = secret.New("current")
	cfg.Auth.EncryptionKeyPrevious = []secret.Value{secret.New("previous")}
	fe := &quickEncryptor{}
	lc := fxtest.NewLifecycle(t)
	initRetentionPurge(lc, cfg, state, nil)
	scheduleFieldEncryptionBackfill(lc, cfg, fe, nil, state)
	scheduleSecretReseal(lc, cfg, fe, fe, nil, state)
	lc.RequireStart()
	for _, name := range []string{"retention_purge", "field_encryption_backfill", "secret_reseal"} {
		waitUntil(t, name+" to record a success", func() bool {
			run, err := state.GetJobRun(context.Background(), name)
			return err == nil && !run.LastSuccessAt.IsZero()
		})
		run, _ := state.GetJobRun(context.Background(), name)
		if run.Holder == "" || run.LastError != "" {
			t.Errorf("%s: job_runs row = %+v, want a holder and no error", name, run)
		}
	}
	lc.RequireStop()
	if fe.calls.Load() != 2 {
		t.Errorf("re-seal passes ran %d time(s) without a leaser, want 2 (fields and keys, alone)", fe.calls.Load())
	}
}
