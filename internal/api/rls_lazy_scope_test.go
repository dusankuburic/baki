package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/txhook"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// The RLS middleware used to run BEGIN + set_config + COMMIT on EVERY
// non-exempt authenticated request and pin a pooled connection for its whole
// lifetime, whether or not the handler touched the database. The scope is
// now lazy for reads: the transaction starts on the handler's first scoped
// statement and the middleware commits or rolls back only what was started.
// A mutation's scope is started before its handler runs (it always reaches
// the store, and a start failure must be refused before any handler side
// effect, as the old middleware did).

// lazyRLSBackend claims Postgres RLS support over an in-test driver: it
// counts BeginRLS calls, and the driver counts the commits and rollbacks the
// middleware issues on the transactions it handed out.
type lazyRLSBackend struct {
	storageif.StorageBackend
	db        *sql.DB
	begins    atomic.Int32
	commits   atomic.Int32
	rollbacks atomic.Int32
	beginErr  error
	commitErr error // returned by every COMMIT, to simulate a connection lost at commit time

	usersMu sync.Mutex
	users   []string // the user of every BeginRLS, in order
}

func newLazyRLSBackend(t *testing.T) *lazyRLSBackend {
	t.Helper()
	store := testutil.NewFakeBackend()
	b := &lazyRLSBackend{StorageBackend: store}
	b.db = sql.OpenDB(countingConnector{b: b})
	b.db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = b.db.Close() })
	return b
}

func (b *lazyRLSBackend) BeginRLS(ctx context.Context, userID string) (*sql.Tx, error) {
	b.begins.Add(1)
	b.usersMu.Lock()
	b.users = append(b.users, userID)
	b.usersMu.Unlock()
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.db.BeginTx(ctx, nil)
}

type countingConnector struct{ b *lazyRLSBackend }

func (c countingConnector) Connect(context.Context) (driver.Conn, error) {
	return &countingConn{b: c.b}, nil
}
func (countingConnector) Driver() driver.Driver { return countingDriver{} }

type countingDriver struct{}

func (countingDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("open through the connector")
}

type countingConn struct{ b *lazyRLSBackend }

func (c *countingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("no statements")
}
func (c *countingConn) Close() error { return nil }
func (c *countingConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *countingConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return countingTx{b: c.b}, nil
}

type countingTx struct{ b *lazyRLSBackend }

func (t countingTx) Commit() error   { t.b.commits.Add(1); return t.b.commitErr }
func (t countingTx) Rollback() error { t.b.rollbacks.Add(1); return nil }

// serveLazyRLS runs handler under rt's RLS middleware for an authenticated,
// non-exempt /api/ request and returns the response plus a recovered panic.
func serveLazyRLS(t *testing.T, rt *Router, method string, handler http.HandlerFunc) (rr *httptest.ResponseRecorder, recovered any) {
	t.Helper()
	req := httptest.NewRequest(method, "/api/probe/x", nil)
	req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{UserID: "u-lazy", Email: "u-lazy@example.com", Role: auth.RoleMember}))
	rr = httptest.NewRecorder()
	func() {
		defer func() { recovered = recover() }()
		rt.rlsMiddleware(handler).ServeHTTP(rr, req)
	}()
	return rr, recovered
}

// touchStore stands in for a storage call: it starts the lazy scope the way
// the backend's first scoped statement does.
func touchStore(t *testing.T, r *http.Request) {
	t.Helper()
	if err := storagedb.EnsureRLS(r.Context()); err != nil {
		t.Errorf("EnsureRLS: %v", err)
	}
}

