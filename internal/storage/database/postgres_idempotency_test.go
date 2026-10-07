package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/interfaces"
)

// TestMigration32_Shape pins the first .sql step without a database: loaded from the embedded file as an expand
// step with a down file, RLS enabled and forced with the user-scoped policy, the user foreign key cascading (account
// erasure removes the stored responses), and a down step that drops the table.
func TestMigration32_Shape(t *testing.T) {
	m, ok := migrationByVersion(32)
	if !ok {
		t.Fatal("migration v32 not registered")
	}
	if m.name != "idempotency_keys" || m.compat != compatExpand || m.downSQL == "" {
		t.Fatalf("v32 = %q compat %q down %v, want idempotency_keys, expand, reversible", m.name, m.compat, m.downSQL != "")
	}
	up := oneLine(m.sql)
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS idempotency_keys (",
		"CONSTRAINT idempotency_keys_pkey PRIMARY KEY (user_id, idem_key)",
		"FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE",
		"CREATE INDEX IF NOT EXISTS idempotency_keys_expires_idx ON idempotency_keys (expires_at);",
		"ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;",
		"ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;",
		"USING ( NOT app_rls_active() OR user_id = app_current_user_id() ) WITH CHECK ( NOT app_rls_active() OR user_id = app_current_user_id() );",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("v32 lacks %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(m.sql), "CONCURRENTLY") {
		t.Error("v32 must not use CONCURRENTLY: applyMigration runs each step in a transaction")
	}
	if !strings.Contains(oneLine(m.downSQL), "DROP TABLE IF EXISTS idempotency_keys;") {
		t.Error("v32 down does not drop idempotency_keys")
	}
}

// idempotencyTableState reports whether idempotency_keys exists and, if so, whether row-level security is enabled
// and forced on it and its policy is in place.
func idempotencyTableState(t *testing.T, b *PostgresStorageBackend) (exists, rlsForced, policy bool) {
	t.Helper()
	ctx := context.Background()
	var enabled, forced bool
	err := b.db.QueryRowContext(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class
		WHERE relname = 'idempotency_keys' AND relnamespace = current_schema()::regnamespace`).Scan(&enabled, &forced)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, false
	}
	if err != nil {
		t.Fatalf("pg_class: %v", err)
	}
	if err := b.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_policies
		WHERE tablename = 'idempotency_keys' AND policyname = 'rls_idempotency_keys_visible')`).Scan(&policy); err != nil {
		t.Fatalf("pg_policies: %v", err)
	}
	return true, enabled && forced, policy
}

// TestMigration32_UpDownUp is the DATABASE_URL-gated round trip: v32 applied (table, forced RLS, policy, checksum
// recorded as expand), rolled back (table gone), re-applied by the next boot.
func TestMigration32_UpDownUp(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if exists, forced, policy := idempotencyTableState(t, b); !exists || !forced || !policy {
		t.Fatalf("at the latest schema: table %v, RLS forced %v, policy %v; want all", exists, forced, policy)
	}
	m, _ := migrationByVersion(32)
	var sum, compat string
	if err := b.db.QueryRowContext(ctx, `SELECT checksum, compat FROM schema_migrations WHERE version = 32`).Scan(&sum, &compat); err != nil {
		t.Fatalf("read v32 record: %v", err)
	}
	if sum != migrationChecksum(m.sql) || compat != string(compatExpand) {
		t.Errorf("v32 recorded checksum %s compat %q, want %s expand", sum, compat, migrationChecksum(m.sql))
	}

	rolled, err := b.MigrateDown(ctx, 31)
	if err != nil {
		t.Fatalf("MigrateDown(31): %v", err)
	}
	if len(rolled) == 0 || rolled[len(rolled)-1] != 32 {
		t.Fatalf("rolled back %v, want a list ending in 32", rolled)
	}
	if exists, _, _ := idempotencyTableState(t, b); exists {
		t.Fatal("idempotency_keys survived the down step")
	}

	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("reconnect (re-apply v32): %v", err)
	}
	t.Cleanup(func() { b2.Close() })
	if exists, forced, policy := idempotencyTableState(t, b2); !exists || !forced || !policy {
		t.Errorf("after the re-apply: table %v, RLS forced %v, policy %v; want all", exists, forced, policy)
	}
}

