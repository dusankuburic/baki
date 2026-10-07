package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/txhook"
	storagedb "pad-analyzer/internal/storage/database"
)

// rlsStreamingPrefixes: streaming endpoints that must NOT be wrapped in the per-request RLS transaction — it would pin a pooled connection for the stream's lifetime.
// Go-layer authz still protects them; they forgo the RLS defense-in-depth layer only.
var rlsStreamingPrefixes = []string{
	"/api/events", // server-sent events bus
	"/api/chat/",  // LLM streaming + conversation operations
}

// rlsHeavyExactPaths: CPU- or blob-heavy paths exempt from the per-request RLS transaction — each can burn seconds of CPU or many blob round-trips while holding it, pinning every pooled connection.
// Every route here was verified to enforce Go-layer authz independently (GetAuthorized / resolveFlow, or user-scoped-only operations) and keeps explicit owner/org WHERE clauses — forgoing RLS removes only the defense-in-depth layer. EXACT paths, not prefixes: the /api/analysis tree also contains cheap DB-write routes (triage, baselines, policies, comments) that keep the transaction's atomicity.
var rlsHeavyExactPaths = map[string]bool{
	// GetAuthorized("viewer"), then seconds of parse/analyze/export CPU.
	"/api/analysis/analyze": true,
	// Six concurrent pool reads per request, each with its own flow-id/user-id predicate.
	"/api/dashboard/analytics":   true,
	"/api/analysis/lineage":      true,
	"/api/analysis/graph":        true,
	"/api/analysis/dataflow":     true,
	"/api/analysis/diff":         true,
	"/api/analysis/compare":      true,
	"/api/analysis/deduplicate":  true,
	"/api/analysis/related":      true,
	"/api/analysis/export/sarif": true,
	"/api/analysis/export/junit": true,
	// Parses the flow and runs every rule to decide which pad-ignore directives are stale.
	"/api/analysis/suppressions": true,
	// Stateless CI payload analysis — nothing persisted; PAT-authed and bounded.
	"/api/analysis/analyze-raw": true,
	// Desktop-only folder batch (rejected in cloud mode where RLS exists).
	"/api/analysis/batch": true,
	// resolveFlow("editor"), then iterative parse+analyze+patch loops over up to 10 MB sources.
	"/api/flow/apply-fix":          true,
	"/api/flow/apply-fix-batch":    true,
	"/api/flow/preview-fix":        true,
	"/api/flow/save-source":        true,
	"/api/flow/suppress-in-source": true,
	// Per-flow blob loads behind a user-scoped ListFlows (owner WHERE).
	"/api/flow/search-library": true,
	// 10 MB multipart parse + save; role-gated (member) and per-user throttled.
	"/api/flow/upload": true,
	// resolveFlow("viewer"), then the search index is built over the whole document; nothing written.
	"/api/flow/search": true,
	// GetAuthorized("viewer"), then seconds of pure rendering (Markdown, the HTML template, the PDF rasteriser); the only DB work is the audit row via its own
	// writer. /compare and /save are desktop-only and keep their transaction.
	"/api/export/markdown": true,
	"/api/export/html":     true,
	"/api/export/pdf":      true,
	// ~16 read-only queries run CONCURRENTLY, each with its explicit owner_id predicate; on the request-long transaction a canceled sibling poisoned the
	// shared *sql.Tx's connection ("driver: bad connection"). Pool connections retry cleanly per query.
	"/api/dashboard/home": true,
	// Provider routes that dial OUTBOUND while the handler runs: live model listings (~8.5 s), the billed connection test, the GitHub/Copilot device-flow polls.
	// None touches the RLS-scoped backend (credentials go through the keystore's own connection) — the transaction pinned a pooled connection for the whole dial. The local siblings (revoke, the config read/save) keep their RLS transaction.
	"/api/providers/list":          true,
	"/api/providers/test":          true,
	"/api/providers/github/start":  true,
	"/api/providers/github/poll":   true,
	"/api/providers/github/user":   true,
	"/api/providers/copilot/start": true,
	"/api/providers/copilot/poll":  true,
	"/api/providers/copilot/user":  true,
}

// rlsBlobContentPath matches the dynamic library routes that stream stored blobs (flow content, a stored version, the two-blob diff) — exact-path exemption is impossible (they carry IDs), and inside the request-long transaction they pinned a pooled connection for the whole blob transfer.
// Go-layer authz is enforced BEFORE the blob read; the /versions save and restore mutations keep their transaction (multi-step writes want atomicity). Segments are matched exactly on the dispatch path (routePath), so /{id}/versions/ (the save) must not read as a version.
func rlsBlobContentPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/library/")
	if !ok {
		return false
	}
	segs := strings.Split(rest, "/")
	if segs[0] == "" {
		return false
	}
	switch len(segs) {
	case 2:
		return segs[1] == "content"
	case 3:
		return segs[1] == "versions" && segs[2] != ""
	case 4:
		return segs[1] == "versions" && segs[2] != "" && segs[3] == "diff"
	}
	return false
}

