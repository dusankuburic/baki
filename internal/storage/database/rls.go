package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"sync"

	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/storage/interfaces"
)

type contextKey string

const rlsTxKey contextKey = "rls_tx"

// ErrRLSUnavailable is what every statement sent through a fail-closed RLS scope returns once the scoped
// transaction could not be started: the request must not fall through to the pool, where the policies
// short-circuit to allow-all (logged by the RLS middleware; render.Error maps it to 503).
var ErrRLSUnavailable = interfaces.ErrRLSUnavailable

// errRLSScopeEnded is the start error of a scope used after the middleware
// committed or rolled it back — a goroutine that outlived its request.
var errRLSScopeEnded = fmt.Errorf("%w: request scope already ended", ErrRLSUnavailable)

// RLSScope is the per-request holder of the RLS transaction. It starts the transaction LAZILY: BEGIN +
// set_config run only when a storage method first asks for the executor, so a handler that never touches
// the database neither pins a pooled connection nor pays a round-trip. Once started, every query of the
// request shares the one *sql.Tx; only the first use is serialised here.
type RLSScope struct {
	beginner RLSBeginner
	userID   string
	// base is the context the transaction is bound to: the request context at install time, NOT the
	// shorter-lived context of whichever call starts it — a sub-call's timeout must not roll back the
	// request-long transaction.
	base context.Context
	// failOpen: a start failure lets the request run on the pool (reads).
	// Otherwise every scoped statement fails with ErrRLSUnavailable.
	failOpen bool

	mu      sync.Mutex
	started bool // a start was attempted (or the tx was supplied)
	ended   bool // Commit/Rollback ran
	tx      *sql.Tx
	err     error // the start failure, sticky
}

// WithLazyRLS installs a lazy RLS scope for userID on ctx (tx opened through b.BeginRLS on first use);
// the caller must End the scope with Commit or Rollback when the request is done.
func WithLazyRLS(ctx context.Context, b RLSBeginner, userID string, failOpen bool) (context.Context, *RLSScope) {
	s := &RLSScope{beginner: b, userID: userID, base: ctx, failOpen: failOpen}
	return context.WithValue(ctx, rlsTxKey, s), s
}

// WithRLSTx returns a new context carrying an already-started RLS transaction (the caller owns commit
// and rollback). A nil tx clears any scope on ctx.
func WithRLSTx(ctx context.Context, tx *sql.Tx) context.Context {
	if tx == nil {
		return context.WithValue(ctx, rlsTxKey, (*RLSScope)(nil))
	}
	return context.WithValue(ctx, rlsTxKey, &RLSScope{started: true, tx: tx})
}

func rlsScopeFromContext(ctx context.Context) *RLSScope {
	s, _ := ctx.Value(rlsTxKey).(*RLSScope)
	return s
}

// rlsTxFromContext returns the request's RLS transaction, starting it on first use. ok is false when ctx
// carries no (or fail-open failed) scope: the caller uses the pool. err is set when the scope could not be
// started and must not be bypassed.
func rlsTxFromContext(ctx context.Context) (tx *sql.Tx, ok bool, err error) {
	s := rlsScopeFromContext(ctx)
	if s == nil {
		return nil, false, nil
	}
	return s.get()
}

// get starts the transaction if nothing has tried yet and reports it.
func (s *RLSScope) get() (*sql.Tx, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		s.started = true
		s.start()
	}
	// A statement after Commit/Rollback — a goroutine that outlived its request — gets neither the
	// committed transaction nor the pool, fail-open scopes included.
	if s.ended {
		return nil, false, errRLSScopeEnded
	}
	if s.err != nil {
		if s.failOpen {
			return nil, false, nil
		}
		return nil, false, s.err
	}
	return s.tx, true, nil
}

// start opens the transaction; s.mu is held.
func (s *RLSScope) start() {
	switch {
	case s.ended:
		s.err = errRLSScopeEnded
	case s.beginner == nil:
		s.err = fmt.Errorf("%w: no backend", ErrRLSUnavailable)
	default:
		// The transaction outlives the request context's cancellation: a handler cut short by the
		// deadline may still persist completed work under context.WithoutCancel, and database/sql
		// would otherwise roll the tx back the moment the deadline passed. The middleware still ends
		// the scope when the handler returns; every statement runs under its caller's context.
		tx, err := s.beginner.BeginRLS(context.WithoutCancel(s.base), s.userID)
		if err != nil {
			s.err = fmt.Errorf("%w: %w", ErrRLSUnavailable, err)
			return
		}
		s.tx = tx
	}
}

// Started reports whether a transaction is open (started and not failed).
func (s *RLSScope) Started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tx != nil
}

// Err returns the start failure, if any — for the middleware's log line.
func (s *RLSScope) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Commit ends the scope, committing the transaction when one was started. A scope that never started
// returns nil so the caller treats the request as committed (post-commit hooks run).
func (s *RLSScope) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
	if s.tx == nil {
		return nil
	}
	tx := s.tx
	s.tx = nil
	return tx.Commit()
}

// Rollback ends the scope, rolling back the transaction when one was started.
func (s *RLSScope) Rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
	if s.tx == nil {
		return nil
	}
	tx := s.tx
	s.tx = nil
	return tx.Rollback()
}