// idemUser creates a users row (the records' foreign key needs one), removed on cleanup with its records.
func idemUser(t *testing.T, b *PostgresStorageBackend) string {
	t.Helper()
	id := "idem-user-" + uuid.NewString()
	if err := b.CreateUser(context.Background(), &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash", Role: "member"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _, _ = b.db.ExecContext(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

// idemHarness runs the contract on b, each request transaction a BeginRLS transaction for its user as the RLS
// middleware opens it.
func idemHarness(b *PostgresStorageBackend) contract.IdempotencyHarness {
	return contract.IdempotencyHarness{
		Store: b,
		User:  func(t *testing.T) string { return idemUser(t, b) },
		Begin: func(t *testing.T, userID string) (context.Context, func(bool)) {
			t.Helper()
			tx, err := b.BeginRLS(context.Background(), userID)
			if err != nil {
				t.Fatalf("BeginRLS: %v", err)
			}
			ended := false
			end := func(commit bool) {
				if ended {
					return
				}
				ended = true
				if !commit {
					_ = tx.Rollback()
					return
				}
				if err := tx.Commit(); err != nil {
					t.Errorf("commit: %v", err)
				}
			}
			t.Cleanup(func() { end(false) })
			return WithRLSTx(context.Background(), tx), end
		},
		Expire: func(t *testing.T, userID, key string) {
			t.Helper()
			if _, err := b.db.ExecContext(context.Background(),
				`UPDATE idempotency_keys SET expires_at = NOW() - INTERVAL '1 second' WHERE user_id = $1 AND idem_key = $2`, userID, key); err != nil {
				t.Fatalf("expire: %v", err)
			}
		},
		Purge: func(t *testing.T) int {
			t.Helper()
			res, err := b.PurgeExpiredData(context.Background(), 0)
			if err != nil {
				t.Fatalf("PurgeExpiredData: %v", err)
			}
			return res.IdempotencyKeys
		},
	}
}

// TestPostgresIdempotency_Contract runs the shared contract on Postgres, with stored bodies in plaintext and sealed.
func TestPostgresIdempotency_Contract(t *testing.T) {
	t.Run("plaintext", func(t *testing.T) { contract.RunIdempotencySuite(t, idemHarness(openMigrationTestDB(t))) })
	t.Run("sealed", func(t *testing.T) {
		b := openMigrationTestDB(t)
		if err := b.EnableFieldEncryption([]byte(testFieldKey)); err != nil {
			t.Fatal(err)
		}
		contract.RunIdempotencySuite(t, idemHarness(b))
	})
}

// idemCompleteFor claims key for userID and stores resp in one committed request transaction.
func idemCompleteFor(t *testing.T, b *PostgresStorageBackend, userID, key string, resp interfaces.IdempotentResponse) {
	t.Helper()
	ctx := context.Background()
	tx, err := b.BeginRLS(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rctx := WithRLSTx(ctx, tx)
	if got, err := b.ClaimIdempotencyKey(rctx, userID, key, "POST /api/auth/tokens x"); err != nil || got != nil {
		t.Fatalf("claim = (%+v, %v)", got, err)
	}
	if err := b.CompleteIdempotencyKey(rctx, userID, key, resp); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestIdempotency_BodySealedAtRest: with field encryption on, a stored response (here a new API token, shown once)
// is ciphertext in the table and plaintext in the replay; the ciphertext is bound to its user and key.
func TestIdempotency_BodySealedAtRest(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	if err := b.EnableFieldEncryption([]byte(testFieldKey)); err != nil {
		t.Fatal(err)
	}
	u := idemUser(t, b)
	const token = `{"token":"pad_SECRET-shown-once"}`
	idemCompleteFor(t, b, u, "k1", interfaces.IdempotentResponse{Status: 201, ContentType: "application/json", Body: []byte(token)})
	idemCompleteFor(t, b, u, "k2", interfaces.IdempotentResponse{Status: 201, ContentType: "application/json", Body: []byte(`{}`)})

	var raw []byte
	if err := b.db.QueryRowContext(ctx, `SELECT body FROM idempotency_keys WHERE user_id = $1 AND idem_key = 'k1'`, u).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), sealedPrefix) || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("stored body is not sealed: %q", raw)
	}

	replay := func(key string) (*interfaces.IdempotentResponse, error) {
		tx, err := b.BeginRLS(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		return b.ClaimIdempotencyKey(WithRLSTx(ctx, tx), u, key, "POST /api/auth/tokens x")
	}
	got, err := replay("k1")
	if err != nil || got == nil || string(got.Body) != token {
		t.Fatalf("replay = (%+v, %v), want the plaintext token", got, err)
	}

	// The ciphertext copied onto another key does not decrypt there.
	if _, err := b.db.ExecContext(ctx, `UPDATE idempotency_keys SET body = $2 WHERE user_id = $1 AND idem_key = 'k2'`, u, raw); err != nil {
		t.Fatal(err)
	}
	if got, err := replay("k2"); err == nil {
		t.Errorf("a ciphertext moved to another key decrypted: %q", got.Body)
	}
}

// TestIdempotency_RowLevelSecurity: inside one user's request transaction another user's records are invisible
// and cannot be written, whatever the query says (defense in depth under the (user, key) scoping).
func TestIdempotency_RowLevelSecurity(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	var bypass bool
	if err := b.db.QueryRowContext(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatal(err)
	}
	if bypass {
		t.Skip("the test role bypasses row-level security")
	}
	alice, bob := idemUser(t, b), idemUser(t, b)
	idemCompleteFor(t, b, alice, "shared-key", interfaces.IdempotentResponse{Status: 201, Body: []byte(`{"alice":true}`)})

	tx, err := b.BeginRLS(ctx, bob)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var visible int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1`, alice).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Errorf("bob's transaction sees %d of alice's records", visible)
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT s`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (user_id, idem_key, fingerprint, expires_at) VALUES ($1, 'forged', 'x', NOW() + INTERVAL '1 hour')`, alice); err == nil {
		t.Error("bob's transaction wrote a record for alice")
	}
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT s`); err != nil {
		t.Fatal(err)
	}
	if got, err := b.ClaimIdempotencyKey(WithRLSTx(ctx, tx), bob, "shared-key", "POST /api/auth/tokens x"); err != nil || got != nil {
		t.Errorf("bob's claim of a key alice used = (%+v, %v), want claimed: keys are per user", got, err)
	}
}

// TestIdempotency_DeleteUserRemovesRecords: account erasure removes the user's stored responses (the foreign key
// cascades), since they can hold personal data.
func TestIdempotency_DeleteUserRemovesRecords(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	u := idemUser(t, b)
	idemCompleteFor(t, b, u, "erased", interfaces.IdempotentResponse{Status: 201, Body: []byte(`{"email":"someone@example.com"}`)})
	if err := b.DeleteUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1`, u).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d records survived the account's erasure", n)
	}
}

// TestPurgeExpiredData_IdempotencyKeys: the retention sweep removes idempotency records past their expiry in
// batches, independent of the audit retention window (a 24-hour retention of its own).
func TestPurgeExpiredData_IdempotencyKeys(t *testing.T) {
	old := purgeBatchSize
	purgeBatchSize = 4
	t.Cleanup(func() { purgeBatchSize = old })
	batches := []int64{4, 1, 0}
	calls := 0
	b, f := newR3ExecBackend(t, func(query string, _ []driver.NamedValue) (int64, error) {
		if strings.HasPrefix(query, "DELETE FROM idempotency_keys ") {
			n := batches[calls]
			calls++
			return n, nil
		}
		return 0, nil
	})
	for _, window := range []int{0, 30} {
		calls = 0
		res, err := b.PurgeExpiredData(context.Background(), window)
		if err != nil {
			t.Fatal(err)
		}
		if res.IdempotencyKeys != 5 {
			t.Errorf("window %d: IdempotencyKeys = %d, want 5", window, res.IdempotencyKeys)
		}
	}
	deletes := deletesOf(f, "idempotency_keys")
	if len(deletes) != 6 {
		t.Fatalf("idempotency_keys deletes = %d, want 3 per sweep:\n%s", len(deletes), describeCalls(deletes))
	}
	for _, c := range deletes {
		if q := oneLine(c.query); !strings.Contains(q, "ctid = ANY(ARRAY(SELECT ctid FROM idempotency_keys WHERE expires_at < NOW() LIMIT $1))") || len(c.args) != 1 {
			t.Errorf("not the batched expiry form: %s %v", q, c.args)
		}
	}
}

// TestIdempotency_ClaimNeedsTheRequestTransaction: outside a request transaction the store neither writes nor
// opens a transaction of its own (that claim would commit apart from the request's writes).
func TestIdempotency_ClaimNeedsTheRequestTransaction(t *testing.T) {
	b, f := newR3ExecBackend(t, func(string, []driver.NamedValue) (int64, error) { return 1, nil })
	ctx := context.Background()
	if _, err := b.ClaimIdempotencyKey(ctx, "u", "k", "fp"); !errors.Is(err, interfaces.ErrIdempotencyUnavailable) {
		t.Errorf("claim = %v, want ErrIdempotencyUnavailable", err)
	}
	if err := b.CompleteIdempotencyKey(ctx, "u", "k", interfaces.IdempotentResponse{Status: 201}); !errors.Is(err, interfaces.ErrIdempotencyUnavailable) {
		t.Errorf("complete = %v, want ErrIdempotencyUnavailable", err)
	}
	if calls := f.Queries(); len(calls) != 0 {
		t.Errorf("statements sent outside the request transaction: %s", describeCalls(calls))
	}
}

// TestIdempotencyLockKey_SeparatesUsersAndKeys: the advisory-lock key covers both parts, with a separator neither
// can contain (a key is printable ASCII, a user id a TEXT value).
func TestIdempotencyLockKey_SeparatesUsersAndKeys(t *testing.T) {
	seen := map[int64]string{}
	for _, pair := range [][2]string{{"u1", "k"}, {"u2", "k"}, {"u1", "k2"}, {"u1k", "x"}, {"u", "1k"}} {
		k := idempotencyLockKey(pair[0], pair[1])
		name := fmt.Sprintf("%q", pair)
		if prev, dup := seen[k]; dup {
			t.Errorf("%s and %s share an advisory-lock key", prev, name)
		}
		seen[k] = name
	}
}
