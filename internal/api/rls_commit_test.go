package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/txhook"
	storagedb "pad-analyzer/internal/storage/database"
)

// serveRLSInto is serveLazyRLS writing into rr, so a test can pre-set response headers the outer layers would
// have added (request ID, security headers) before the RLS middleware runs.
func serveRLSInto(rt *Router, rr *httptest.ResponseRecorder, method string, handler http.HandlerFunc) (recovered any) {
	req := httptest.NewRequest(method, "/api/probe/x", nil)
	req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{UserID: "u-lazy", Email: "u-lazy@example.com", Role: auth.RoleMember}))
	defer func() { recovered = recover() }()
	rt.rlsMiddleware(handler).ServeHTTP(rr, req)
	return nil
}

// TestRLS_CommitFailureAnswers503 (R5): the transaction is committed before the first byte of a 2xx goes out, so
// when the COMMIT fails the client gets a 503 instead of the handler's success. Headers the outer layers set
// survive; what the handler set for its own response (cookies, ETag) does not; its post-commit work never runs.
func TestRLS_CommitFailureAnswers503(t *testing.T) {
	for _, writes := range []bool{true, false} {
		name := "handler writes a body"
		if !writes {
			name = "handler writes nothing (implicit 200)"
		}
		t.Run(name, func(t *testing.T) {
			b := newLazyRLSBackend(t)
			b.commitErr = errors.New("connection reset at COMMIT")
			rt := newTestRouter(b, true)
			rr := httptest.NewRecorder()
			rr.Header().Set("X-Request-Id", "rid-1")
			hooked := false
			p := serveRLSInto(rt, rr, http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
				touchStore(t, r)
				txhook.Register(r.Context(), func(context.Context) { hooked = true })
				w.Header().Set("Set-Cookie", "session=new")
				w.Header().Set("ETag", `"v2"`)
				if writes {
					render.JSON(w, map[string]string{"saved": "yes"})
					if _, err := w.Write([]byte("more")); err == nil {
						t.Error("a write after the failed commit must report an error")
					}
				}
			})
			if p != nil {
				t.Fatalf("panic: %v", p)
			}
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503 (the write did not persist)", rr.Code)
			}
			if strings.Contains(rr.Body.String(), "saved") || !strings.Contains(rr.Body.String(), `"SERVICE_UNAVAILABLE"`) {
				t.Errorf("body %q leaked the handler's response or lacks the retryable error code", rr.Body.String())
			}
			if rr.Header().Get("X-Request-Id") != "rid-1" {
				t.Error("the outer layers' headers must survive the 503")
			}
			if rr.Header().Get("Set-Cookie") != "" || rr.Header().Get("ETag") != "" {
				t.Errorf("handler headers leaked into the 503: cookie %q etag %q", rr.Header().Get("Set-Cookie"), rr.Header().Get("ETag"))
			}
			if hooked {
				t.Error("post-commit work ran although the commit failed")
			}
		})
	}
}

// TestRLS_PanicAfterCommitStillRunsHooks: once the response started, the write is committed; a later panic must
// not drop the post-commit work that keeps storage consistent (e.g. the blob purge of a deleted flow).
func TestRLS_PanicAfterCommitStillRunsHooks(t *testing.T) {
	b := newLazyRLSBackend(t)
	rt := newTestRouter(b, true)
	hooked := false
	p := serveRLSInto(rt, httptest.NewRecorder(), http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		touchStore(t, r)
		txhook.Register(r.Context(), func(context.Context) { hooked = true })
		w.WriteHeader(http.StatusOK)
		panic("after the response")
	})
	if p != "after the response" {
		t.Fatalf("recovered %v, want the handler's panic re-raised", p)
	}
	if b.commits.Load() != 1 || b.rollbacks.Load() != 0 {
		t.Errorf("commit x%d rollback x%d, want the commit to stand", b.commits.Load(), b.rollbacks.Load())
	}
	if !hooked {
		t.Error("post-commit work of a committed write was dropped by a later panic")
	}
}

// TestRLS_HookGetsItsOwnScopeForTheSameUser: a hook runs after the request's scope ended. Its database access
// goes through a fresh lazy RLS scope for the same user (not the pool, where the policies allow every row),
// committed when the hook returns.
func TestRLS_HookGetsItsOwnScopeForTheSameUser(t *testing.T) {
	b := newLazyRLSBackend(t)
	rt := newTestRouter(b, true)
	var hookErr error
	ran := false
	p := serveRLSInto(rt, httptest.NewRecorder(), http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		touchStore(t, r)
		txhook.Register(r.Context(), func(ctx context.Context) {
			ran = true
			if ctx.Err() != nil {
				t.Error("the hook's context inherited the request's cancellation")
			}
			hookErr = storagedb.EnsureRLS(ctx) // starts the hook's own scope, as its first statement would
		})
		w.WriteHeader(http.StatusNoContent)
	})
	if p != nil {
		t.Fatalf("panic: %v", p)
	}
	if !ran || hookErr != nil {
		t.Fatalf("hook ran=%v, scope start err=%v", ran, hookErr)
	}
	if !slices.Equal(b.users, []string{"u-lazy", "u-lazy"}) {
		t.Errorf("BeginRLS users %v, want the request's and then the hook's scope, both for u-lazy", b.users)
	}
	if b.commits.Load() != 2 || b.rollbacks.Load() != 0 {
		t.Errorf("commit x%d rollback x%d, want both scopes committed", b.commits.Load(), b.rollbacks.Load())
	}
}

// TestTxResponseWriter_DecidesOnFirstOutput pins when the transaction is decided: the first final status, first
// write or flush; 1xx responses do not decide; an error status rolls back (end is told the status).
func TestTxResponseWriter_DecidesOnFirstOutput(t *testing.T) {
	var decided []int
	end := func(status int) bool { decided = append(decided, status); return true }

	tw := newTxResponseWriter(httptest.NewRecorder(), end)
	tw.WriteHeader(http.StatusEarlyHints)
	if len(decided) != 0 {
		t.Fatalf("a 1xx decided the transaction: %v", decided)
	}
	tw.Flush()
	tw.WriteHeader(http.StatusConflict) // too late: the flush already sent a 200
	tw.finish()
	if !slices.Equal(decided, []int{http.StatusOK}) || !tw.committed() {
		t.Fatalf("decided %v committed=%v, want one decision at the flush (200)", decided, tw.committed())
	}

	decided = nil
	tw = newTxResponseWriter(httptest.NewRecorder(), end)
	tw.WriteHeader(http.StatusBadRequest)
	_, _ = tw.Write([]byte("bad"))
	tw.finish()
	if !slices.Equal(decided, []int{http.StatusBadRequest}) || tw.committed() {
		t.Fatalf("decided %v committed=%v, want one decision for the 400 (a rollback)", decided, tw.committed())
	}
}
