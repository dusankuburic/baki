package database

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/lock"
)

// leaseTestJob is the job the lease tests run under real advisory locks.
var leaseTestJob = lifecycle.DeclareJob(lifecycle.JobDecl{Name: "test_postgres_lease", Schedule: "tests only"})

// fastHeartbeat shortens the lease heartbeat for one test.
func fastHeartbeat(t *testing.T) time.Duration {
	t.Helper()
	const every = 50 * time.Millisecond
	old := leaseHeartbeatInterval
	leaseHeartbeatInterval = every
	t.Cleanup(func() { leaseHeartbeatInterval = old })
	return every
}

// randomKey is an advisory key no other test binary on the server uses, negative half the time (derived job keys
// are FNV hashes, often negative).
func randomKey() int64 { return int64(rand.Uint64()) }

// lockHolderPID is the backend that holds key, from another session.
func lockHolderPID(t *testing.T, b *PostgresStorageBackend, key int64) int {
	t.Helper()
	var pid int
	if err := b.db.QueryRowContext(context.Background(), `
		SELECT pid FROM pg_locks
		 WHERE locktype = 'advisory' AND granted AND objsubid = 1
		   AND classid = (($1::bigint >> 32) & 4294967295)::oid AND objid = ($1::bigint & 4294967295)::oid`, key).Scan(&pid); err != nil {
		t.Fatalf("find the session holding %#x: %v", key, err)
	}
	return pid
}

func terminateBackend(t *testing.T, b *PostgresStorageBackend, pid int) {
	t.Helper()
	var ok bool
	if err := b.db.QueryRowContext(context.Background(), `SELECT pg_terminate_backend($1)`, pid).Scan(&ok); err != nil || !ok {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v", pid, ok, err)
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("%s did not happen within %s", what, within)
	}
}

// TestPostgresLease_HeartbeatConfirmsAHeldLock: the heartbeat's pg_locks lookup finds the lock its session holds
// (the key's two halves in classid and objid), for positive and negative keys: a lookup that missed would report
// every lease lost and cancel every job at its first heartbeat.
func TestPostgresLease_HeartbeatConfirmsAHeldLock(t *testing.T) {
	b := openMigrationTestDB(t)
	every := fastHeartbeat(t)
	for _, key := range []int64{0x62616b6974657374, -0x1d2c3b4a59687706, randomKey()} {
		lease, ok, err := b.TryLease(context.Background(), key)
		if err != nil || !ok {
			t.Fatalf("TryLease(%#x) = %v, %v", key, ok, err)
		}
		select {
		case <-lease.Lost():
			t.Fatalf("lease on %#x reported lost while its session held the lock", key)
		case <-time.After(6 * every):
		}
		lease.Release()
		select {
		case <-lease.Lost():
			t.Errorf("a clean release of %#x reported the lease lost", key)
		default:
		}
		again, ok, err := b.TryLease(context.Background(), key)
		if err != nil || !ok {
			t.Fatalf("re-lease of %#x after release = %v, %v", key, ok, err)
		}
		again.Release()
	}
}

