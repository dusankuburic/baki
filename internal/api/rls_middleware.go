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

// rlsHeavyExactPaths lists CPU- or blob-heavy request paths exempt from the
// per-request RLS transaction for the same reason as the streaming routes:
// each can burn seconds of CPU (parse + 41-rule analysis + iterative fix
// loop) or perform many blob round-trips while holding the transaction, so a
// handful of concurrent calls pins every pooled connection and stalls ALL
// other requests — including health-gated writes.
//
// Every route here was verified to enforce Go-layer authz independently
// (FlowService.GetAuthorized / resolveFlow with viewer/editor perms, or
// user-scoped-only operations), so forgoing RLS removes only the
// defense-in-depth layer, not the access control. Queries still carry their
// explicit owner/org WHERE clauses, and RLS policies short-circuit to "allow"
// when app.current_user_id is unset (app_rls_active()), so exempted routes
// behave exactly like the streaming routes above.
//
// EXACT paths (not prefixes): the /api/analysis tree also contains cheap
// DB-write routes (triage, baselines, policies, comments) that should keep
// the RLS transaction's atomicity guarantees.
var rlsHeavyExactPaths = map[string]bool{
	// Analysis of stored flows — resolve via GetAuthorized("viewer"), then
	// seconds of parse/analyze/export CPU on the resolved document.
	"/api/analysis/analyze":        true,
	"/api/analysis/lineage":        true,
	"/api/analysis/graph":          true,
	"/api/analysis/metrics":        true,
	"/api/analysis/dataflow":       true,
	"/api/analysis/diff":           true,
	"/api/analysis/compare":        true,
	"/api/analysis/subflow-hashes": true,
	"/api/analysis/deduplicate":    true,
	"/api/analysis/related":        true,
	"/api/analysis/export/sarif":   true,
	"/api/analysis/export/junit":   true,
	"/api/analysis/export/csv":     true,
	// Stateless CI payload analysis — nothing is persisted; PAT-authed and
	// body/time bounded by the handler itself.
	"/api/analysis/analyze-raw": true,
	// Desktop-only folder batch (rejected in cloud mode where RLS exists).
	"/api/analysis/batch": true,
	// Flow patch/fix paths — resolveFlow("editor"), then iterative
	// parse+analyze+patch loops over up to 10 MB sources.
	"/api/flow/apply-fix":          true,
	"/api/flow/apply-fix-batch":    true,
	"/api/flow/preview-fix":        true,
	"/api/flow/save-source":        true,
	"/api/flow/suppress-in-source": true,
	// Up to maxLibrarySearchFlows per-flow blob loads behind a user-scoped
	// ListFlows (explicit owner WHERE clause).
	"/api/flow/search-library": true,
	// 10 MB multipart parse + save; role-gated (member) and per-user throttled.
	"/api/flow/upload": true,
}

func rlsExemptPath(path string) bool {
	for _, p := range rlsStreamingPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return rlsHeavyExactPaths[path]
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
