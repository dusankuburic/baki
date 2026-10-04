package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"pad-analyzer/internal/auth"
)

// PostgresBlacklist implements auth.BlacklistStore on a Postgres table, so
// revocation is immediately consistent across replicas (in-memory is per-process).
type PostgresBlacklist struct {
	db       *sql.DB
	stop     chan struct{}
	stopOnce sync.Once
}

func NewPostgresBlacklist(db *sql.DB) *PostgresBlacklist {
	bl := &PostgresBlacklist{db: db, stop: make(chan struct{})}
	go bl.cleanup()
	return bl
}

func (bl *PostgresBlacklist) Add(jti string, ttl time.Duration) {
	expiresAt := time.Now().Add(ttl).UTC()
	// Bound the write as IsRevoked bounds its read: an unbounded write on a
	// stalled DB would block every logout/refresh and exhaust the pool.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := bl.db.ExecContext(ctx,
		`INSERT INTO token_blacklist (jti, expires_at) VALUES ($1, $2)
		 ON CONFLICT (jti) DO UPDATE SET expires_at = GREATEST(token_blacklist.expires_at, EXCLUDED.expires_at)`,
		jti, expiresAt); err != nil {
		slog.Warn("failed to add token to blacklist", "jti", jti, "err", err)
	}
}

// IsRevoked is the fail-closed form of IsRevokedCtx for callers without a
// context: a DB error counts as revoked, so a revoked token is never accepted
// during an outage. The hot path goes through auth.CachedBlacklist.
func (bl *PostgresBlacklist) IsRevoked(jti string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	revoked, err := bl.IsRevokedCtx(ctx, jti)
	if err != nil {
		slog.Warn("blacklist check failed — treating as revoked (fail-closed)", "jti", jti, "err", err)
		return true
	}
	return revoked
}

// IsRevokedCtx reports whether jti is on the blacklist and unexpired. A DB
// error is returned as such — the fail-closed decision belongs to the caller,
// which knows whether it has a local copy to fall back on.
func (bl *PostgresBlacklist) IsRevokedCtx(ctx context.Context, jti string) (bool, error) {
	var tmp int
	err := bl.db.QueryRowContext(ctx,
		`SELECT 1 FROM token_blacklist WHERE jti = $1 AND expires_at > NOW()`, jti).Scan(&tmp)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	}
	return false, fmt.Errorf("blacklist lookup: %w", err)
}

// LoadActive returns every unexpired entry with its expiry — the snapshot
// auth.CachedBlacklist boots from and reconciles against; only short-lived ids.
func (bl *PostgresBlacklist) LoadActive(ctx context.Context) (map[string]time.Time, error) {
	rows, err := bl.db.QueryContext(ctx,
		`SELECT jti, expires_at FROM token_blacklist WHERE expires_at > NOW()`)
	if err != nil {
		return nil, fmt.Errorf("blacklist load: %w", err)
	}
	defer rows.Close()
	active := make(map[string]time.Time)
	for rows.Next() {
		var jti string
		var expiresAt time.Time
		if err := rows.Scan(&jti, &expiresAt); err != nil {
			return nil, fmt.Errorf("blacklist load: %w", err)
		}
		active[jti] = expiresAt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blacklist load: %w", err)
	}
	return active, nil
}

func (bl *PostgresBlacklist) AddIfAbsent(jti string, ttl time.Duration) (bool, error) {
	expiresAt := time.Now().Add(ttl).UTC()
	// Bound the write as IsRevoked bounds its read: an unbounded write on a
	// stalled DB would pile up blocked goroutines and exhaust the pool.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd, err := bl.db.ExecContext(ctx,
		`INSERT INTO token_blacklist (jti, expires_at) VALUES ($1, $2)
		 ON CONFLICT (jti) DO UPDATE SET expires_at = EXCLUDED.expires_at
		 WHERE token_blacklist.expires_at < NOW()`,
		jti, expiresAt)
	if err != nil {
		// Return the error, not a bare false: mistaking a DB error for
		// "already present" would reject now and allow a replay after recovery.
		return false, fmt.Errorf("blacklist insert: %w", err)
	}
	rows, _ := cmd.RowsAffected()
	return rows > 0, nil
}

func (bl *PostgresBlacklist) Stop() {
	bl.stopOnce.Do(func() { close(bl.stop) })
}

// blacklistCleanupLockKey namespaces the cleanup's advisory lock: every
// replica derives the SAME key ("bakiblck") and it collides with no other.
const blacklistCleanupLockKey int64 = 0x62616b69626c636b // "bakiblck"

// blacklistCleanupTimeout bounds one purge cycle: the session-level advisory
// lock pins its pooled connection while held, and 30 s matches the pooled
// statement_timeout that reaps the DELETE server-side anyway.
const blacklistCleanupTimeout = 30 * time.Second

// tryDBAdvisoryLock is TryGlobalLock on a bare *sql.DB with the same
// session-level discipline: the lock is pinned to one dedicated pooled
// connection and released explicitly so it never leaks onto the next borrower.
func tryDBAdvisoryLock(ctx context.Context, db *sql.DB, key int64) (release func(), acquired bool, err error) {
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
	return func() { releaseAdvisoryLock(conn, key) }, true, nil
}

func (bl *PostgresBlacklist) cleanup() {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("blacklist cleanup goroutine panicked", "err", r)
		}
	}()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-bl.stop:
			return
		case <-ticker.C:
			bl.purgeExpired()
		}
	}
}

// purgeExpired deletes one cleanup cycle's expired rows. The advisory lock
// makes the full-table DELETE single-flight across replicas (otherwise N
// replicas ran N identical sweeps); without the lock, skipping silently is
// correct — another replica owns the cycle. Errors wait for the next tick.
func (bl *PostgresBlacklist) purgeExpired() {
	ctx, cancel := context.WithTimeout(context.Background(), blacklistCleanupTimeout)
	defer cancel()
	release, acquired, err := tryDBAdvisoryLock(ctx, bl.db, blacklistCleanupLockKey)
	if err != nil {
		slog.Warn("blacklist cleanup: advisory lock unavailable", "err", err)
		return
	}
	if !acquired {
		return // another replica owns this cycle
	}
	defer release()
	if _, err := bl.db.ExecContext(ctx, `DELETE FROM token_blacklist WHERE expires_at < NOW()`); err != nil {
		slog.Warn("blacklist cleanup: delete failed", "err", err)
	}
}

var (
	_ auth.BlacklistStore   = (*PostgresBlacklist)(nil)
	_ auth.RevocationSource = (*PostgresBlacklist)(nil)
)
