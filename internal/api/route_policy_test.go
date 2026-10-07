package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/reqinfo"
	"pad-analyzer/internal/storage/filesystem"
	"pad-analyzer/internal/testutil"
)

// mountTestRoutes registers test routes on rt after construction, through a route.Builder as NewRouter registers
// its own, then rebuilds the policy table and the resolver so the routes resolve to their policies.
func mountTestRoutes(t testing.TB, rt *Router, register func(rb *route.Builder)) {
	t.Helper()
	rb := route.NewBuilder(rt.mux)
	register(rb)
	tbl, err := rb.Build()
	if err != nil {
		t.Fatalf("route table: %v", err)
	}
	rt.routes = tbl
	if err := rt.finishRoutes(rb); err != nil {
		t.Fatalf("finishRoutes: %v", err)
	}
}

// routePolicyFromEnv sets the route-policy mode of test routers from BAKI_TEST_ROUTE_POLICY, so the whole API suite
// runs under any mode: BAKI_TEST_ROUTE_POLICY=declarative go test ./internal/api/...
func routePolicyFromEnv(cfg *config.Config) {
	m := config.RoutePolicyMode(os.Getenv("BAKI_TEST_ROUTE_POLICY"))
	switch m {
	case "":
	case config.RoutePolicyLegacy, config.RoutePolicyShadow, config.RoutePolicyDeclarative:
		cfg.Server.RoutePolicy = m
	default:
		panic(fmt.Sprintf("BAKI_TEST_ROUTE_POLICY=%q: want legacy, shadow or declarative", m))
	}
}

var allRoutePolicyModes = []config.RoutePolicyMode{config.RoutePolicyLegacy, config.RoutePolicyShadow, config.RoutePolicyDeclarative}

// newModeTestRouter is a JWT test router on a filesystem backend in route-policy mode.
func newModeTestRouter(t *testing.T, mode config.RoutePolicyMode) *Router {
	t.Helper()
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newTestRouterSSO(fs, true, nil, nil, func(c *config.Config) { c.Server.RoutePolicy = mode })
}

// withResolvedPolicy returns r carrying p as the policy an outer layer resolved: the way to make the resolver
// disagree with chi's dispatch.
func withResolvedPolicy(r *http.Request, p *route.Policy) *http.Request {
	return r.WithContext(reqinfo.With(r.Context(), &reqinfo.Info{Policy: p}))
}

// countingHandler answers 204 and counts the requests that reach it.
type countingHandler struct{ hits atomic.Int32 }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.hits.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

// Router.ServeHTTP resolves the policy itself when no outer layer did, before its path.Clean; resolving after the
// clean must give the same policy for every request the golden covers, or direct callers would see another one.
func TestRouter_ResolvesTheSamePolicyBeforeAndAfterPathClean(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), true)
	for _, req := range policyRequests(t, rt) {
		r := httptest.NewRequest(req.method, req.target, nil)
		before := rt.resolver.Resolve(req.method, r.URL)
		cleaned := *r.URL
		cleaned.Path = path.Clean(cleaned.Path)
		if after := rt.resolver.Resolve(req.method, &cleaned); after != before {
			t.Errorf("%s %s: %s %q before path.Clean, %s %q after", req.method, req.target, before.Kind, before.Label, after.Kind, after.Label)
		}
	}

	// Through ServeHTTP: the policy it installs is the outer layer's answer for the request as received.
	info := &reqinfo.Info{}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/library/./x", nil)
	want := rt.resolver.Resolve(r.Method, r.URL)
	rt.ServeHTTP(httptest.NewRecorder(), r.WithContext(reqinfo.With(r.Context(), info)))
	if info.Policy != want || want.Pattern != "/api/library/{id}" {
		t.Errorf("ServeHTTP installed %+v, want the policy of /api/library/{id} (%p)", info.Policy, want)
	}
}

