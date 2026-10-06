package database

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"pad-analyzer/internal/platform/txhook"
)

// TestWithTx_OwnTransactionRunsHooksAfterCommitOnly: when withTx opens its own transaction, the hooks registered
// inside run after COMMIT, and not at all when the body fails (the transaction rolled back).
func TestWithTx_OwnTransactionRunsHooksAfterCommitOnly(t *testing.T) {
	b, f := newFakeBackend(t, 1, nil)
	ctx := context.Background()

	var callsAtHook []string
	err := b.withTx(ctx, func(txCtx context.Context, _ *sql.Tx) error {
		if !txhook.Has(txCtx) {
			t.Error("withTx's own transaction carries no post-commit registry")
		}
		txhook.Register(txCtx, func(context.Context) {
			for _, c := range f.Calls() {
				callsAtHook = append(callsAtHook, c.query)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatalf("withTx: %v", err)
	}
	if !slices.Equal(callsAtHook, []string{"BEGIN", "COMMIT"}) {
		t.Fatalf("statements seen by the hook = %v, want the COMMIT already sent", callsAtHook)
	}

	ran := false
	err = b.withTx(ctx, func(txCtx context.Context, _ *sql.Tx) error {
		txhook.Register(txCtx, func(context.Context) { ran = true })
		return errors.New("step failed")
	})
	if err == nil || ran {
		t.Fatalf("err=%v hook ran=%v: a failed body must roll back and drop its hooks", err, ran)
	}
}

// TestWithTx_JoinedTransactionLeavesHooksToItsOwner: joining the request's transaction, withTx passes ctx through
// unchanged, so hooks wait for the request's registry; a registry of withTx's own would fire before the
// request commits.
func TestWithTx_JoinedTransactionLeavesHooksToItsOwner(t *testing.T) {
	b, _ := newFakeBackend(t, 1, nil)
	tx, err := b.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	reqCtx, reqHooks := txhook.With(WithRLSTx(context.Background(), tx))

	ran := false
	if err := b.withTx(reqCtx, func(txCtx context.Context, _ *sql.Tx) error {
		if txCtx != reqCtx {
			t.Error("joining withTx must hand fn the caller's context unchanged")
		}
		txhook.Register(txCtx, func(context.Context) { ran = true })
		return nil
	}); err != nil {
		t.Fatalf("withTx: %v", err)
	}
	if ran {
		t.Fatal("hook ran before the request's transaction committed")
	}
	reqHooks.Commit(nil)
	if !ran {
		t.Fatal("hook did not run on the request's commit")
	}
}
