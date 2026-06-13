package database

import (
	"context"
	"database/sql"
	"fmt"
)

type contextKey string

const rlsTxKey contextKey = "rls_tx"

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