// EnsureRLS starts the lazy scope on ctx now, for a handler that wants the fail-closed decision before
// doing its work. Returns the start failure of a fail-closed scope; nil when ctx has no scope, the scope
// started, or it failed fail-open (the request runs on the pool).
func EnsureRLS(ctx context.Context) error {
	_, _, err := rlsTxFromContext(ctx)
	return err
}

// hasRLSTx reports whether ctx carries an RLS scope — WITHOUT starting it. Its callers decide whether to
// open (and commit) a transaction of their own: inside a scope they must not. A fail-open failed scope no
// longer counts; an ended scope still does (no own pool transaction for a request that is over).
func hasRLSTx(ctx context.Context) bool {
	s := rlsScopeFromContext(ctx)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended || s.err == nil || !s.failOpen
}

// rlsFailedDB answers every statement with ErrRLSUnavailable. It is a real *sql.DB over a refusing
// connector because DBTX's QueryRowContext returns a *sql.Row, which only database/sql can build.
var (
	rlsFailedOnce sync.Once
	rlsFailedDB   *sql.DB
)

func rlsFailed() *sql.DB {
	rlsFailedOnce.Do(func() { rlsFailedDB = sql.OpenDB(rlsFailConnector{}) })
	return rlsFailedDB
}

type rlsFailConnector struct{}

func (rlsFailConnector) Connect(context.Context) (driver.Conn, error) { return nil, ErrRLSUnavailable }
func (rlsFailConnector) Driver() driver.Driver                        { return rlsFailDriver{} }

type rlsFailDriver struct{}

func (rlsFailDriver) Open(string) (driver.Conn, error) { return nil, ErrRLSUnavailable }

// RLSHookRunner is the txhook.Runner the RLS middleware commits a request's registry with. Each hook gets base
// without its cancellation and a fresh lazy row-level-security scope for the same user: committed when the hook
// returns, rolled back if it panics, and fail-closed if it cannot start. The request's own scope has ended by
// then (every statement on it fails), and running with no scope at all would reach the pool, where the policies
// allow every row.
func RLSHookRunner(base context.Context, b RLSBeginner, userID string) txhook.Runner {
	return func(fn txhook.Hook) {
		ctx, scope := WithLazyRLS(context.WithoutCancel(base), b, userID, false)
		returned := false
		defer func() {
			end := scope.Rollback
			if returned {
				end = scope.Commit
			}
			if err := end(); err != nil {
				slog.Warn("post-commit hook: ending its RLS scope failed", "err", err)
			}
		}()
		fn(ctx)
		returned = true
	}
}

// withTx runs fn inside a transaction, JOINING the request's RLS transaction when ctx carries a scope
// (starting it on this first use) and opening (and committing) its own otherwise. Every write method that
// opens a transaction must go through this. fn gets the context to use inside the transaction: ctx itself
// when joining, and when withTx opens its own, a context carrying that transaction and a post-commit registry
// (txhook), committed after the transaction commits and aborted when it rolls back.
//
// Calling b.db.BeginTx unconditionally takes a SECOND pooled connection while the RLS middleware still
// holds the first for the whole request — a self-deadlock (with the default pool of 25, 13 concurrent
// requests wedge every route). It is a correctness bug too: the inner transaction commits independently
// (its write survives the middleware's rollback), and the second connection carries no app.current_user_id,
// so the RLS policies short-circuit to allow-all for exactly the writes that most need them.
func (b *PostgresStorageBackend) withTx(ctx context.Context, fn func(txCtx context.Context, tx *sql.Tx) error) error {
	rtx, ok, err := rlsTxFromContext(ctx)
	if err != nil {
		return err
	}
	if ok {
		// The middleware owns commit, rollback and the post-commit registry; joining is all we do.
		return fn(ctx, rtx)
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	txCtx, hooks := txhook.With(WithRLSTx(ctx, tx))
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
			hooks.Abort()
		}
	}()
	if err := fn(txCtx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	hooks.Commit(txhook.Detached(ctx))
	return nil
}

// DBTX is the common interface between *sql.DB and *sql.Tx. All tenant-scoped queries should call
// b.query(ctx) to get the right executor.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// query returns the RLS-scoped executor for ctx: the scope's *sql.Tx if the RLS middleware installed one
// (started here on first use), otherwise the pool. A fail-closed scope that could not start yields an
// executor whose every statement fails with ErrRLSUnavailable.
func (b *PostgresStorageBackend) query(ctx context.Context) DBTX {
	tx, ok, err := rlsTxFromContext(ctx)
	if err != nil {
		return rlsFailed()
	}
	if ok {
		return tx
	}
	return b.db
}

// BeginRLS starts a transaction and sets the app.current_user_id session variable so Postgres RLS policies
// can filter rows by the calling user; read-write, because a wrapped request's writes must see the GUC.
// The caller commits or rolls it back.
func (b *PostgresStorageBackend) BeginRLS(ctx context.Context, userID string) (*sql.Tx, error) {
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: false})
	if err != nil {
		return nil, fmt.Errorf("rls: begin tx: %w", err)
	}
	// SET LOCAL cannot be parameterized (utility statement, not a planned query), so set_config(...,
	// is_local => true) is used instead — it accepts a bind parameter and is scoped to this transaction.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_user_id', $1, true)`, userID); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("rls: set user id: %w", err)
	}
	return tx, nil
}
