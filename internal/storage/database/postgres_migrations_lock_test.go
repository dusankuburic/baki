package database

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// cancelAfterLockTracer cancels the caller's context the moment
// pg_advisory_lock returns: the lock is then held and every later statement
// of the run fails on the cancelled context — the shape of a boot aborted
// mid-migration.
type cancelAfterLockTracer struct{ cancel context.CancelFunc }

type tracedSQLKey struct{}

func (tr cancelAfterLockTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, tracedSQLKey{}, data.SQL)
}

func (tr cancelAfterLockTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if sqlText, _ := ctx.Value(tracedSQLKey{}).(string); data.Err == nil && strings.Contains(sqlText, "pg_advisory_lock(") {
		tr.cancel()
	}
}

// TestMigrate_CancelledContextStillReleasesTheLock pins an invariant: a
// migration run whose context is cancelled while it holds the session-level
// migration lock must not leave that lock held. Closing a *sql.Conn returns
// it to the pool without ending the session, so a stranded lock would block
// every other replica's migrate (lock_timeout lifted to 0) until the pool
// recycled the connection. Today it holds both through releaseMigrationLock's
// own context and through pgx discarding a session used with a cancelled
// context (driver.ErrBadConn); this keeps it holding if either changes.
func TestMigrate_CancelledContextStillReleasesTheLock(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping Postgres migration-lock test")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.Tracer = cancelAfterLockTracer{cancel: cancel}
	connStr := stdlib.RegisterConnConfig(cfg)
	defer stdlib.UnregisterConnConfig(connStr)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Kept open until the end: the migration's session must stay alive in
	// this pool for a lock it failed to release to still be held.
	defer db.Close()

	b := &PostgresStorageBackend{db: db}
	if err := b.migrate(ctx); err == nil {
		t.Fatal("migrate succeeded although its context was cancelled right after taking the lock")
	}

	// Probe from an independent pool — a different session, since advisory
	// locks are re-entrant within the session that holds them.
	probe, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer probe.Close()
	pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pcancel()
	conn, err := probe.Conn(pctx)
	if err != nil {
		t.Fatalf("probe conn: %v", err)
	}
	defer conn.Close()
	var got bool
	if err := conn.QueryRowContext(pctx, "SELECT pg_try_advisory_lock($1)", migrationLockKey).Scan(&got); err != nil {
		t.Fatalf("pg_try_advisory_lock: %v", err)
	}
	if !got {
		t.Fatal("the migration lock is still held after migrate returned — a pooled session kept it")
	}
	if _, err := conn.ExecContext(pctx, "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
		t.Fatalf("pg_advisory_unlock: %v", err)
	}
}
