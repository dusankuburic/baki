package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	storagedb "pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// These tests pin one invariant: every path-based policy decision (auth and public routes, SSE query token, PAT scope, RLS, rate limits, /assets/) is made on the path chi DISPATCHES on — r.URL.RawPath verbatim under non-default escapes — not on the cleaned, decoded r.URL.Path.
// The old cleaned-path decisions let e.g. /api/system/settings/org/..%2F..%2F..%2Fhealth hit a protected route as if public (jwtAuth waved it through; the handler answered 403 where jwtAuth must answer 401).

// routeProbe is a handler mounted on a test router's mux: it counts the
// requests that reach it and records what the request carried there.
type routeProbe struct {
	hits      atomic.Int32
	id        atomic.Value // string: chi's {id}
	routePath atomic.Value // string: routePath(r) inside the mux
	anonymous atomic.Bool  // the last hit carried no claims
}

func (p *routeProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The RLS scope starts lazily, on a handler's first scoped statement; the probe stands in for such a handler,
	// starting the scope itself and answering 503 where a mutation's statements would fail.
	if err := storagedb.EnsureRLS(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	p.hits.Add(1)
	p.id.Store(chi.URLParam(r, "id"))
	p.routePath.Store(routePath(r))
	p.anonymous.Store(auth.ClaimsFromContext(r.Context()) == nil)
	w.WriteHeader(http.StatusOK)
}

func (p *routeProbe) lastID() string {
	s, _ := p.id.Load().(string)
	return s
}

func (p *routeProbe) lastRoutePath() string {
	s, _ := p.routePath.Load().(string)
	return s
}

// mountProbes adds the probe routes to rt's mux: a protected {id} route under a prefix no policy special-cases, and one under /api/admin/ (the admin capability for scoped PATs); chi accepts routes after Use.
func mountProbes(rt *Router) (plain, admin *routeProbe) {
	plain, admin = &routeProbe{}, &routeProbe{}
	rt.mux.Handle("/api/probe/{id}", plain)
	rt.mux.Handle("/api/admin/probe/{id}", admin)
	return plain, admin
}

// fullChain wraps rt in BuildHandler's complete stack (with the per-IP limiters when tweak selects cloud mode) so the tests run requests exactly the way the server does.
func fullChain(t *testing.T, rt *Router, tweak func(*config.Config)) http.Handler {
	t.Helper()
	cfg := config.Default()
	if tweak != nil {
		tweak(cfg)
	}
	h, limiters := BuildHandler(rt, cfg, nil)
	t.Cleanup(func() {
		for _, l := range limiters {
			l.Stop()
		}
	})
	return h
}

func serve(h http.Handler, method, target, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// A request whose RawPath cleans to a public (or non-/api) path but that chi dispatches to a protected route is authenticated for the protected route: 401 without credentials, and the handler never runs.
func TestJWTAuth_EncodedPathAuthenticatesTheDispatchedRoute(t *testing.T) {
	rt := newJWTTestRouter(t)
	probe, _ := mountProbes(rt)
	h := fullChain(t, rt, nil)

	for _, target := range []string{
		"/api/probe/..%2F..%2Fapi%2Fhealth",            // decoded + cleaned: /api/health (public)
		"/api/probe/..%2F..%2Fapi%2Fsystem%2Ffeatures", // /api/system/features (public)
		"/api/probe/..%2F..%2Fhealthz",                 // /healthz (outside /api/)
		"/api/probe/..%2F..%2Fassets%2Fapp.js",         // /assets/app.js (outside /api/)
		"/api/v1/probe/..%2F..%2F..%2Fapi%2Fhealth",    // via the version alias
	} {
		rr := serve(h, http.MethodGet, target, "")
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401 (body %s)", target, rr.Code, rr.Body.String())
		}
	}
	if n := probe.hits.Load(); n != 0 {
		t.Errorf("the protected handler ran %d time(s) for anonymous requests", n)
	}

	// The live case: the org-settings route answered 403 from its own
	// membership check, i.e. jwtAuth had let the anonymous request through.
	target := "/api/system/settings/org/..%2F..%2F..%2Fhealth"
	if rr := serve(h, http.MethodGet, target, ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET %s = %d, want 401 (body %s)", target, rr.Code, rr.Body.String())
	}

	// Public is exact on the dispatch path, so it fails closed: a needless escape is not the public route's spelling (chi would not route it there either).
	target = "/api/%68ealth"
	if rr := serve(h, http.MethodGet, target, ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET %s = %d, want 401 (body %s)", target, rr.Code, rr.Body.String())
	}
}

// Legitimate ids with an escaped "/" (the SPA encodes every path segment; frontend/src/api/providers.ts azurePath) keep routing to their handler — verbatim, as chi hands them over — and keep authenticating.
func TestJWTAuth_EncodedSlashInIDStillRoutes(t *testing.T) {
	rt := newJWTTestRouter(t)
	probe, _ := mountProbes(rt)
	h := fullChain(t, rt, nil)
	bearer := jwtBearer(t, rt, "u-1", "u1@example.com")

	for _, target := range []string{"/api/probe/m%2Fabcdefghij", "/api/v1/probe/m%2Fabcdefghij"} {
		before := probe.hits.Load()
		if rr := serve(h, http.MethodGet, target, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401", target, rr.Code)
		}
		rr := serve(h, http.MethodGet, target, bearer)
		if rr.Code != http.StatusOK {
			t.Fatalf("authenticated GET %s = %d, want 200 (body %s)", target, rr.Code, rr.Body.String())
		}
		if got := probe.hits.Load() - before; got != 1 {
			t.Errorf("GET %s reached the handler %d time(s), want once (the authenticated request)", target, got)
		}
		if probe.anonymous.Load() {
			t.Errorf("GET %s reached the handler without the caller's claims", target)
		}
		if id := probe.lastID(); id != "m%2Fabcdefghij" {
			t.Errorf("GET %s: {id} = %q, want the escaped segment verbatim", target, id)
		}
	}

	// A real registry route: the handler answers (a JSON error code), not
	// the router's plain-text 404.
	target := "/api/providers/azure-foundry/models/m%2Fabcdefghij"
	if rr := serve(h, http.MethodGet, target, ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET %s = %d, want 401", target, rr.Code)
	}
	rr := serve(h, http.MethodGet, target, bearer)
	if !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Errorf("authenticated GET %s = %d %q: never reached the registry handler (body %s)", target, rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
}

// The access token may ride in the query string only to the SSE route, judged on the route chi dispatches on. auth.ExtractToken read the cleaned, decoded Path, so ?token= authenticated any route whose RawPath cleaned to /api/events.
func TestJWTAuth_QueryTokenOnlyOnTheDispatchedSSERoute(t *testing.T) {
	rt := newJWTTestRouter(t)
	probe, _ := mountProbes(rt)
	h := fullChain(t, rt, nil)
	token := strings.TrimPrefix(jwtBearer(t, rt, "u-1", "u1@example.com"), "Bearer ")

	for _, target := range []string{
		"/api/probe/x",
		"/api/probe/..%2Fevents",    // decoded + cleaned: /api/events
		"/api/v1/probe/..%2Fevents", // via the version alias
		"/api/auth/sessions/..%2F..%2Fevents",
	} {
		method := http.MethodGet
		if strings.HasPrefix(target, "/api/auth/sessions/") {
			method = http.MethodDelete // the real revoke route
		}
		if rr := serve(h, method, target+"?token="+token, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s?token=<jwt> = %d, want 401 (body %s)", method, target, rr.Code, rr.Body.String())
		}
	}
	if n := probe.hits.Load(); n != 0 {
		t.Errorf("a query-string token reached a non-SSE handler %d time(s)", n)
	}

	// The SSE route itself still takes it, under either spelling. The stream
	// runs until the request context ends.
	for _, target := range []string{"/api/events", "/api/v1/events"} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		req := httptest.NewRequest(http.MethodGet, target+"?token="+token, nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		cancel()
		if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/event-stream") {
			t.Errorf("GET %s?token=<jwt> = %d %q, want the event stream (body %s)", target, rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
		}
	}
}

// Public routes — every canonical and non-canonical spelling that carries
// no escapes — still work without credentials.
func TestJWTAuth_PublicRoutesStayPublic(t *testing.T) {
	rt := newJWTTestRouter(t)
	h := fullChain(t, rt, nil)
	for _, target := range []string{"/api/system/features", "/api/v1/system/features", "/api/./system/features", "/api//system/features", "/healthz"} {
		if rr := serve(h, http.MethodGet, target, ""); rr.Code != http.StatusOK {
			t.Errorf("anonymous GET %s = %d, want 200 (body %s)", target, rr.Code, rr.Body.String())
		}
	}
}

// A scoped PAT's capabilities are checked against the route chi dispatches on: a write-only token must not reach /api/admin/ through a RawPath that cleans to a write route, nor a read-only token mutate through one that cleans to the read-level /api/auth/me.
func TestAPIToken_ScopeCheckedOnTheDispatchedRoute(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	probe, adminProbe := mountProbes(rt)
	writer := "Bearer " + mintToken(t, rt, "ci-admin", "ci-admin@example.com", auth.RoleAdmin, nil, auth.ScopeWrite)
	reader := "Bearer " + mintToken(t, rt, "ci-reader", "ci-reader@example.com", auth.RoleMember, nil, auth.ScopeRead)

	rr := doRequestWithAuth(t, rt, http.MethodPut, "/api/admin/probe/..%2F..%2Fflow%2Fx", writer, nil)
	checkStatus(t, rr, http.StatusForbidden)
	if n := adminProbe.hits.Load(); n != 0 {
		t.Errorf("a write-scoped token reached an /api/admin/ handler %d time(s)", n)
	}
	// The real admin route, same shape.
	rr = doRequestWithAuth(t, rt, http.MethodPut, "/api/admin/users/..%2F..%2Fflow/role", writer, map[string]any{"role": "admin"})
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "missing scope") {
		t.Errorf("write-scoped PUT on the admin role route = %d %s, want 403 missing scope", rr.Code, rr.Body.String())
	}

	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/probe/..%2Fauth%2Fme", reader, nil)
	checkStatus(t, rr, http.StatusForbidden)
	if n := probe.hits.Load(); n != 0 {
		t.Errorf("a read-scoped token reached a POST handler %d time(s)", n)
	}

	// Within scope, escaped ids pass the gate.
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/probe/a%2Fb", reader, nil), http.StatusOK)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPut, "/api/probe/a%2Fb", writer, nil), http.StatusOK)
}

// rlsProbeBackend claims Postgres RLS support but fails each BeginRLS, so a mutation NOT exempted is refused 503 (rlsMiddleware fails closed) while a read runs on; the count tells the tests whether the RLS layer was skipped.
type rlsProbeBackend struct {
	storageif.StorageBackend
	begins atomic.Int32
}

func (b *rlsProbeBackend) BeginRLS(context.Context, string) (*sql.Tx, error) {
	b.begins.Add(1)
	return nil, errors.New("rls probe: no database")
}

// The flow read every library handler opens with: the scope is lazy, so the probe starts it where a Postgres backend's first statement would, surfacing the fail-closed refusal (ErrRLSUnavailable → 503).
func (b *rlsProbeBackend) LoadFlow(ctx context.Context, id string) (*storageif.FlowDocument, error) {
	if err := storagedb.EnsureRLS(ctx); err != nil {
		return nil, err
	}
	return b.StorageBackend.LoadFlow(ctx, id)
}

func (b *rlsProbeBackend) LoadFlowHeader(ctx context.Context, id string) (*storageif.FlowDocument, error) {
	if err := storagedb.EnsureRLS(ctx); err != nil {
		return nil, err
	}
	return b.StorageBackend.LoadFlowHeader(ctx, id)
}

// The RLS exemptions (streaming prefixes, outbound routes, public routes) apply to the route chi dispatches on — neither skipped for a route that only LOOKS exempt once decoded, nor imposed on an exempt route whose id carries an escaped "/".
func TestRLSMiddleware_DecidesOnTheDispatchedRoute(t *testing.T) {
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	backend := &rlsProbeBackend{StorageBackend: fs}
	rt := newTestRouter(backend, true)
	probe, _ := mountProbes(rt)
	h := fullChain(t, rt, nil)
	bearer := jwtBearer(t, rt, "u-1", "u1@example.com")

	// Cleans to /api/chat/x (a streaming prefix): the mutation must still
	// get its transaction — here, fail closed.
	rr := serve(h, http.MethodPost, "/api/probe/..%2Fchat%2Fx", bearer)
	if rr.Code != http.StatusServiceUnavailable || backend.begins.Load() != 1 {
		t.Errorf("POST through a RawPath that cleans to /api/chat/: %d, BeginRLS x%d; want 503 after one BeginRLS", rr.Code, backend.begins.Load())
	}
	if n := probe.hits.Load(); n != 0 {
		t.Errorf("the mutation ran %d time(s) without its RLS transaction", n)
	}
	// Cleans to /api/events: the read still asks for its transaction.
	backend.begins.Store(0)
	serve(h, http.MethodGet, "/api/probe/..%2Fevents", bearer)
	if n := backend.begins.Load(); n != 1 {
		t.Errorf("GET through a RawPath that cleans to /api/events: BeginRLS x%d, want 1", n)
	}

	// chi routes a RawPath uncleaned and r.Route mounts the versions collection with a trailing slash too, so the version save arrives as /{id}/versions/ — which reads as an exempt version fetch — whenever the id spells a character Go escapes and the SPA does not.
	for _, target := range []string{"/api/library/x!y/versions/", "/api/v1/library/x!y/versions/"} {
		backend.begins.Store(0)
		rr = serve(h, http.MethodPost, target, bearer)
		if rr.Code != http.StatusServiceUnavailable || backend.begins.Load() != 1 {
			t.Errorf("POST %s (the version save): %d, BeginRLS x%d; want 503 after one BeginRLS", target, rr.Code, backend.begins.Load())
		}
	}
	// The blob reads stay exempt under a RawPath.
	for _, target := range []string{"/api/library/x!y/content", "/api/library/a%2Fb/versions/1", "/api/library/x!y/versions/1/diff"} {
		backend.begins.Store(0)
		serve(h, http.MethodGet, target, bearer)
		if n := backend.begins.Load(); n != 0 {
			t.Errorf("GET %s: BeginRLS x%d, want exempt (0)", target, n)
		}
	}

	// An outbound route whose org id carries an escaped "/" stays exempt:
	// decoded, the id split into two segments and matched nothing.
	backend.begins.Store(0)
	rr = serve(h, http.MethodPost, "/api/orgs/o%2F1/providers/azure-foundry/test", bearer)
	if n := backend.begins.Load(); n != 0 || rr.Code == http.StatusServiceUnavailable {
		t.Errorf("POST org connection test with an escaped id: %d, BeginRLS x%d; want exempt (0)", rr.Code, n)
	}
	// Public routes never open one.
	backend.begins.Store(0)
	serve(h, http.MethodGet, "/api/system/features", "")
	if n := backend.begins.Load(); n != 0 {
		t.Errorf("public GET opened %d RLS transaction(s)", n)
	}
}

// Rate limiting runs outside the mux (BuildHandler layer 7) and must predict the dispatch path too: a RawPath that cleans to /assets/ is no static asset (no exemption), and one that cleans off the chat-bucket route still rides the chat bucket.
// Each case gets its own limiters, one bucket tight and the others roomy, so a 429 names the bucket that throttled it.
func TestRateLimit_ClassifiesTheDispatchedRoute(t *testing.T) {
	newChain := func(t *testing.T, generalBurst, chatBurst float64) (http.Handler, string) {
		rt := newJWTTestRouter(t)
		mountProbes(rt)
		h := fullChain(t, rt, func(c *config.Config) {
			c.Mode = config.ModeCloud
			c.Runtime.RateLimitGeneralRPS = 0.001
			c.Runtime.RateLimitGeneralBurst = generalBurst
			c.Runtime.RateLimitChatRPS = 0.001
			c.Runtime.RateLimitChatBurst = chatBurst
		})
		return h, jwtBearer(t, rt, "u-1", "u1@example.com")
	}

	t.Run("no assets exemption", func(t *testing.T) {
		h, bearer := newChain(t, 1, 100)
		target := "/api/probe/..%2F..%2Fassets%2Fapp.js"
		serve(h, http.MethodGet, target, bearer)
		if rr := serve(h, http.MethodGet, target, bearer); rr.Code != http.StatusTooManyRequests {
			t.Errorf("second GET %s = %d, want 429 (it is an API route, not a static asset)", target, rr.Code)
		}
		// Real assets keep their exemption, with the general bucket empty.
		for i := 0; i < 3; i++ {
			if rr := serve(h, http.MethodGet, "/assets/app.js", ""); rr.Code == http.StatusTooManyRequests {
				t.Fatalf("GET /assets/app.js #%d was throttled", i+1)
			}
		}
	})

	// The exemption hands the request to the router, and chi dispatches an /assets/ RawPath to the NotFound fallback, which serves the decoded, cleaned path: the SPA's index.html under /assets/ as chi sees it, or a file outside /assets/ as served.
	t.Run("assets exemption only for what is served from /assets/", func(t *testing.T) {
		dir := t.TempDir()
		for name, body := range map[string]string{"index.html": "<!doctype html>spa", "robots.txt": "robots", "assets/app.js": "app"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		newStaticChain := func(t *testing.T) http.Handler {
			rt := newJWTTestRouter(t)
			rt.staticDir = dir
			return fullChain(t, rt, func(c *config.Config) {
				c.Mode = config.ModeCloud
				c.Runtime.RateLimitGeneralRPS = 0.001
				c.Runtime.RateLimitGeneralBurst = 1
			})
		}

		for target, served := range map[string]string{
			"/assets/..%2Fsome%2Fpage":  "<!doctype html>spa",
			"/assets/%2e%2e/robots.txt": "robots",
		} {
			h := newStaticChain(t)
			if rr := serve(h, http.MethodGet, target, ""); rr.Code != http.StatusOK || rr.Body.String() != served {
				t.Fatalf("first GET %s = %d %q, want 200 %q", target, rr.Code, rr.Body.String(), served)
			}
			if rr := serve(h, http.MethodGet, target, ""); rr.Code != http.StatusTooManyRequests {
				t.Errorf("second GET %s = %d, want 429 (it serves %q, not a static asset)", target, rr.Code, served)
			}
		}

		// Escapes that stay inside /assets/ keep the exemption, with the
		// general bucket empty.
		h := newStaticChain(t)
		serve(h, http.MethodGet, "/some/page", "")
		if rr := serve(h, http.MethodGet, "/some/page", ""); rr.Code != http.StatusTooManyRequests {
			t.Fatalf("second GET /some/page = %d, want 429", rr.Code)
		}
		for _, target := range []string{"/assets/app.js", "/assets/app%2Ejs", "/assets/x%2F..%2Fapp.js"} {
			if rr := serve(h, http.MethodGet, target, ""); rr.Code != http.StatusOK || rr.Body.String() != "app" {
				t.Errorf("GET %s with the general bucket empty = %d %q, want 200 app", target, rr.Code, rr.Body.String())
			}
		}
	})

	t.Run("chat bucket", func(t *testing.T) {
		h, bearer := newChain(t, 100, 1)
		// POST /api/orgs/{id}/providers/azure-foundry/test with an id that,
		// decoded and cleaned, leaves /api/orgs entirely.
		target := "/api/orgs/x%2F..%2F..%2F..%2Ffoo/providers/azure-foundry/test"
		if g := rateLimitGroup(http.MethodPost, mustParseRequestURL(t, target)); g != rlGroupChat {
			t.Errorf("rateLimitGroup(POST %s) = %q, want chat", target, g)
		}
		serve(h, http.MethodPost, target, bearer)
		if rr := serve(h, http.MethodPost, target, bearer); rr.Code != http.StatusTooManyRequests {
			t.Errorf("second POST %s = %d, want 429 on the chat bucket", target, rr.Code)
		}
	})
}

// inboundRoutePath (rate limiting, before the Router normalizes the request) and routePath (inside the mux, after it) name the same path for every spelling that reaches a handler.
func TestInboundRoutePath_MatchesRoutePathInsideTheMux(t *testing.T) {
	rt := newJWTTestRouter(t)
	probe, _ := mountProbes(rt)
	bearer := jwtBearer(t, rt, "u-1", "u1@example.com")
	for _, target := range []string{
		"/api/probe/x",
		"/api/v1/probe/x",
		"/api/./probe/x",
		"/api//probe/x",
		"/api/probe/a%2Fb",
		"/api/v1/probe/a%2Fb",
		"/api/probe/a%20b", // the default encoding: no RawPath
		"/api/probe/a!b",   // not the default encoding (%21): RawPath set
		"/api/probe/%61b",  // a needless escape: RawPath set
	} {
		want := inboundRoutePath(mustParseRequestURL(t, target))
		before := probe.hits.Load()
		rr := doRequestWithAuth(t, rt, http.MethodGet, target, bearer, nil)
		if rr.Code != http.StatusOK || probe.hits.Load() != before+1 {
			t.Errorf("GET %s = %d, never reached the probe", target, rr.Code)
			continue
		}
		if got := probe.lastRoutePath(); got != want {
			t.Errorf("GET %s: routePath inside the mux = %q, inboundRoutePath = %q", target, got, want)
		}
	}
}

func mustParseRequestURL(t *testing.T, target string) *url.URL {
	t.Helper()
	u, err := url.ParseRequestURI(target)
	if err != nil {
		t.Fatalf("parse %q: %v", target, err)
	}
	return u
}
