package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// The RLS scope the middleware installs is lazy: BEGIN + set_config run on
// the first scoped statement, not at install time, so a request that never
// touches the store neither pins a pooled connection nor pays the
// round-trips. The fake-driver tests pin the statements sent; the Postgres
// test pins that the lazily started transaction carries the user id and
// that the policies filter through it.

// TestLazyRLS_NoStatementOpensNoTransaction: install, never query, end.
func TestLazyRLS_NoStatementOpensNoTransaction(t *testing.T) {
	b, f := newFakeBackend(t, 1, nil)
	ctx, scope := WithLazyRLS(context.Background(), b, "u1", false)

	if !hasRLSTx(ctx) {
		t.Fatal("hasRLSTx must report the scope so writers join it instead of opening their own transaction")
	}
	if scope.Started() {
		t.Fatal("the scope started before any statement")
	}
	if err := scope.Commit(); err != nil {
		t.Fatalf("Commit of an unstarted scope: %v", err)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Fatalf("a request without statements sent %d statement(s) to the driver: %+v", len(calls), calls)
	}

	// A statement after the request ended (a goroutine that outlived it)
	// must not open a transaction or fall through to the pool.
	if _, _, err := rlsTxFromContext(ctx); !errors.Is(err, ErrRLSUnavailable) {
		t.Errorf("statement after the scope ended: err %v, want ErrRLSUnavailable", err)
	}
	if err := b.query(ctx).QueryRowContext(ctx, `SELECT 1`).Scan(new(int)); !errors.Is(err, ErrRLSUnavailable) {
		t.Errorf("query after the scope ended: %v, want ErrRLSUnavailable", err)
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Errorf("the late statement reached the driver: %+v", calls)
	}
}

// TestLazyRLS_StatementAfterEndOfAStartedScope: a scope that was used and
// then committed (or rolled back) must refuse later statements — a goroutine
// that outlived its request — with ErrRLSUnavailable: neither the committed
// transaction (a nil *sql.Tx, which panics in grabConn) nor the pool, for
// fail-open scopes too.
func TestLazyRLS_StatementAfterEndOfAStartedScope(t *testing.T) {
	for _, failOpen := range []bool{false, true} {
		t.Run(map[bool]string{false: "fail closed", true: "fail open"}[failOpen], func(t *testing.T) {
			b, f := newFakeBackend(t, 1, nil)
			ctx, scope := WithLazyRLS(context.Background(), b, "u1", failOpen)
			if _, err := b.query(ctx).ExecContext(ctx, `SELECT 1`); err != nil {
				t.Fatalf("statement inside the request: %v", err)
			}
			if err := scope.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			before := len(f.Calls())

			if _, err := b.query(ctx).ExecContext(ctx, `SELECT 2`); !errors.Is(err, ErrRLSUnavailable) {
				t.Errorf("Exec after Commit: %v, want ErrRLSUnavailable", err)
			}
			if err := b.query(ctx).QueryRowContext(ctx, `SELECT 2`).Scan(new(int)); !errors.Is(err, ErrRLSUnavailable) {
				t.Errorf("QueryRow after Commit: %v, want ErrRLSUnavailable", err)
			}
			err := b.withTx(ctx, func(context.Context, *sql.Tx) error { t.Error("withTx ran its body after the scope ended"); return nil })
			if !errors.Is(err, ErrRLSUnavailable) {
				t.Errorf("withTx after Commit: %v, want ErrRLSUnavailable", err)
			}
			if !hasRLSTx(ctx) {
				t.Error("hasRLSTx = false after Commit: a writer would open its own pool transaction for a request that is over")
			}
			if n := len(f.Calls()) - before; n != 0 {
				t.Errorf("%d statement(s) reached the driver after the scope ended: %+v", n, f.Calls()[before:])
			}
		})
	}

	// A fail-open scope whose start failed ran its request on the pool and
	// did not count as a scope; once ended it counts again, so a late writer
	// errors instead of opening a pool transaction of its own.
	t.Run("fail open after a failed start", func(t *testing.T) {
		b, f := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
			if strings.HasPrefix(query, "SELECT set_config(") {
				return fakeResult{err: errors.New("pool exhausted")}
			}
			return fakeResult{}
		})
		ctx, scope := WithLazyRLS(context.Background(), b, "u1", true)
		if _, err := b.query(ctx).ExecContext(ctx, `SELECT 1`); err != nil {
			t.Fatalf("fail-open statement inside the request: %v", err)
		}
		if hasRLSTx(ctx) {
			t.Error("hasRLSTx = true for a fail-open scope that failed to start")
		}
		if err := scope.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		before := len(f.Calls())
		if !hasRLSTx(ctx) {
			t.Error("hasRLSTx = false after Commit: a writer would open its own pool transaction for a request that is over")
		}
		if _, err := b.query(ctx).ExecContext(ctx, `SELECT 2`); !errors.Is(err, ErrRLSUnavailable) {
			t.Errorf("Exec after Commit: %v, want ErrRLSUnavailable", err)
		}
		if n := len(f.Calls()) - before; n != 0 {
			t.Errorf("%d statement(s) reached the driver after the scope ended: %+v", n, f.Calls()[before:])
		}
	})
}

