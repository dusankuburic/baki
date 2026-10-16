package database

import (
	"context"
	"os"
	"testing"
	"time"

	"pad-analyzer/internal/config"
)

// TestIdleInTxTimeout_MarginCoversTheBlobWait: off a request, the longest a store method keeps its own transaction
// idle is one blob operation (SaveFlow uploads the content between its row write and its commit). The derived
// idle-transaction timeout is the request timeout plus config.DBIdleTxTimeoutMargin, so that wait must fit in the
// margin alone, whatever the request timeout: a background flow copy (the migration runner) must never be cut.
func TestIdleInTxTimeout_MarginCoversTheBlobWait(t *testing.T) {
	if blobOpTimeout >= config.DBIdleTxTimeoutMargin {
		t.Errorf("blobOpTimeout %v is not below config.DBIdleTxTimeoutMargin %v: a store method's own transaction may idle that long on a blob upload", blobOpTimeout, config.DBIdleTxTimeoutMargin)
	}
	if DefaultConfig("x").IdleInTxTimeout != 0 {
		t.Error("DefaultConfig sets an idle-transaction timeout; the backend knows no request deadline, the app derives it (internal/app storageConfig)")
	}
}

// idleTxBackend opens a backend whose sessions end a transaction left idle for longer than idle. MaxOpenConns is 1,
// so the migration's dedicated session (which lifts the timeout for itself) is the same physical connection the
// assertions run on: had the migration not restored the setting, the first read would see 0.
func idleTxBackend(t *testing.T, idle time.Duration) *PostgresStorageBackend {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping idle_in_transaction_session_timeout integration test")
	}
	cfg := DefaultConfig(dsn)
	cfg.IdleInTxTimeout = idle
	cfg.MaxOpenConns = 1
	cfg.MaxIdleConns = 1
	b, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New (migrations must run despite a %v idle-transaction timeout): %v", idle, err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestIdleInTxTimeout_AppliedToPool: every pooled session carries the configured idle_in_transaction_session_timeout,
// and a transaction left idle past it is ended by the server: the session is gone, so the transaction's locks and
// its hold on the vacuum horizon go with it, and the pool replaces the connection.
func TestIdleInTxTimeout_AppliedToPool(t *testing.T) {
	b := idleTxBackend(t, 500*time.Millisecond)
	ctx := context.Background()

	var got string
	if err := b.db.QueryRowContext(ctx, `SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&got); err != nil {
		t.Fatalf("read the setting: %v", err)
	}
	if got != "500ms" {
		t.Fatalf("idle_in_transaction_session_timeout on a pooled session = %q, want 500ms", got)
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	_, err = tx.ExecContext(ctx, `SELECT 1`)
	_ = tx.Rollback()
	if err == nil {
		t.Fatal("a transaction idle for 1.5s under a 500ms idle-transaction timeout is still open")
	}
	t.Logf("the idle transaction's next statement: %v", err)

	// The pool discarded the dead session and serves the next caller on a fresh one.
	if err := b.db.QueryRowContext(ctx, `SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&got); err != nil {
		t.Fatalf("the pool did not recover from the ended session: %v", err)
	}
}

// TestIdleInTxTimeout_SparesLeasesAndLiftedSessions: what must outlive the timeout does. A lease's session holds its
// advisory lock between heartbeats idle but outside any transaction, which the timeout does not count; and a session
// that lifted its timeouts (the migrations, the vector index build) may keep a transaction idle for as long as it
// needs, then gets the configured timeout back.
func TestIdleInTxTimeout_SparesLeasesAndLiftedSessions(t *testing.T) {
	b := idleTxBackend(t, 500*time.Millisecond)
	ctx := context.Background()
	// The lease pins the pool's only connection; the lifted session below needs one of its own.
	b.db.SetMaxOpenConns(2)

	l, acquired, err := b.TryLease(ctx, randomKey())
	if err != nil || !acquired {
		t.Fatalf("TryLease = %v, %v", acquired, err)
	}
	lease := l.(*advisoryLease)
	defer lease.Release()

	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		t.Fatalf("liftSessionTimeouts: %v", err)
	}
	var lifted string
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&lifted); err != nil || lifted != "0" {
		t.Fatalf("lifted session: idle_in_transaction_session_timeout = %q, %v; want 0", lifted, err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	if _, err := tx.ExecContext(ctx, `SELECT 1`); err != nil {
		t.Errorf("a lifted session's transaction idle for 1.5s was ended: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Errorf("commit on the lifted session: %v", err)
	}
	if !lease.confirm() {
		t.Error("a lease's session idle for 1.5s lost its lock under a 500ms idle-transaction timeout")
	}

	restore()
	var restored string
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&restored); err != nil || restored != "500ms" {
		t.Errorf("after restore: idle_in_transaction_session_timeout = %q, %v; want the configured 500ms back", restored, err)
	}
}
