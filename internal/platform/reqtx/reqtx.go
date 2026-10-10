// Package reqtx is the request transaction as the HTTP layer sees it: a lazy row-level-security scope for the
// authenticated caller, and the runner of the post-commit hooks registered under it (ADR 0004). The Postgres
// backend implements it (storage/database.RequestTx) and di provides it, nil where requests run without row-level
// security (the desktop, auth off), so internal/api never imports the database package. The backend also marks
// each scope it puts on a context, so the layers below can ask whether their reads are filtered (ReadsFiltered).
package reqtx

import (
	"context"

	"pad-analyzer/internal/platform/txhook"
)

// Opener opens the transaction scope of one request.
type Opener interface {
	// Open installs a lazy scope for userID on ctx: the transaction begins with the first scoped statement. With
	// failOpen a scope that cannot begin lets the request run without it (reads); otherwise every scoped statement
	// fails. The caller ends the scope with Commit or Rollback.
	Open(ctx context.Context, userID string, failOpen bool) (context.Context, Scope)
	// HookRunner runs each post-commit hook of the request with a fresh fail-closed scope for userID: the
	// request's own scope has ended by then.
	HookRunner(base context.Context, userID string) txhook.Runner
}

// Scope is one request's transaction.
type Scope interface {
	// Begin starts the transaction now instead of at the first statement. It returns the start failure of a
	// fail-closed scope; nil when the scope started or failed open.
	Begin() error
	// Commit ends the scope, committing what it started; a scope that never started commits nothing.
	Commit() error
	// Rollback ends the scope, rolling back what it started.
	Rollback() error
	// Err is the start failure, if any.
	Err() error
}

type readFilterKey struct{}

// WithReadFilter returns ctx whose storage reads filtered answers for (nil: none). The Postgres backend sets it with
// every scope it puts on a context.
func WithReadFilter(ctx context.Context, filtered func() bool) context.Context {
	return context.WithValue(ctx, readFilterKey{}, filtered)
}

// ReadsFiltered reports whether storage reads under ctx are filtered by row-level security for a caller: ctx
// carries a request's scope (or a post-commit hook's) that has not failed open. Without one they run on the pool,
// where the policies allow every row.
func ReadsFiltered(ctx context.Context) bool {
	filtered, _ := ctx.Value(readFilterKey{}).(func() bool)
	return filtered != nil && filtered()
}