// TestLazyRLS_FirstStatementStartsExactlyOneTransaction: the first scoped
// statement begins and sets the user; every later one joins that
// transaction; the middleware's Commit commits it.
func TestLazyRLS_FirstStatementStartsExactlyOneTransaction(t *testing.T) {
	b, f := newFakeBackend(t, 1, nil)
	ctx, scope := WithLazyRLS(context.Background(), b, "u1", false)

	first := b.query(ctx)
	if !scope.Started() {
		t.Fatal("the first statement did not start the scope")
	}
	second := b.query(ctx)
	if first != second {
		t.Fatal("the second statement got a different executor than the first")
	}
	if _, err := second.ExecContext(ctx, `UPDATE t SET x = 1`); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := scope.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	calls := f.Calls()
	want := []string{"BEGIN", `SELECT set_config('app.current_user_id', $1, true)`, `UPDATE t SET x = 1`, "COMMIT"}
	if len(calls) != len(want) {
		t.Fatalf("statements: got %d, want %d:\n%+v", len(calls), len(want), calls)
	}
	for i, c := range calls {
		if c.query != want[i] {
			t.Errorf("statement %d = %q, want %q", i, c.query, want[i])
		}
		if c.conn != 1 {
			t.Errorf("statement %d ran on connection %d, want the transaction's (1)", i, c.conn)
		}
	}
	if args := calls[1].args; len(args) != 1 || args[0].Value != "u1" {
		t.Errorf("set_config args = %+v, want the scope's user", args)
	}
	if !calls[2].inTx {
		t.Error("the update ran outside the transaction")
	}
}

// TestLazyRLS_StartsOnTheRequestContextNotTheCallers: the statement that
// happens to start the transaction may run under a derived, shorter-lived
// context (a sub-call's timeout); its cancellation must not roll back the
// request-long transaction.
func TestLazyRLS_StartsOnTheRequestContextNotTheCallers(t *testing.T) {
	b, _ := newFakeBackend(t, 1, nil)
	ctx, scope := WithLazyRLS(context.Background(), b, "u1", false)
	defer func() { _ = scope.Rollback() }()

	sub, cancel := context.WithTimeout(ctx, time.Hour)
	if _, err := b.query(sub).ExecContext(sub, `SELECT 1`); err != nil {
		t.Fatalf("first statement: %v", err)
	}
	cancel()

	if _, err := b.query(ctx).ExecContext(ctx, `SELECT 2`); err != nil {
		t.Fatalf("after the starting call's context was cancelled, the request transaction is gone: %v", err)
	}
}