func rlsExemptPath(path string) bool {
	for _, p := range rlsStreamingPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	if rlsBlobContentPath(path) {
		return true
	}
	// The org admin's shared-credential test dials upstream up to 15 s like /api/providers/test;
	// its org lookups are Go-layer authz (canOrg, OrgAdmin) and the credential uses the keystore's connection.
	if isOrgProviderTestPath(path) {
		return true
	}
	// The Azure AI Foundry registry's test and discovery routes (own and org) dial Azure for up
	// to 30 s; registry reads/writes carry explicit owner predicates and CRUD keeps its transaction.
	if _, outbound := azureOutboundRoute(path); outbound {
		return true
	}
	// The knowledge re-index walks up to 10 000 chunks through the embedding provider (minutes) and must commit page by page — inside the request transaction a failure on page 3 rolled back the paid-for pages 1-2 (the upload has the same shape at smaller scale).
	// Each write opens its own short RLS transaction as the caller, so the knowledge policies still apply.
	if isKnowledgeEmbedPath(path) {
		return true
	}
	// The org notification-channel test dials the webhook for up to 15 s; requireOrg(OrgAdmin) is its
	// access control and ListOrgChannels carries an explicit org_id predicate.
	if isOrgChannelTestPath(path) {
		return true
	}
	return rlsHeavyExactPaths[path]
}

// isKnowledgeEmbedPath matches the two knowledge routes that walk the embedding provider: POST /api/orgs/{id}/knowledge/{reindex,upload} (path already canonical — the /api/v1 alias is stripped before this runs).
func isKnowledgeEmbedPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/orgs/")
	if !ok {
		return false
	}
	orgID, tail, ok := strings.Cut(rest, "/")
	return ok && orgID != "" && (tail == "knowledge/reindex" || tail == "knowledge/upload")
}

// isOrgChannelTestPath matches POST /api/orgs/{id}/channels/{channelId}/test
// segment by segment (no empty ids, nothing after "test").
func isOrgChannelTestPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/orgs/")
	if !ok {
		return false
	}
	segs := strings.Split(rest, "/")
	return len(segs) == 4 && segs[0] != "" && segs[1] == "channels" && segs[2] != "" && segs[3] == "test"
}

// rlsMiddleware sets app.current_user_id for authenticated /api/ requests that are not exempt, in a transaction wrapping the entire request: LAZY for reads (first scoped statement, storagedb.RLSScope), fail-closed for mutations (starts before the handler; a start failure is a 503).
// No-op without JWT auth, Postgres, or on public/exempt routes; /ws and background work (scanner, retention, audit writer, notifications) are outside it entirely and rest on Go-layer authz plus explicit clauses — RLS policies allow every row when app.current_user_id is unset. Ends the transaction before the first response byte (txResponseWriter): below 400 it commits, and a failed commit becomes a 503; 400 and above, or a panic before the response started, roll back.
func (rt *Router) rlsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rt.security.JWTEnabled {
			next.ServeHTTP(w, r)
			return
		}

		// The route policy decides (wantsTx); the legacy exemptions are judged on the path chi dispatches on
		// (routePath): a RawPath such as /api/library/..%2Fchat%2Fx decodes and cleans to the exempt /api/chat/x
		// while chi runs a transactional library route.
		if !rt.wantsTx(r) {
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

		// The scope is LAZY for reads: BEGIN + set_config run on the first scoped statement, so a read that never touches the store pins nothing.
		// MUTATING requests fail CLOSED — without the transaction the policies short-circuit to allow-all — so a mutation's scope starts HERE, before handler code, landing the 503 before its non-transactional side effects (keystore writes, provider calls, blob uploads, audit rows). Reads stay fail-open (explicit ownership predicates also scope them).
		failOpen := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		base := r.Context()
		ctx, scope := storagedb.WithLazyRLS(base, rlsBackend, claims.UserID, failOpen)
		if !failOpen {
			if err := storagedb.EnsureRLS(ctx); err != nil {
				slog.Error("rls: failed to begin transaction, refusing mutation", "method", r.Method, "path", r.URL.Path, "err", err)
				render.Error(w, errRLSUnavailableResponse, http.StatusServiceUnavailable)
				return
			}
		}
		ctx, hooks := txhook.With(ctx)
		r = r.WithContext(ctx)
		// Post-commit work runs with a fresh RLS scope for the same user per hook: the request's own scope has ended.
		runHooks := func() { hooks.Commit(storagedb.RLSHookRunner(base, rlsBackend, claims.UserID)) }

		tw := newTxResponseWriter(w, func(status int) bool {
			if status >= http.StatusBadRequest {
				if err := scope.Rollback(); err != nil {
					slog.Warn("rls: rollback after error response failed", "err", err)
				}
				hooks.Abort()
				return true
			}
			// Commit is a no-op for a scope that never started: the request wrote nothing through the
			// transaction, so it is "committed" as it stands.
			if err := scope.Commit(); err != nil {
				slog.Error("rls: commit failed; answering 503 instead of the handler's response", "method", r.Method, "path", r.URL.Path, "err", err)
				hooks.Abort()
				return false
			}
			return true
		})

		// A panic before the response started rolls back. After it the write is already committed, so its
		// post-commit work still runs. Either way the panic continues to the recoverer above.
		defer func() {
			if p := recover(); p != nil {
				if tw.committed() {
					runHooks()
				} else {
					_ = scope.Rollback()
					hooks.Abort()
				}
				panic(p)
			}
		}()

		next.ServeHTTP(tw, r)

		// Only a read can get here with a start failure (a mutation's was
		// refused above); it ran on the pool.
		if err := scope.Err(); err != nil {
			slog.Warn("rls: failed to begin transaction, proceeded without RLS (read-only)", "method", r.Method, "path", r.URL.Path, "err", err)
		}
		// A handler that wrote nothing is decided now (net/http then sends its implicit 200, or the 503).
		tw.finish()
		// Deferred work runs only after the write is durable (e.g. blob cleanup for a deleted flow): a
		// rolled-back request must not delete data whose row survives.
		if tw.committed() {
			runHooks()
		}
	})
}

