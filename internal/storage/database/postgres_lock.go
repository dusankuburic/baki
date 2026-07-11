package database

import (
	"context"
	"database/sql"
	"time"
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
	conn, err := b.db.Conn(ctx)
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
	return func() { releaseAdvisoryLock(conn, key) }, true, nil
}

// releaseAdvisoryLock unlocks the advisory key on its owning session and
// returns the connection to the pool. Uses its own timeout: the caller invokes
// this from cleanup paths whose context may already be cancelled.
func releaseAdvisoryLock(conn *sql.Conn, key int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, key)
	_ = conn.Close()
}