// The outer layer resolves into the reqinfo.Info an outer RequestInfo layer created, or creates one, and never
// touches the URL the router will clean and de-alias itself.
func TestResolvePolicy_RecordsThePolicyAndLeavesTheURL(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), true)
	var seen *reqinfo.Info
	var seenURL string
	h := rt.resolvePolicy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = reqinfo.From(r.Context())
		seenURL = r.URL.String()
	}))

	const target = "/api/v1/library/a%2Fb?x=1"
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	if seen == nil || seen.Policy == nil || seen.Policy.Pattern != "/api/library/{id}" {
		t.Fatalf("no Info or wrong policy: %+v", seen)
	}
	if seenURL != target {
		t.Errorf("the URL reached the next layer as %q, want %q untouched", seenURL, target)
	}

	outer := &reqinfo.Info{ID: "req-1"}
	r := httptest.NewRequest(http.MethodGet, "/api/library", nil)
	h.ServeHTTP(httptest.NewRecorder(), r.WithContext(reqinfo.With(r.Context(), outer)))
	if seen != outer || outer.Policy == nil || outer.Policy.Pattern != "/api/library" {
		t.Errorf("the outer layer's Info was not reused: %+v", seen)
	}
}

// finishRoutes refuses a route that its own sample path does not reach: it would run under another route's policy.
func TestFinishRoutes_SelfTestRefusesAnUnreachableRoute(t *testing.T) {
	mux := chi.NewRouter()
	rt := &Router{mux: mux}
	rb := route.NewBuilder(mux)
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	rb.Get("/api/t/{id}", ok)
	rb.Get("/api/t/x", ok) // the sample path of /api/t/{id}
	rb.Get("/api/u/{id}", ok)
	rb.Post("/api/v", ok, route.RateLimit("chatt")) // no limiter has that bucket
	tbl, err := rb.Build()
	if err != nil {
		t.Fatal(err)
	}
	rt.routes = tbl
	err = rt.finishRoutes(rb)
	if err == nil || !strings.Contains(err.Error(), "GET /api/t/{id}") || !strings.Contains(err.Error(), `POST /api/v: unknown rate-limit bucket "chatt"`) ||
		strings.Contains(err.Error(), "/api/u/{id}") {
		t.Fatalf("finishRoutes = %v, want errors naming GET /api/t/{id} and the bucket of POST /api/v only", err)
	}
	if err := (&Router{mux: chi.NewRouter()}).finishRoutes(route.NewBuilder(chi.NewRouter())); err == nil {
		t.Error("finishRoutes without a table must fail")
	}
}

// The endpoint guard: when chi dispatches to a route other than the one whose policy the middleware applied, the
// mismatch is counted and logged; declarative mode refuses the request before the handler, the other modes let it
// through (the legacy tables decided).
func TestGuard_EndpointMismatch(t *testing.T) {
	for _, mode := range allRoutePolicyModes {
		t.Run(string(mode), func(t *testing.T) {
			rt := newModeTestRouter(t, mode)
			probe := &countingHandler{}
			label := "/api/guard-probe-" + string(mode)
			mountTestRoutes(t, rt, func(rb *route.Builder) { rb.Get(label, probe.ServeHTTP) })
			bearer := jwtBearer(t, rt, "u-1", "u1@example.com")

			// Resolved as itself: no mismatch.
			checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, label, bearer, nil), http.StatusNoContent)

			elsewhere := rt.routes.Lookup(http.MethodGet, "/api/library")
			r := withResolvedPolicy(httptest.NewRequest(http.MethodGet, label, nil), elsewhere)
			r.Header.Set("Authorization", bearer)
			rr := httptest.NewRecorder()
			rt.ServeHTTP(rr, r)

			want, wantHits := http.StatusNoContent, int32(2)
			if mode == config.RoutePolicyDeclarative {
				want, wantHits = http.StatusInternalServerError, 1
			}
			if rr.Code != want || probe.hits.Load() != wantHits {
				t.Errorf("mismatched request = %d with %d handler run(s), want %d with %d", rr.Code, probe.hits.Load(), want, wantHits)
			}
			if got := metrics.RouteGuardMismatchCount(label); got != 1 {
				t.Errorf("guard mismatches for %s = %v, want 1", label, got)
			}
		})
	}
}

