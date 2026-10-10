package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/lock"
)

// TryGlobalLock acquires a cross-replica mutual-exclusion lock via a Postgres
// session-level advisory lock. It reports acquired=false (nil release) when
// another session already holds the key.
//
// Advisory locks are tied to the SESSION, so the lock is pinned to a dedicated
// pooled connection held open until release. release MUST unlock explicitly
// before returning the connection to the pool — a pooled connection is reused,
// not closed, so without the explicit pg_advisory_unlock the lock would leak
// onto whatever query borrows that session next. If the whole process dies,
// Postgres drops the session and releases the lock automatically, so a crashed
// holder can never wedge the lock permanently.
func (b *PostgresStorageBackend) TryGlobalLock(ctx context.Context, key int64) (release func(), acquired bool, err error) {
	conn, acquired, err := tryAdvisoryLock(ctx, b.db, key)
	if err != nil || !acquired {
		return nil, false, err
	}
	return func() { releaseAdvisoryLock(conn, key) }, true, nil
}

// tryAdvisoryLock takes key on a connection of its own, which it returns pinned when it got the lock.
func tryAdvisoryLock(ctx context.Context, db *sql.DB, key int64) (*sql.Conn, bool, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	if !got {
		_ = conn.Close()
		return nil, false, nil
	}
	return conn, true, nil
}

// releaseAdvisoryLock unlocks the advisory key on its owning session and
// returns the connection to the pool. Uses its own timeout: the caller invokes
// this from cleanup paths whose context may already be cancelled. It reports
// notHeld when the unlock answered that the session did not hold the lock.
func releaseAdvisoryLock(conn *sql.Conn, key int64) (notHeld bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var held bool
	err := conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&held)
	switch {
	case err != nil:
		// The lock may still be held by this session. Pooling the connection would park the lock there and silently
		// stall the job on every replica until the connection is recycled, so discard it (as releaseMigrationLock
		// does): closing the session releases the lock.
		slog.Warn("advisory unlock failed; discarding connection", "key", fmt.Sprintf("%#x", key), "err", err)
		discardConn(conn)
		return false
	case !held:
		// The session no longer held the lock: it was lost while held (another replica may have taken it), or a
		// transaction-mode pooler ran the unlock on another server session, where the lock now leaks. Discard the
		// session either way: whatever it does hold goes with it.
		metrics.RecordAdvisoryUnlockNotHeld()
		slog.Warn("advisory unlock reported not held; discarding connection", "key", fmt.Sprintf("%#x", key))
		discardConn(conn)
		return true
	}
	_ = conn.Close()
	return false
}

// discardConn closes conn's session instead of pooling it.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// leaseHeartbeatInterval is how often a held lease confirms its lock on the pinned session; leaseHeartbeatTimeout
// bounds one confirmation. Vars so tests can shorten them.
var (
	leaseHeartbeatInterval = 15 * time.Second
	leaseHeartbeatTimeout  = 5 * time.Second
)

// leaseHeldSQL asks whether this session holds the advisory lock on a bigint key: pg_locks shows such a key with its
// high half in classid, its low half in objid and objsubid 1. Behind a transaction-mode pooler the statement runs on
// another server session, which does not hold it: that too is a lost lease.
const leaseHeldSQL = `SELECT EXISTS (SELECT 1 FROM pg_locks
	WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid() AND objsubid = 1
	  AND classid = (($1::bigint >> 32) & 4294967295)::oid AND objid = ($1::bigint & 4294967295)::oid)`

// TryLease takes key as a lease (lock.Leaser): the advisory lock of TryGlobalLock, whose pinned session a heartbeat
// checks every leaseHeartbeatInterval. When the session no longer holds the lock, or does not answer within
// leaseHeartbeatTimeout, the lease reports Lost and the session is discarded.
func (b *PostgresStorageBackend) TryLease(ctx context.Context, key int64) (lock.Lease, bool, error) {
	return tryLease(ctx, b.db, key)
}

// dbLeaser leases on a bare pool, for the jobs that only have the pool (the token-blacklist cleanup).
type dbLeaser struct{ db *sql.DB }

func (l dbLeaser) TryLease(ctx context.Context, key int64) (lock.Lease, bool, error) {
	return tryLease(ctx, l.db, key)
}

func tryLease(ctx context.Context, db *sql.DB, key int64) (lock.Lease, bool, error) {
	conn, acquired, err := tryAdvisoryLock(ctx, db, key)
	if err != nil || !acquired {
		return nil, false, err
	}
	l := &advisoryLease{key: key, conn: conn, lost: make(chan struct{}), stop: make(chan struct{})}
	go l.heartbeat(leaseHeartbeatInterval)
	return l, true, nil
}

// advisoryLease is a held advisory lock and its pinned session.
type advisoryLease struct {
	key int64
	// mu serializes the heartbeat's statement and Release's unlock on conn, which is nil once released or discarded.
	mu   sync.Mutex
	conn *sql.Conn

	lost     chan struct{}
	lostOnce sync.Once
	stop     chan struct{}
	stopOnce sync.Once
}

func (l *advisoryLease) Lost() <-chan struct{} { return l.lost }

func (l *advisoryLease) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		if !l.confirm() {
			return
		}
	}
}

// confirm asks the pinned session whether it still holds the lock. Not holding it, or not answering, loses the
// lease: the session is discarded (a session that does still hold the lock gives it up with the session).
func (l *advisoryLease) confirm() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return false // released
	}
	ctx, cancel := context.WithTimeout(context.Background(), leaseHeartbeatTimeout)
	defer cancel()
	var held bool
	err := l.conn.QueryRowContext(ctx, leaseHeldSQL, l.key).Scan(&held)
	if err == nil && held {
		return true
	}
	metrics.RecordAdvisoryLeaseLost()
	slog.Warn("advisory lease lost; discarding its session", "key", fmt.Sprintf("%#x", l.key), "held", held, "err", err)
	discardConn(l.conn)
	l.conn = nil
	l.markLost()
	return false
}

func (l *advisoryLease) markLost() { l.lostOnce.Do(func() { close(l.lost) }) }

// Release unlocks and pools the session, or discards it when the unlock fails or reports the lock not held; the
// latter also marks the lease lost. Idempotent.
func (l *advisoryLease) Release() {
	l.stopOnce.Do(func() { close(l.stop) })
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return
	}
	if releaseAdvisoryLock(l.conn, l.key) {
		l.markLost()
	}
	l.conn = nil
}