func TestRLSMiddleware_LazyScope(t *testing.T) {
	t.Run("a read that never queries opens no transaction", func(t *testing.T) {
		b := newLazyRLSBackend(t)
		rt := newTestRouter(b, true)
		hooked := false
		rr, p := serveLazyRLS(t, rt, http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
			txhook.Register(r.Context(), func(context.Context) { hooked = true })
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("seen"))
		})
		if p != nil {
			t.Fatalf("panic: %v", p)
		}
		if rr.Code != http.StatusAccepted || rr.Body.String() != "seen" {
			t.Errorf("response changed: %d %q", rr.Code, rr.Body.String())
		}
		if n := b.begins.Load(); n != 0 {
			t.Errorf("BeginRLS x%d for a read that never touched the store, want 0 (eager start)", n)
		}
		if b.commits.Load() != 0 || b.rollbacks.Load() != 0 {
			t.Errorf("commit x%d rollback x%d on a transaction that never started", b.commits.Load(), b.rollbacks.Load())
		}
		if !hooked {
			t.Error("post-commit hook of a request without a transaction did not run after the handler")
		}
	})

	t.Run("a mutation starts its transaction before the handler", func(t *testing.T) {
		b := newLazyRLSBackend(t)
		rt := newTestRouter(b, true)
		var beginsInHandler int32
		hooked := false
		rr, p := serveLazyRLS(t, rt, http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
			beginsInHandler = b.begins.Load()
			txhook.Register(r.Context(), func(context.Context) { hooked = true })
			w.WriteHeader(http.StatusCreated)
		})
		if p != nil {
			t.Fatalf("panic: %v", p)
		}
		if rr.Code != http.StatusCreated {
			t.Errorf("status %d", rr.Code)
		}
		if beginsInHandler != 1 {
			t.Errorf("BeginRLS x%d when the mutation's handler started, want 1 (started before any handler code)", beginsInHandler)
		}
		if b.begins.Load() != 1 || b.commits.Load() != 1 || b.rollbacks.Load() != 0 {
			t.Errorf("begin x%d commit x%d rollback x%d, want 1/1/0", b.begins.Load(), b.commits.Load(), b.rollbacks.Load())
		}
		if !hooked {
			t.Error("post-commit hook did not run")
		}
	})

	t.Run("a read that queries gets one begin and one commit", func(t *testing.T) {
		b := newLazyRLSBackend(t)
		rt := newTestRouter(b, true)
		hooked := false
		hookedBeforeCommit := false
		rr, p := serveLazyRLS(t, rt, http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
			if n := b.begins.Load(); n != 0 {
				t.Errorf("BeginRLS x%d before the read's first statement, want 0", n)
			}
			touchStore(t, r)
			touchStore(t, r) // the second statement joins the same transaction
			txhook.Register(r.Context(), func(context.Context) {
				hooked = true
				hookedBeforeCommit = b.commits.Load() == 0
			})
			w.WriteHeader(http.StatusOK)
		})
		if p != nil {
			t.Fatalf("panic: %v", p)
		}
		if rr.Code != http.StatusOK {
			t.Errorf("status %d", rr.Code)
		}
		if n := b.begins.Load(); n != 1 {
			t.Errorf("BeginRLS x%d, want exactly 1", n)
		}
		if b.commits.Load() != 1 || b.rollbacks.Load() != 0 {
			t.Errorf("commit x%d rollback x%d, want 1 commit and no rollback", b.commits.Load(), b.rollbacks.Load())
		}
		if !hooked {
			t.Error("post-commit hook did not run")
		}
		if hookedBeforeCommit {
			t.Error("post-commit hook ran before the commit")
		}
	})

	t.Run("an error response after querying rolls back", func(t *testing.T) {
		b := newLazyRLSBackend(t)
		rt := newTestRouter(b, true)
		hooked := false
		rr, p := serveLazyRLS(t, rt, http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
			touchStore(t, r)
			txhook.Register(r.Context(), func(context.Context) { hooked = true })
			w.WriteHeader(http.StatusConflict)
		})
		if p != nil {
			t.Fatalf("panic: %v", p)
		}
		if rr.Code != http.StatusConflict {
			t.Errorf("status %d", rr.Code)
		}
		if b.begins.Load() != 1 || b.commits.Load() != 0 || b.rollbacks.Load() != 1 {
			t.Errorf("begin x%d commit x%d rollback x%d, want 1 begin, 1 rollback and no commit", b.begins.Load(), b.commits.Load(), b.rollbacks.Load())
		}
		if hooked {
			t.Error("post-commit hook ran for a rolled-back request")
		}
	})

	t.Run("a panic after querying rolls back and re-panics", func(t *testing.T) {
		b := newLazyRLSBackend(t)
		rt := newTestRouter(b, true)
		_, p := serveLazyRLS(t, rt, http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
			touchStore(t, r)
			panic("boom")
		})
		if p != "boom" {
			t.Errorf("recovered %v, want the handler's panic", p)
		}
		if b.commits.Load() != 0 || b.rollbacks.Load() != 1 {
			t.Errorf("commit x%d rollback x%d, want 1 rollback and no commit", b.commits.Load(), b.rollbacks.Load())
		}
	})

	t.Run("a read's error response without querying rolls nothing back", func(t *testing.T) {
		b := newLazyRLSBackend(t)
		rt := newTestRouter(b, true)
		hooked := false
		rr, _ := serveLazyRLS(t, rt, http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
			txhook.Register(r.Context(), func(context.Context) { hooked = true })
			w.WriteHeader(http.StatusBadRequest)
		})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status %d", rr.Code)
		}
		if b.begins.Load() != 0 || b.commits.Load() != 0 || b.rollbacks.Load() != 0 {
			t.Errorf("begin x%d commit x%d rollback x%d for a 400 that never touched the store", b.begins.Load(), b.commits.Load(), b.rollbacks.Load())
		}
		if hooked {
			t.Error("post-commit hook ran for an error response")
		}
	})

	t.Run("a mutation whose start fails is refused before the handler", func(t *testing.T) {
		b := newLazyRLSBackend(t)
		b.beginErr = errors.New("pool exhausted")
		rt := newTestRouter(b, true)
		invoked := false
		rr, _ := serveLazyRLS(t, rt, http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
			invoked = true
			w.WriteHeader(http.StatusOK)
		})
		if invoked {
			t.Error("the mutation's handler ran without its RLS transaction (side effects before the 503)")
		}
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("mutation status %d, want 503", rr.Code)
		}
		if n := b.begins.Load(); n != 1 {
			t.Errorf("BeginRLS x%d, want 1 attempt", n)
		}
	})

	t.Run("a read whose start fails still runs, lazily, on the pool", func(t *testing.T) {
		b := newLazyRLSBackend(t)
		b.beginErr = errors.New("pool exhausted")
		rt := newTestRouter(b, true)

		// No statement: the start is never attempted.
		rr, _ := serveLazyRLS(t, rt, http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		if rr.Code != http.StatusOK || b.begins.Load() != 0 {
			t.Errorf("read without statements: status %d, BeginRLS x%d; want 200 and no attempt", rr.Code, b.begins.Load())
		}

		// With a statement: one attempt, fail-open.
		var gotErr error
		rr, _ = serveLazyRLS(t, rt, http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
			gotErr = storagedb.EnsureRLS(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		if gotErr != nil || rr.Code != http.StatusOK {
			t.Errorf("read: err %v status %d, want fail-open (nil, 200)", gotErr, rr.Code)
		}
		if n := b.begins.Load(); n != 1 {
			t.Errorf("BeginRLS x%d, want one attempt", n)
		}
	})
}