// A route declaring Needs answers 503 SERVICE_UNAVAILABLE in every mode when the deployment lacks them, before its
// handler runs, and runs once it has them.
func TestGuard_NeedsAnswersServiceUnavailable(t *testing.T) {
	for _, mode := range allRoutePolicyModes {
		t.Run(string(mode), func(t *testing.T) {
			rt := newModeTestRouter(t, mode)
			probe := &countingHandler{}
			mountTestRoutes(t, rt, func(rb *route.Builder) {
				rb.Get("/api/needs-probe", probe.ServeHTTP, route.Needs(capability.ServerStore, capability.Accounts))
			})
			bearer := jwtBearer(t, rt, "u-1", "u1@example.com")

			rt.caps = capability.NewSet(capability.Desktop, capability.Accounts)
			rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/needs-probe", bearer, nil)
			checkStatus(t, rr, http.StatusServiceUnavailable)
			if !strings.Contains(rr.Body.String(), `"code":"SERVICE_UNAVAILABLE"`) || probe.hits.Load() != 0 {
				t.Errorf("without the capability: body %s, handler ran %d time(s)", rr.Body.String(), probe.hits.Load())
			}

			rt.caps = capability.NewSet(capability.Cloud, capability.ServerStore, capability.Accounts)
			checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/needs-probe", bearer, nil), http.StatusNoContent)
		})
	}
}

// The 404 and 405 fallbacks are guarded too: they must have been resolved as no-route and 405 policies.
func TestGuard_FallbacksCheckTheResolvedKind(t *testing.T) {
	for _, mode := range allRoutePolicyModes {
		t.Run(string(mode), func(t *testing.T) {
			rt := newModeTestRouter(t, mode)
			bearer := jwtBearer(t, rt, "u-1", "u1@example.com")
			library := rt.routes.Lookup(http.MethodGet, "/api/library")
			serveAs := func(method, target string, p *route.Policy) int {
				r := withResolvedPolicy(httptest.NewRequest(method, target, nil), p)
				r.Header.Set("Authorization", bearer)
				rr := httptest.NewRecorder()
				rt.ServeHTTP(rr, r)
				return rr.Code
			}
			notFound := metrics.RouteGuardMismatchCount(dispatchedNotFound)
			notAllowed := metrics.RouteGuardMismatchCount(dispatchedMethodNotAllowed)

			want404, want405 := http.StatusNotFound, http.StatusMethodNotAllowed
			if mode == config.RoutePolicyDeclarative {
				want404, want405 = http.StatusInternalServerError, http.StatusInternalServerError
			}
			if got := serveAs(http.MethodGet, "/api/no-such-route", library); got != want404 {
				t.Errorf("404 resolved as a route = %d, want %d", got, want404)
			}
			if got := serveAs(http.MethodPut, "/api/library", library); got != want405 {
				t.Errorf("405 resolved as a route = %d, want %d", got, want405)
			}
			if d := metrics.RouteGuardMismatchCount(dispatchedNotFound) - notFound; d != 1 {
				t.Errorf("404 guard mismatches +%v, want +1", d)
			}
			if d := metrics.RouteGuardMismatchCount(dispatchedMethodNotAllowed) - notAllowed; d != 1 {
				t.Errorf("405 guard mismatches +%v, want +1", d)
			}
		})
	}
}

