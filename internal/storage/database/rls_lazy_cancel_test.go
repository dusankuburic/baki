package database

import (
	"context"
	"testing"
	"time"
)

// TestLazyRLS_TransactionSurvivesTheRequestContextsCancellation: a handler
// cut short by the request deadline may still persist the work it completed
// under context.WithoutCancel (a partial fix batch). database/sql rolls a
// transaction back the moment the context it was begun on is done, so the
// scope begins it on an uncancellable context: the request's cancellation
// fails the request's own statements (each runs under its caller's
// context), and the middleware ends the scope when the handler returns.
func TestLazyRLS_TransactionSurvivesTheRequestContextsCancellation(t *testing.T) {
	b, f := newFakeBackend(t, 1, nil)
	base, cancel := context.WithCancel(context.Background())
	ctx, scope := WithLazyRLS(base, b, "u1", false)

	ex := b.query(ctx)
	if !scope.Started() {
		t.Fatal("the first statement did not start the scope")
	}
	cancel()
	// database/sql rolls back a transaction bound to a done context from a
	// goroutine; give it the chance so the assertion below has teeth.
	time.Sleep(50 * time.Millisecond)

	// The request's own statement fails: its context is done.
	if _, err := ex.ExecContext(ctx, `UPDATE t SET x = 1`); err == nil {
		t.Fatal("a statement under the cancelled request context succeeded")
	}
	// The detached persist still runs inside the (live) transaction.
	detached := context.WithoutCancel(ctx)
	if _, err := b.query(detached).ExecContext(detached, `UPDATE t SET x = 2`); err != nil {
		t.Fatalf("exec under the detached context: %v (the transaction was rolled back with the request context)", err)
	}
	if err := scope.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var sawCommit bool
	for _, c := range f.Calls() {
		switch c.query {
		case "ROLLBACK":
			t.Fatalf("the transaction was rolled back by the request context's cancellation:\n%+v", f.Calls())
		case "COMMIT":
			sawCommit = true
		case `UPDATE t SET x = 2`:
			if !c.inTx {
				t.Error("the detached persist ran outside the transaction")
			}
		}
	}
	if !sawCommit {
		t.Fatalf("no COMMIT recorded:\n%+v", f.Calls())
	}
}