// errRLSUnavailableResponse is the error rendered when the request's transaction cannot be started or committed.
// Like every 5xx its message is masked on the wire; the envelope's SERVICE_UNAVAILABLE code tells the client to
// retry, and nothing about the database leaks.
var errRLSUnavailableResponse = errors.New("temporarily unavailable — retry")

// txResponseWriter ends the request's RLS transaction BEFORE the first byte of the response leaves. A response
// below 400 is committed first, and a commit failure turns it into a 503, so a client is never told that a write
// succeeded which did not persist (the response used to stream through, with Commit running after the handler
// returned and its failure only logged). A response of 400 or more rolls back. A handler that writes nothing
// is decided when it returns (finish). After the commit the scope has ended, so the handler must not touch
// the database once it has started its response.
type txResponseWriter struct {
	http.ResponseWriter
	end     func(status int) bool // commits or rolls back for status; false = the commit failed
	entry   http.Header           // the headers as they were before the handler ran
	status  int
	decided bool
	failed  bool // the commit failed: a 503 went out instead, later writes are swallowed
}

func newTxResponseWriter(w http.ResponseWriter, end func(status int) bool) *txResponseWriter {
	return &txResponseWriter{ResponseWriter: w, end: end, entry: w.Header().Clone(), status: http.StatusOK}
}

// decide ends the transaction for status, once. On a failed commit it answers 503 itself, with the headers
// restored to what they were before the handler ran: the security, CORS and request-ID headers stay, while
// what the handler set for its own response (cookies, ETag, Content-Length) goes.
func (t *txResponseWriter) decide(status int) {
	if t.decided {
		return
	}
	t.decided = true
	t.status = status
	if t.end(status) {
		return
	}
	t.failed = true
	h := t.Header()
	for k := range h {
		delete(h, k)
	}
	for k, v := range t.entry {
		h[k] = v
	}
	render.Error(t.ResponseWriter, errRLSUnavailableResponse, http.StatusServiceUnavailable)
}

func (t *txResponseWriter) WriteHeader(code int) {
	if code < http.StatusOK {
		// 1xx is informational, not the response: nothing to decide yet.
		if !t.decided {
			t.ResponseWriter.WriteHeader(code)
		}
		return
	}
	if t.decided {
		return // a second WriteHeader is dropped, as net/http would
	}
	t.decide(code)
	if !t.failed {
		t.ResponseWriter.WriteHeader(code)
	}
}

func (t *txResponseWriter) Write(b []byte) (int, error) {
	if !t.decided {
		t.WriteHeader(http.StatusOK)
	}
	if t.failed {
		return 0, errRLSUnavailableResponse
	}
	return t.ResponseWriter.Write(b)
}

// Flush commits like a first write, then forwards to the underlying writer when it supports flushing, so
// handlers that stream incremental output keep working through the wrapper.
func (t *txResponseWriter) Flush() {
	if !t.decided {
		t.WriteHeader(http.StatusOK)
	}
	if t.failed {
		return
	}
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer — without it, an RLS-wrapped
// streaming handler could not lift the server's WriteTimeout and was cut off mid-stream.
func (t *txResponseWriter) Unwrap() http.ResponseWriter {
	return t.ResponseWriter
}

// finish decides a response the handler did not write.
func (t *txResponseWriter) finish() {
	t.decide(t.status)
}

// committed reports whether the transaction committed (as opposed to: not decided yet, rolled back, or a
// failed commit).
func (t *txResponseWriter) committed() bool {
	return t.decided && !t.failed && t.status < http.StatusBadRequest
}
