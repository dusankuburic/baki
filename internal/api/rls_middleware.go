package api

import (
	"log/slog"
	"net/http"
	"strings"

	"pad-analyzer/internal/auth"
	storagedb "pad-analyzer/internal/storage/database"
)

// rlsStreamingPrefixes are path prefixes for long-lived / streaming endpoints
// that must NOT be wrapped in a per-request RLS transaction. Wrapping them would
// hold a database transaction (and pin its pooled connection) open for the
// entire stream / SSE lifetime, exhausting the pool under concurrency. These
// handlers remain protected by the Go-layer authz checks; they simply forgo the
// RLS defense-in-depth layer.
var rlsStreamingPrefixes = []string{
	"/api/events", // server-sent events bus
	"/api/chat/",  // LLM streaming + conversation operations
}

func rlsExemptPath(path string) bool {
	for _, p := range rlsStreamingPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// rlsMiddleware sets the Postgres session variable app.current_user_id for
// every authenticated /api/ request. The variable is scoped to a transaction
// that wraps the entire request so that Row-Level Security policies can filter
// rows by the caller's identity.
//
// The middleware is a no-op when:
//   - JWT auth is disabled (local / desktop mode)
//   - the backend is not Postgres (filesystem backend)
//   - the route is public (no authenticated user)
//   - the route streams (see rlsStreamingPrefixes) — wrapping it would pin a
//     connection for the stream's lifetime
//
// If the RLS transaction cannot be started, the request proceeds without RLS
// (the Go-layer authz is still enforced). A warning is logged so operators can
// detect misconfiguration without taking the service down.
//
// On completion the transaction is committed only when the handler produced a
// success (<400) response; an error response or a panic rolls it back so a
// partially applied write is not persisted.
func (rt *Router) rlsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rt.security.JWTEnabled {
			next.ServeHTTP(w, r)
			return
		}

		if !strings.HasPrefix(r.URL.Path, "/api/") || publicRoutes[r.URL.Path] || rlsExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		claims := auth.ClaimsFromContext(r.Context())
		if claims == nil {
			next.ServeHTTP(w, r)
			return
		}

		rlsBackend, ok := rt.security.Backend.(storagedb.RLSBeginner)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		tx, err := rlsBackend.BeginRLS(r.Context(), claims.UserID)
		if err != nil {
			slog.Warn("rls: failed to begin transaction, proceeding without RLS", "err", err)
			next.ServeHTTP(w, r)
			return
		}

		ctx := storagedb.WithRLSTx(r.Context(), tx)
		ctx, postCommit := storagedb.WithPostCommit(ctx)
		r = r.WithContext(ctx)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// On panic, roll back and re-panic so an upstream recoverer still sees
		// the original failure instead of a silently committed partial write.
		defer func() {
			if p := recover(); p != nil {
				_ = tx.Rollback()
				panic(p)
			}
		}()

		next.ServeHTTP(rec, r)

		if rec.status >= 400 {
			if err := tx.Rollback(); err != nil {
				slog.Warn("rls: rollback after error response failed", "err", err)
			}
			return
		}
		if err := tx.Commit(); err != nil {
			slog.Warn("rls: failed to commit transaction", "err", err)
			return
		}
		// Run deferred work only after the write is durably committed (e.g. blob
		// cleanup for a deleted flow), so a rolled-back request never deletes data
		// that still has a surviving row.
		postCommit.Run()
	})
}

// statusRecorder captures the response status code so the RLS middleware can
// decide whether to commit or roll back, while passing every write straight
// through to the underlying ResponseWriter.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
		r.ResponseWriter.WriteHeader(code)
	}
	// A duplicate WriteHeader is silently dropped: net/http would only log a
	// "superfluous response.WriteHeader call" and the second status never reaches
	// the client anyway. Freezing on the first status also keeps the RLS
	// commit/rollback decision based on the status the client actually received.
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer when it supports flushing so handlers
// that stream incremental output keep working through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
