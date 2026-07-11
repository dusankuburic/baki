package database

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

type contextKey string

const rlsTxKey contextKey = "rls_tx"

const postCommitKey contextKey = "post_commit_hooks"

// hasRLSTx reports whether ctx carries an RLS transaction.
func hasRLSTx(ctx context.Context) bool {
	tx, ok := ctx.Value(rlsTxKey).(*sql.Tx)
	return ok && tx != nil
}

// PostCommitRegistry collects work that must run only after the request's RLS
// transaction commits successfully (e.g. deleting blobs that back a row being
// removed). The RLS middleware creates one per request and runs it post-commit.
type PostCommitRegistry struct {
	mu    sync.Mutex
	hooks []func()
}

// WithPostCommit returns a context carrying a fresh registry plus the registry,
// so the RLS middleware can run the hooks after a successful commit.
func WithPostCommit(ctx context.Context) (context.Context, *PostCommitRegistry) {
	reg := &PostCommitRegistry{}
	return context.WithValue(ctx, postCommitKey, reg), reg
}

// Run executes and clears the registered hooks. Called by the RLS middleware
// only after the transaction commits.
func (r *PostCommitRegistry) Run() {
	r.mu.Lock()
	hooks := r.hooks
	r.hooks = nil
	r.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// hasPostCommit reports whether ctx carries a post-commit registry, i.e.
// whether RegisterPostCommit would defer fn rather than run it inline. Callers
// registering work that is only safe AFTER a commit (e.g. deleting a blob the
// current row version still references) must skip registration when this is
// false and they are inside an uncommitted transaction.
func hasPostCommit(ctx context.Context) bool {
	reg, ok := ctx.Value(postCommitKey).(*PostCommitRegistry)
	return ok && reg != nil
}

// RegisterPostCommit schedules fn to run after the request's RLS transaction
// commits. When there is no registry on the context (no RLS tx — the caller is
// in autocommit, so the surrounding write is already durable) fn runs inline.
func RegisterPostCommit(ctx context.Context, fn func()) {
	if reg, ok := ctx.Value(postCommitKey).(*PostCommitRegistry); ok && reg != nil {
		reg.mu.Lock()
		reg.hooks = append(reg.hooks, fn)
		reg.mu.Unlock()
		return
	}
	fn()
}

// DBTX is the common interface between *sql.DB and *sql.Tx. All tenant-scoped
// queries should call b.query(ctx) to get the right executor — if an RLS
// transaction has been set on the context (by the RLS middleware), the
// transaction is returned; otherwise the pool is used directly (no RLS).
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// query returns the RLS-scoped executor for ctx: a *sql.Tx if the RLS
// middleware set one, otherwise the connection pool. All tenant-scoped storage
// methods should use this instead of b.db directly so that RLS policies can
// enforce row-level visibility.
func (b *PostgresStorageBackend) query(ctx context.Context) DBTX {
	if tx, ok := ctx.Value(rlsTxKey).(*sql.Tx); ok && tx != nil {
		return tx
	}
	return b.db
}

// BeginRLS starts a transaction and sets the app.current_user_id session
// variable so that Postgres RLS policies can filter rows by the calling user.
// The caller must commit or roll back the returned transaction when done.
//
// The transaction is read-write: a request wrapped by the RLS middleware may
// perform writes, and those must run inside the same transaction so the GUC
// (set with SET LOCAL semantics) is in scope.
func (b *PostgresStorageBackend) BeginRLS(ctx context.Context, userID string) (*sql.Tx, error) {
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: false})
	if err != nil {
		return nil, fmt.Errorf("rls: begin tx: %w", err)
	}
	// SET LOCAL cannot be parameterized (it is a utility statement, not a
	// planned query), so set_config(..., is_local => true) is used instead — it
	// accepts a bind parameter and is scoped to this transaction.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_user_id', $1, true)`, userID); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("rls: set user id: %w", err)
	}
	return tx, nil
}

// WithRLSTx returns a new context that carries the RLS transaction. Storage
// methods called with this context will use the transaction instead of the pool.
func WithRLSTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, rlsTxKey, tx)
}
