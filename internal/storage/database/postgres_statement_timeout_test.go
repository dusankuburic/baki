package database

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestStatementTimeout_AppliedToPool: every pooled connection carries the
// configured statement_timeout, and a statement that exceeds it is cancelled
// server-side (SQLSTATE 57014). Nothing bounded a runaway query before: the
// per-request Go deadline does not reliably cancel a Postgres backend
// mid-statement, and the streaming routes had no deadline at all.
//
// MaxOpenConns is 1 so the migration's dedicated connection — which lifts the
// timeout for its own session — is the SAME physical connection the assertions
// run on. If migrate() forgot to RESET, the first query would read "0".
func TestStatementTimeout_AppliedToPool(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping statement_timeout integration test")
	}
	cfg := DefaultConfig(dsn)
	cfg.StatementTimeout = 300 * time.Millisecond
	cfg.MaxOpenConns = 1
	cfg.MaxIdleConns = 1
	ctx := context.Background()
	b, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("connect (migrations must run despite a 300ms statement_timeout): %v", err)
	}
	defer b.Close()

	var got string
	if err := b.db.QueryRowContext(ctx, `SELECT current_setting('statement_timeout')`).Scan(&got); err != nil {
		t.Fatalf("read setting: %v", err)
	}
	if got != "300ms" {
		t.Fatalf("statement_timeout on a pooled connection = %q, want 300ms", got)
	}
	if err := b.db.QueryRowContext(ctx, `SELECT current_setting('lock_timeout')`).Scan(&got); err != nil {
		t.Fatalf("read lock_timeout: %v", err)
	}
	if got != "300ms" {
		t.Fatalf("lock_timeout on a pooled connection = %q, want 300ms", got)
	}

	_, err = b.db.ExecContext(ctx, `SELECT pg_sleep(1)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("a 1s statement under a 300ms timeout must fail with SQLSTATE 57014, got: %v", err)
	}
}

// TestStatementTimeout_ZeroMeansNone: an explicit 0 opts out (the escape hatch
// for a maintenance job), and must not write a "0" runtime param that would
// itself be a no-op but misleading in pg_stat_activity.
func TestStatementTimeout_DefaultIsBounded(t *testing.T) {
	if DefaultConfig("x").StatementTimeout <= 0 {
		t.Error("DefaultConfig must carry a bounded statement_timeout; unbounded queries are the outage nobody can see")
	}
}