// The fallbacks answer exactly as chi did before they were wrapped (recorded from the unwrapped router): 405 with
// an Allow header per registered method and an empty body — none for a method chi does not route — and chi's
// plain-text 404, with the router's own headers on both, in every mode.
func TestFallbacks_AnswerAsChiDid(t *testing.T) {
	cases := []struct {
		method, target string
		authed         bool
		status         int
		allow          string // sorted, comma-joined
		body           string
	}{
		{http.MethodPut, "/api/library", true, 405, "GET,POST", ""},
		{http.MethodHead, "/api/admin/audit", true, 405, "GET", ""},
		{http.MethodGet, "/api/auth/login", false, 405, "POST", ""},
		{http.MethodGet, "/api/auth/login", true, 405, "POST", ""},
		{"FOO", "/api/library", true, 405, "", ""},
		{"FOO", "/nothing", false, 405, "", ""},
		{http.MethodDelete, "/healthz", false, 405, "GET", ""},
		{http.MethodPost, "/ws", false, 405, "GET", ""},
		{http.MethodPatch, "/api/library/x", true, 405, "DELETE,GET,PUT", ""},
		{http.MethodGet, "/api/chat/stream", true, 405, "POST", ""},
		{http.MethodPost, "/swagger/x", false, 405, "GET", ""},
		{http.MethodConnect, "/api/library", true, 405, "GET,POST", ""},
		{http.MethodTrace, "/api/library", true, 405, "GET,POST", ""},
		{http.MethodGet, "/api/does-not-exist", true, 404, "", "404 page not found\n"},
		{http.MethodGet, "/api/library/x/nope", true, 404, "", "404 page not found\n"},
		{http.MethodGet, "/nothing", false, 404, "", "404 page not found\n"},
	}
	for _, mode := range allRoutePolicyModes {
		t.Run(string(mode), func(t *testing.T) {
			rt := newModeTestRouter(t, mode)
			bearer := jwtBearer(t, rt, "u-1", "u1@example.com")
			for _, c := range cases {
				r := httptest.NewRequest(c.method, c.target, nil)
				if c.authed {
					r.Header.Set("Authorization", bearer)
				}
				rr := httptest.NewRecorder()
				rt.ServeHTTP(rr, r)
				h := rr.Result().Header
				allow := append([]string(nil), h.Values("Allow")...)
				sort.Strings(allow)
				if rr.Code != c.status || strings.Join(allow, ",") != c.allow || rr.Body.String() != c.body {
					t.Errorf("%s %s = %d Allow=%v body=%q, want %d Allow=%s body=%q", c.method, c.target, rr.Code, allow, rr.Body.String(), c.status, c.allow, c.body)
				}
				wantType := ""
				if c.status == http.StatusNotFound {
					wantType = "text/plain; charset=utf-8"
				}
				if got := h.Get("Content-Type"); got != wantType {
					t.Errorf("%s %s: Content-Type %q, want %q", c.method, c.target, got, wantType)
				}
				if h.Get("X-Frame-Options") != "DENY" || h.Get("Vary") != "Origin" {
					t.Errorf("%s %s lost the router's headers: %v", c.method, c.target, h)
				}
			}
		})
	}
}

// Shadow mode: the legacy tables decide, and each route whose declared policy disagrees with them is counted per
// decision. Declarative mode applies the declared policy instead.
func TestRoutePolicyModes_DecideAndCountDisagreements(t *testing.T) {
	type outcome struct {
		anon, readToken int  // status
		chatThrottled   bool // the second chat call answered 429
		notxBegins      int32
	}
	for _, mode := range allRoutePolicyModes {
		t.Run(string(mode), func(t *testing.T) {
			fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			backend := &rlsProbeBackend{StorageBackend: fs}
			rt := newTestRouterSSO(backend, true, nil, nil, func(c *config.Config) { c.Server.RoutePolicy = mode })
			prefix := "/api/shadow-" + string(mode)
			public, admin, chat, notx := &countingHandler{}, &countingHandler{}, &countingHandler{}, &countingHandler{}
			mountTestRoutes(t, rt, func(rb *route.Builder) {
				rb.Get(prefix+"/public", public.ServeHTTP, route.Public())                    // legacy: authenticated
				rb.Get(prefix+"/admin", admin.ServeHTTP, route.Scopes(auth.ScopeAdmin))       // legacy: read
				rb.Post(prefix+"/chat", chat.ServeHTTP, route.RateLimit(route.RateChat))      // legacy: general
				rb.Post(prefix+"/notx/{id}", notx.ServeHTTP, route.NoTx(), route.Streaming()) // legacy: tx, default deadline
			})
			h := fullChain(t, rt, func(c *config.Config) {
				c.Mode = config.ModeCloud
				c.Runtime.RateLimitGeneralRPS, c.Runtime.RateLimitGeneralBurst = 1000, 1000
				c.Runtime.RateLimitChatRPS, c.Runtime.RateLimitChatBurst = 0.001, 1
				c.Runtime.RateLimitPerUserRPS, c.Runtime.RateLimitPerUserBurst = 1000, 1000
			})
			bearer := jwtBearer(t, rt, "u-1", "u1@example.com")
			reader := "Bearer " + mintToken(t, rt, "ci-reader", "ci-reader@example.com", auth.RoleAdmin, nil, auth.ScopeRead)

			var got outcome
			got.anon = serve(h, http.MethodGet, prefix+"/public", "").Code
			got.readToken = serve(h, http.MethodGet, prefix+"/admin", reader).Code
			serve(h, http.MethodPost, prefix+"/chat", bearer)
			got.chatThrottled = serve(h, http.MethodPost, prefix+"/chat", bearer).Code == http.StatusTooManyRequests
			backend.begins.Store(0)
			serve(h, http.MethodPost, prefix+"/notx/x", bearer)
			got.notxBegins = backend.begins.Load()

			want := outcome{anon: 401, readToken: 204, chatThrottled: false, notxBegins: 1}
			if mode == config.RoutePolicyDeclarative {
				want = outcome{anon: 204, readToken: 403, chatThrottled: true, notxBegins: 0}
			}
			if got != want {
				t.Errorf("outcomes %+v, want %+v (anonymous public, read token on admin, second chat call, BeginRLS on NoTx)", got, want)
			}

			// Each probe disagrees on its one decision only (the notx probe on two); the others agree.
			for _, c := range []struct {
				route, decision string
				shadow          float64
			}{
				{"/public", middleware.DecisionAuth, 1},
				{"/admin", middleware.DecisionScope, 1},
				{"/chat", middleware.DecisionRateLimit, 2},
				{"/notx/{id}", middleware.DecisionTx, 1},
				{"/notx/{id}", middleware.DecisionDeadline, 1},
				{"/public", middleware.DecisionRateLimit, 0},
				{"/admin", middleware.DecisionAuth, 0},
				{"/admin", middleware.DecisionTx, 0},
				{"/chat", middleware.DecisionTx, 0},
				{"/notx/{id}", middleware.DecisionRateLimit, 0},
			} {
				want := c.shadow
				if mode != config.RoutePolicyShadow {
					want = 0
				}
				if got := metrics.RoutePolicyMismatchCount(prefix+c.route, c.decision); got != want {
					t.Errorf("%s mismatches on %s = %v, want %v", c.decision, prefix+c.route, got, want)
				}
			}
		})
	}
}