// TestLazyRLS_StartFailure: fail-closed scopes refuse every statement
// (ErrRLSUnavailable, never the pool); fail-open scopes run on the pool and
// stop counting as an RLS scope for writers. The fake driver's BEGIN cannot
// fail, so the start fails on set_config (BeginRLS rolls back and errors).
func TestLazyRLS_StartFailure(t *testing.T) {
	boom := errors.New("pool exhausted")
	respond := func(query string, _ []driver.NamedValue) fakeResult {
		if strings.HasPrefix(query, "SELECT set_config(") {
			return fakeResult{err: boom}
		}
		return fakeResult{}
	}
	// reads lists the test's own statements the driver saw (not the start's).
	reads := func(f *fakeSQL) []fakeCall {
		var out []fakeCall
		for _, c := range f.Queries() {
			if c.query == "SELECT 1" {
				out = append(out, c)
			}
		}
		return out
	}

	t.Run("fail closed", func(t *testing.T) {
		b, f := newFakeBackend(t, 1, respond)
		ctx, scope := WithLazyRLS(context.Background(), b, "u1", false)
		err := b.withTx(ctx, func(context.Context, *sql.Tx) error { t.Error("withTx ran its body without a transaction"); return nil })
		if !errors.Is(err, ErrRLSUnavailable) || !errors.Is(err, boom) {
			t.Errorf("withTx: %v, want ErrRLSUnavailable wrapping the cause", err)
		}
		if err := b.query(ctx).QueryRowContext(ctx, `SELECT 1`).Scan(new(int)); !errors.Is(err, ErrRLSUnavailable) {
			t.Errorf("QueryRow: %v, want ErrRLSUnavailable", err)
		}
		if _, err := b.query(ctx).ExecContext(ctx, `SELECT 1`); !errors.Is(err, ErrRLSUnavailable) {
			t.Errorf("Exec: %v, want ErrRLSUnavailable", err)
		}
		rows, err := b.query(ctx).QueryContext(ctx, `SELECT 1`)
		if err == nil {
			defer rows.Close()
			_ = rows.Err()
		}
		if !errors.Is(err, ErrRLSUnavailable) {
			t.Errorf("Query: %v, want ErrRLSUnavailable", err)
		}
		if n := len(reads(f)); n != 0 {
			t.Errorf("%d statement(s) reached the pool after a fail-closed start failure: %+v", n, reads(f))
		}
		if begins := countQuery(f, "BEGIN"); begins != 1 {
			t.Errorf("BEGIN x%d, want one attempt (the failure is sticky)", begins)
		}
		if !hasRLSTx(ctx) {
			t.Error("a fail-closed scope must still count as one (writers must not open their own transaction)")
		}
		if !errors.Is(scope.Err(), boom) {
			t.Errorf("scope.Err() = %v", scope.Err())
		}
		if scope.Started() || scope.Commit() != nil {
			t.Error("a failed scope has nothing to commit")
		}
	})

	t.Run("fail open", func(t *testing.T) {
		b, f := newFakeBackend(t, 1, respond)
		ctx, scope := WithLazyRLS(context.Background(), b, "u1", true)
		if _, err := b.query(ctx).ExecContext(ctx, `SELECT 1`); err != nil {
			t.Fatalf("a fail-open read must run on the pool: %v", err)
		}
		if qs := reads(f); len(qs) != 1 || qs[0].inTx {
			t.Errorf("statements %+v, want the one read on the pool", qs)
		}
		if hasRLSTx(ctx) {
			t.Error("a fail-open scope that failed is no RLS scope: writers open their own transaction")
		}
		if err := scope.Commit(); err != nil {
			t.Errorf("Commit: %v", err)
		}
	})
}

func countQuery(f *fakeSQL, q string) int {
	n := 0
	for _, c := range f.Calls() {
		if c.query == q {
			n++
		}
	}
	return n
}

// TestLazyRLS_WithRLSTxIsEager: a supplied transaction (tests, and the
// storage methods that open their own) is used as is, and a nil one clears
// the scope.
func TestLazyRLS_WithRLSTxIsEager(t *testing.T) {
	b, f := newFakeBackend(t, 1, nil)
	tx, err := b.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	ctx := WithRLSTx(context.Background(), tx)
	got, ok, err := rlsTxFromContext(ctx)
	if err != nil || !ok || got != tx {
		t.Errorf("rlsTxFromContext = (%v, %v, %v), want the supplied transaction", got, ok, err)
	}
	if !hasRLSTx(ctx) {
		t.Error("hasRLSTx = false for a supplied transaction")
	}
	cleared := WithRLSTx(ctx, nil)
	if hasRLSTx(cleared) {
		t.Error("WithRLSTx(ctx, nil) left the scope in place")
	}
	if b.query(cleared) != DBTX(b.db) {
		t.Error("a cleared scope must route to the pool")
	}
	if n := countQuery(f, "BEGIN"); n != 1 {
		t.Errorf("BEGIN x%d, want only the test's own", n)
	}
}

