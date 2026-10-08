package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"pad-analyzer/internal/platform/txhook"
)

// setsUser reports whether c is BeginRLS's set_config for userID.
func setsUser(c fakeCall, userID string) bool {
	return strings.Contains(c.query, "set_config('app.current_user_id'") && len(c.args) == 1 && c.args[0].Value == userID
}

// TestWithTxAs_OwnTransactionRunsAsTheUser: without a request transaction, withTxAs opens one as the user (the
// set_config right after BEGIN), commits it, and runs its hooks in a fresh scope for the same user; a failing body
// rolls back and drops them.
func TestWithTxAs_OwnTransactionRunsAsTheUser(t *testing.T) {
	b, f := newFakeBackend(t, 1, nil)
	ctx := context.Background()

	hookRan := false
	err := b.withTxAs(ctx, "u-1", func(txCtx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(txCtx, `DELETE FROM knowledge_documents WHERE id = $1`, "d-1"); err != nil {
			return err
		}
		txhook.Register(txCtx, func(hookCtx context.Context) {
			hookRan = true
			if _, err := b.query(hookCtx).ExecContext(hookCtx, `SELECT 1`); err != nil {
				t.Errorf("hook statement: %v", err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatalf("withTxAs: %v", err)
	}
	calls := f.Calls()
	var got []string
	for _, c := range calls {
		got = append(got, oneLine(c.query))
	}
	want := []string{
		"BEGIN", "SELECT set_config('app.current_user_id', $1, true)", "DELETE FROM knowledge_documents WHERE id = $1", "COMMIT",
		"BEGIN", "SELECT set_config('app.current_user_id', $1, true)", "SELECT 1", "COMMIT",
	}
	if !hookRan || !slices.Equal(got, want) {
		t.Fatalf("hook ran=%v, statements = %q, want %q", hookRan, got, want)
	}
	if !setsUser(calls[1], "u-1") || !setsUser(calls[5], "u-1") {
		t.Errorf("transaction and hook scope must both run as u-1: %+v, %+v", calls[1].args, calls[5].args)
	}

	ran := false
	err = b.withTxAs(ctx, "u-1", func(txCtx context.Context, _ *sql.Tx) error {
		txhook.Register(txCtx, func(context.Context) { ran = true })
		return errors.New("step failed")
	})
	if err == nil || ran {
		t.Fatalf("err=%v hook ran=%v: a failed body must roll back and drop its hooks", err, ran)
	}
	if last := f.Calls()[len(f.Calls())-1]; last.query != "ROLLBACK" {
		t.Errorf("last statement = %q, want ROLLBACK", last.query)
	}
}

// TestWithTxAs_JoinsTheRequestTransaction: inside a request's transaction withTxAs opens nothing (no BEGIN, no
// set_config: the request's scope already runs as its user) and hands fn the caller's context.
func TestWithTxAs_JoinsTheRequestTransaction(t *testing.T) {
	b, f := newFakeBackend(t, 1, nil)
	tx, err := b.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	reqCtx := WithRLSTx(context.Background(), tx)
	before := len(f.Calls())

	if err := b.withTxAs(reqCtx, "u-1", func(txCtx context.Context, got *sql.Tx) error {
		if txCtx != reqCtx || got != tx {
			t.Error("joining withTxAs must run fn on the request's context and transaction")
		}
		_, err := got.ExecContext(txCtx, `SELECT 1`)
		return err
	}); err != nil {
		t.Fatalf("withTxAs: %v", err)
	}
	if calls := f.Calls()[before:]; len(calls) != 1 || calls[0].query != "SELECT 1" || !calls[0].inTx {
		t.Fatalf("statements = %+v, want only the body's, inside the request transaction", calls)
	}
}

// TestWithTxAs_FailClosedScopeRefuses: a request scope that could not start answers ErrRLSUnavailable instead of
// letting the write fall back to a transaction of its own.
func TestWithTxAs_FailClosedScopeRefuses(t *testing.T) {
	b, f := newFakeBackend(t, 1, nil)
	ctx, scope := WithLazyRLS(context.Background(), nil, "u-1", false)
	defer func() { _ = scope.Rollback() }()
	err := b.withTxAs(ctx, "u-1", func(context.Context, *sql.Tx) error {
		t.Error("fn ran without the request's transaction")
		return nil
	})
	if !errors.Is(err, ErrRLSUnavailable) {
		t.Fatalf("err = %v, want ErrRLSUnavailable", err)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Fatalf("statements = %+v, want none", calls)
	}
}

// TestWithOwnTx_NeverJoinsTheRequestTransaction: a write that must outlive the request's rollback runs on a
// connection and transaction of its own, unscoped, even inside a request.
func TestWithOwnTx_NeverJoinsTheRequestTransaction(t *testing.T) {
	b, f := newFakeBackend(t, 2, nil)
	reqTx, err := b.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reqTx.Rollback() }()
	reqCtx := WithRLSTx(context.Background(), reqTx)
	reqConn := f.Calls()[0].conn

	if err := b.withOwnTx(reqCtx, sql.LevelDefault, func(txCtx context.Context, tx *sql.Tx) error {
		if tx == reqTx {
			t.Error("withOwnTx joined the request's transaction")
		}
		if _, err := b.query(txCtx).ExecContext(txCtx, `INSERT INTO audit_events VALUES ($1)`, "e-1"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("withOwnTx: %v", err)
	}
	var own []string
	for _, c := range f.Calls()[1:] {
		if c.conn == reqConn {
			t.Errorf("%q ran on the request's connection", c.query)
		}
		own = append(own, c.query)
	}
	if want := []string{"BEGIN", "INSERT INTO audit_events VALUES ($1)", "COMMIT"}; !slices.Equal(own, want) {
		t.Fatalf("statements = %q, want %q (no set_config: the own transaction is unscoped)", own, want)
	}
}

// TestWithOwnTx_SerializableRetriesASerializationFailure: at SERIALIZABLE a 40001 reruns fn in a new transaction
// (each one SERIALIZABLE), up to serializableAttempts in all; any other error returns at once.
func TestWithOwnTx_SerializableRetriesASerializationFailure(t *testing.T) {
	failures := 2
	b, f := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
		if query == "INSERT" && failures > 0 {
			failures--
			return fakeResult{err: &pgconn.PgError{Code: pgErrSerializationFailure}}
		}
		return fakeResult{}
	})
	ctx := context.Background()
	runs := 0
	body := func(_ context.Context, tx *sql.Tx) error {
		runs++
		_, err := tx.ExecContext(ctx, "INSERT")
		return err
	}
	if err := b.withOwnTx(ctx, sql.LevelSerializable, body); err != nil {
		t.Fatalf("withOwnTx: %v", err)
	}
	if runs != 3 {
		t.Errorf("fn ran %d times, want 3 (two serialization failures, then success)", runs)
	}
	for i, iso := range f.isolations {
		if iso != driver.IsolationLevel(sql.LevelSerializable) {
			t.Errorf("BEGIN %d isolation = %v, want serializable", i, iso)
		}
	}
	if n := len(f.isolations); n != 3 {
		t.Errorf("%d transactions, want 3", n)
	}

	runs, failures = 0, serializableAttempts+1
	err := b.withOwnTx(ctx, sql.LevelSerializable, body)
	if runs != serializableAttempts || !isPgErrCode(err, pgErrSerializationFailure) {
		t.Errorf("runs = %d, err = %v: want %d runs and the serialization failure", runs, err, serializableAttempts)
	}

	runs = 0
	err = b.withOwnTx(ctx, sql.LevelSerializable, func(context.Context, *sql.Tx) error {
		runs++
		return errors.New("unique violation")
	})
	if runs != 1 || err == nil {
		t.Errorf("runs = %d, err = %v: any other error is not retried", runs, err)
	}

	runs, failures = 0, 1
	if err := b.withOwnTx(ctx, sql.LevelDefault, body); !isPgErrCode(err, pgErrSerializationFailure) || runs != 1 {
		t.Errorf("runs = %d, err = %v: below SERIALIZABLE nothing is retried", runs, err)
	}
}