// TestPostgresLease_LostWhenConnectionKilled: the pinned session dies (pg_terminate_backend, as a failover or a
// network cut would), so Postgres drops the lock and another replica can take it. The heartbeat notices within its
// interval, and the job running under the lease has its context cancelled with ErrLeaseLost (R5): it used to run on
// beside the next holder.
func TestPostgresLease_LostWhenConnectionKilled(t *testing.T) {
	b := openMigrationTestDB(t)
	every := fastHeartbeat(t)
	key := leaseTestJob.Key()

	running := make(chan struct{})
	var cause atomic.Value
	job := (&lifecycle.Group{}).NewSingleton(lifecycle.Job{Spec: leaseTestJob, Run: func(ctx context.Context) error {
		close(running)
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return ctx.Err()
	}}, b, b)
	defer job.Stop()
	before := metrics.JobRunCount(leaseTestJob.Name(), metrics.JobOutcomeLeaseLost)

	done := make(chan error, 1)
	go func() { done <- job.RunOnce(context.Background()) }()
	waitClosed(t, running, 5*time.Second, "the job's start")
	terminateBackend(t, b, lockHolderPID(t, b, key))

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a run whose lease was lost reported success")
		}
	case <-time.After(20 * every):
		t.Fatal("the job kept running after its lock's session was killed")
	}
	if got, _ := cause.Load().(error); !errors.Is(got, lifecycle.ErrLeaseLost) {
		t.Errorf("the run's context ended with cause %v, want ErrLeaseLost", got)
	}
	if got := metrics.JobRunCount(leaseTestJob.Name(), metrics.JobOutcomeLeaseLost) - before; got != 1 {
		t.Errorf("pad_job_runs_total{outcome=lease_lost} rose by %v, want 1", got)
	}
	// The lock went with the session: another replica takes it at once.
	other, ok, err := b.TryLease(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("the lock was not free after the holder's session died: %v, %v", ok, err)
	}
	other.Release()
}

// TestPostgresLease_LostWhenTheLockIsGoneFromItsSession: a session that is alive but no longer holds the lock (a
// transaction-mode pooler put the unlock elsewhere, or it was unlocked behind the lease's back) is a lost lease too.
// Release then does not unlock anything another holder has.
func TestPostgresLease_LostWhenTheLockIsGoneFromItsSession(t *testing.T) {
	b := openMigrationTestDB(t)
	every := fastHeartbeat(t)
	key := randomKey()
	lease, ok, err := b.TryLease(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("TryLease = %v, %v", ok, err)
	}
	l := lease.(*advisoryLease)
	l.mu.Lock()
	_, err = l.conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
	l.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	before := metrics.AdvisoryLeaseLostCount()
	waitClosed(t, lease.Lost(), 20*every, "the lost lease")
	if got := metrics.AdvisoryLeaseLostCount() - before; got != 1 {
		t.Errorf("pad_advisory_lease_lost_total rose by %v, want 1", got)
	}
	lease.Release() // a no-op: the session is gone
}

// TestSingleton_TwoReplicasOnePostgres: two replicas (two pools on one database) with the same job: while one runs it
// the other skips; when the holder's session dies the holder's run is cancelled, and the next attempt elsewhere runs
// the job, once.
func TestSingleton_TwoReplicasOnePostgres(t *testing.T) {
	a := openMigrationTestDB(t)
	b := openMigrationTestDB(t)
	fastHeartbeat(t)
	key := leaseTestJob.Key()

	var runs atomic.Int32
	running := make(chan struct{}, 2)
	newReplica := func(store *PostgresStorageBackend) *lifecycle.Singleton {
		return (&lifecycle.Group{}).NewSingleton(lifecycle.Job{Spec: leaseTestJob, Run: func(ctx context.Context) error {
			runs.Add(1)
			running <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}}, store, store)
	}
	first, second := newReplica(a), newReplica(b)
	defer first.Stop()
	defer second.Stop()

	if err := first.RunNow(context.Background()); err != nil {
		t.Fatalf("first replica's RunNow: %v", err)
	}
	<-running
	if err := second.RunNow(context.Background()); !errors.Is(err, lifecycle.ErrJobHeld) {
		t.Fatalf("second replica's RunNow while the first runs = %v, want ErrJobHeld", err)
	}

	terminateBackend(t, a, lockHolderPID(t, a, key))
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := second.RunNow(context.Background())
		if err == nil {
			break
		}
		if !errors.Is(err, lifecycle.ErrJobHeld) || time.Now().After(deadline) {
			t.Fatalf("second replica could not take over after the holder died: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	<-running
	if got := runs.Load(); got != 2 {
		t.Errorf("the job ran %d times, want 2 (the holder's, then the takeover)", got)
	}
}

var _ lock.Leaser = (*PostgresStorageBackend)(nil)