// TestLazyRLS_ConcurrentFirstUse: goroutines sharing the request context
// race to start the scope; exactly one transaction is opened.
func TestLazyRLS_ConcurrentFirstUse(t *testing.T) {
	b, f := newFakeBackend(t, 4, nil)
	ctx, scope := WithLazyRLS(context.Background(), b, "u1", false)
	defer func() { _ = scope.Rollback() }()

	const n = 8
	txs := make(chan DBTX, n)
	for i := 0; i < n; i++ {
		go func() { txs <- b.query(ctx) }()
	}
	first := <-txs
	for i := 1; i < n; i++ {
		if got := <-txs; got != first {
			t.Error("concurrent first use handed out different executors")
		}
	}
	if begins := countQuery(f, "BEGIN"); begins != 1 {
		t.Errorf("BEGIN x%d under concurrent first use, want 1", begins)
	}
}

// TestLazyRLS_Postgres: against a real server the lazily started
// transaction carries app.current_user_id and the flow policies filter
// through it — and until the first statement the request holds no
// connection at all (pool of 1: a pool read runs while the scope is idle).
func TestLazyRLS_Postgres(t *testing.T) {
	b := backendWithPool(t, 1)
	ctx := context.Background()

	stamp := time.Now().Format("150405.000000000")
	userA, userB := "lazy-rls-a-"+stamp, "lazy-rls-b-"+stamp
	flowA, flowB := "lazy-rls-flow-a-"+stamp, "lazy-rls-flow-b-"+stamp
	for _, id := range []string{userA, userB} {
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: id, Email: id + "@test.com", Password: "$2a$12$testhash", Role: "member",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range []string{flowA, flowB} {
			_ = b.DeleteFlow(ctx, id)
		}
		for _, id := range []string{userA, userB} {
			_, _ = b.db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, id)
		}
	})
	for id, owner := range map[string]string{flowA: userA, flowB: userB} {
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: id, OwnerID: owner, Content: []byte("{}")}); err != nil {
			t.Fatalf("SaveFlow %s: %v", id, err)
		}
	}

	rctx, scope := WithLazyRLS(ctx, b, userA, false)
	defer func() { _ = scope.Rollback() }()

	// Idle scope: the only pooled connection is free.
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var one int
	if err := b.db.QueryRowContext(probe, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("pool read while the scope is idle: %v (the scope pinned the only connection before any statement)", err)
	}

	var current string
	if err := b.query(rctx).QueryRowContext(rctx, `SELECT current_setting('app.current_user_id', true)`).Scan(&current); err != nil {
		t.Fatalf("current_setting: %v", err)
	}
	if current != userA {
		t.Errorf("app.current_user_id = %q, want %q", current, userA)
	}
	if !scope.Started() {
		t.Error("the first statement did not start the scope")
	}

	var inTx bool
	if err := b.query(rctx).QueryRowContext(rctx, `SELECT current_setting('app.current_user_id', true) = $1`, userA).Scan(&inTx); err != nil || !inTx {
		t.Errorf("second statement outside the started transaction (err %v)", err)
	}

	flows, err := b.ListFlows(rctx, interfaces.FlowFilter{UserID: userA, Limit: 100})
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	seenA := false
	for _, f := range flows {
		if f.OwnerID != userA {
			t.Errorf("RLS leak through the lazy scope: %s sees flow %s owned by %s", userA, f.ID, f.OwnerID)
		}
		seenA = seenA || f.ID == flowA
	}
	if !seenA {
		t.Error("RLS over-filtered: user A cannot see own flow through the lazy scope")
	}
	if _, err := b.LoadFlow(rctx, flowB); err == nil {
		skipIfRLSBypassedInternal(t, b)
		t.Errorf("RLS leak through the lazy scope: %s loaded %s's flow", userA, userB)
	}
	if err := scope.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if scope.Started() {
		t.Error("Started() after Commit")
	}
}

// skipIfRLSBypassedInternal mirrors rls_test.go's guard for this package's
// internal tests: a superuser / BYPASSRLS role sees every row regardless.
func skipIfRLSBypassedInternal(t *testing.T, b *PostgresStorageBackend) {
	t.Helper()
	var bypass bool
	if err := b.db.QueryRowContext(context.Background(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		t.Fatalf("check rls bypass: %v", err)
	}
	if bypass {
		t.Skip("connected as a superuser/BYPASSRLS role; RLS isolation cannot be validated")
	}
}