// Every route's metrics label is its pattern, and nothing but a route's pattern or one of the fixed no-route labels
// ever is: an unknown /api/ path is one label however many there are, and an invite credential never reaches it.
func TestRouteLabels_ArePatterns(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), true)
	if n := len(rt.routes.Policies()); n < 150 {
		t.Fatalf("the route table has %d routes; the walk is probably broken", n)
	}
	for _, p := range rt.routes.Policies() {
		if p.Label != p.Pattern || p.Pattern == "" {
			t.Errorf("%s %s: label %q", p.Method, p.Pattern, p.Label)
		}
	}
	resolve := func(method, target string) string {
		return rt.resolver.Resolve(method, httptest.NewRequest(method, target, nil).URL).Label
	}
	for target, want := range map[string]string{
		"/api/v1/library/abc":                              "/api/library/{id}",
		"/api/library/a%2Fb/content":                       "/api/library/{id}/content",
		"/api/orgs/o-1/members/u-1":                        "/api/orgs/{id}/members/{userId}",
		"/api/invites/s3cret-invite/accept":                "/api/invites/{token}/accept",
		"/api/auth/sessions/sess-abc":                      "/api/auth/sessions/{id}",
		"/api/providers/azure-foundry/test":                "/api/providers/azure-foundry/test",
		"/api/orgs/o/providers/azure-foundry/models/m%2Fx": "/api/orgs/{id}/providers/azure-foundry/models/{modelId}",
	} {
		method := http.MethodGet
		if strings.HasSuffix(target, "/accept") || strings.HasSuffix(target, "/test") {
			method = http.MethodPost
		}
		if strings.HasPrefix(target, "/api/auth/sessions/") {
			method = http.MethodDelete
		}
		if got := resolve(method, target); got != want {
			t.Errorf("%s %s: label %q, want %q", method, target, got, want)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		seen[resolve(http.MethodGet, fmt.Sprintf("/api/unknown-%d/x", i))] = true
		seen[resolve(http.MethodGet, fmt.Sprintf("/assets/chunk-%d.js", i))] = true
		seen[resolve(http.MethodGet, fmt.Sprintf("/page-%d", i))] = true
	}
	if len(seen) != 2 || !seen["/api/other"] || !seen["/static/*"] {
		t.Errorf("no-route labels %v, want just /api/other and /static/*", seen)
	}
}
