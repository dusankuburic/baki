package database

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// backendWithPool builds a backend whose pool holds exactly n connections.
//
// A pool of 1 turns the defect below from a concurrency race into a
// deterministic, single-goroutine assertion: if the method under test opens a
// SECOND connection while the request's RLS transaction holds the first, it can
// only block. With the production pool of 25 the same bug needs 13 concurrent
// requests to show itself, which is exactly why it survived this long.
func backendWithPool(t *testing.T, n int) *PostgresStorageBackend {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping RLS connection-reuse test")
	}
	cfg := DefaultConfig(dsn)
	cfg.MaxOpenConns = n
	b, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// TestWriteMethods_JoinTheRequestRLSTxInsteadOfOpeningASecondConnection is the
// regression gate for a pool self-deadlock with no self-recovery.
//
// rlsMiddleware pins ONE pooled connection for the whole request. SaveOrg,
// MutateOrg and DeleteUser each called b.db.BeginTx unconditionally, so every
// such request needed TWO of the pool's 25 connections. Thirteen concurrent
// org-member edits exhaust the pool; each request then holds one connection and
// waits forever for another that only a peer can release. It wedges EVERY
// route, not just the one at fault, and the server sets no write timeout
// (deliberately), so nothing unwedges it but clients giving up.
//
// Two further consequences ride along: the inner transaction commits
// independently, so a write survives the middleware's rollback and breaks its
// documented atomicity guarantee; and the fresh connection carries no
// app.current_user_id, so app_rls_active() is false and the RLS policies
// short-circuit to allow-all — losing the second layer entirely.
//
// SaveFlow, SaveFlowVersion, BatchSetFindingStatus and SaveKnowledgeChunks all
// consult the context first. These three did not.
func TestWriteMethods_JoinTheRequestRLSTxInsteadOfOpeningASecondConnection(t *testing.T) {
	b := backendWithPool(t, 1)
	base := context.Background()

	const userID = "rls-reuse-user"
	const orgID = "rls-reuse-org"
	t.Cleanup(func() {
		db := b.DB()
		_, _ = db.ExecContext(base, `DELETE FROM org_members WHERE org_id = $1`, orgID)
		_, _ = db.ExecContext(base, `DELETE FROM organisations WHERE id = $1`, orgID)
		_, _ = db.ExecContext(base, `DELETE FROM users WHERE id = $1`, userID)
	})

	if _, err := b.DB().ExecContext(base,
		`INSERT INTO users (id, email, password, role) VALUES ($1,$2,'x','user')
		 ON CONFLICT (id) DO NOTHING`, userID, userID+"@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Hold the request's RLS transaction, exactly as the middleware does. With
	// MaxOpenConns=1 this is now the pool's only connection.
	tx, err := b.BeginRLS(base, userID)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	ctx, cancel := context.WithTimeout(WithRLSTx(base, tx), 5*time.Second)
	defer cancel()

	org := &interfaces.Organisation{ID: orgID, Name: "Reuse Org", OwnerID: userID}
	if err := b.SaveOrg(ctx, org); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SaveOrg deadlocked waiting for a second pooled connection while the " +
				"request's RLS transaction held the first — this wedges the whole pool in production")
		}
		t.Fatalf("SaveOrg: %v", err)
	}

	if err := b.MutateOrg(ctx, orgID, func(o *interfaces.Organisation) error {
		o.Name = "Renamed In Tx"
		return nil
	}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("MutateOrg deadlocked waiting for a second pooled connection while the " +
				"request's RLS transaction held the first")
		}
		t.Fatalf("MutateOrg: %v", err)
	}
}

// TestMutateOrg_WriteIsRolledBackWithTheRequest is the atomicity half, and it
// is the sharper assertion: a second connection makes the write durable even
// when the request fails. The middleware promises that an error response or a
// panic leaves nothing persisted.
func TestMutateOrg_WriteIsRolledBackWithTheRequest(t *testing.T) {
	b := backendWithPool(t, 4)
	base := context.Background()

	const userID = "rls-atomic-user"
	const orgID = "rls-atomic-org"
	t.Cleanup(func() {
		db := b.DB()
		_, _ = db.ExecContext(base, `DELETE FROM org_members WHERE org_id = $1`, orgID)
		_, _ = db.ExecContext(base, `DELETE FROM organisations WHERE id = $1`, orgID)
		_, _ = db.ExecContext(base, `DELETE FROM users WHERE id = $1`, userID)
	})
	if _, err := b.DB().ExecContext(base,
		`INSERT INTO users (id, email, password, role) VALUES ($1,$2,'x','user')
		 ON CONFLICT (id) DO NOTHING`, userID, userID+"@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := b.DB().ExecContext(base,
		`INSERT INTO organisations (id, name, owner_id, created_at, updated_at)
		 VALUES ($1,'Original',$2,now(),now()) ON CONFLICT (id) DO UPDATE SET name='Original'`,
		orgID, userID); err != nil {
		t.Fatalf("seed org: %v", err)
	}

	// A request that mutates and then fails: the middleware rolls back.
	tx, err := b.BeginRLS(base, userID)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	ctx := WithRLSTx(base, tx)
	if err := b.MutateOrg(ctx, orgID, func(o *interfaces.Organisation) error {
		o.Name = "RENAMED BY A REQUEST THAT FAILED"
		return nil
	}); err != nil {
		t.Fatalf("MutateOrg: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	var name string
	if err := b.DB().QueryRowContext(base,
		`SELECT name FROM organisations WHERE id = $1`, orgID).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "Original" {
		t.Errorf("org name is %q after the request was rolled back — the mutation committed on a "+
			"second connection and outlived the transaction that was supposed to own it", name)
	}
}
