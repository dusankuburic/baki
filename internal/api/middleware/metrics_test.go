package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeRoute(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		// Library: dynamic ID collapsed
		{"/api/library/abc123", "/api/library/:id"},
		{"/api/library/abc123/export", "/api/library/:id"},

		// Flow collaborators
		{"/api/flows/123/collaborators", "/api/flows/:id/collaborators"},
		{"/api/flows/123/collaborators/456", "/api/flows/:id/collaborators"},

		// Admin user role
		{"/api/admin/users/abc/role", "/api/admin/users/:id/role"},

		// Orgs
		{"/api/orgs/org-1", "/api/orgs/:id"},
		{"/api/orgs/org-1/members", "/api/orgs/:id/members"},
		{"/api/orgs/org-1/members/user-1", "/api/orgs/:id/members/:userId"},
		{"/api/orgs/org-1/members/user-1/role", "/api/orgs/:id/members/:userId"},

		// Swagger
		{"/swagger/index.html", "/swagger/*"},
		{"/swagger/", "/swagger/*"},

		// Health & metrics (exact matches)
		{"/healthz", "/healthz"},
		{"/readyz", "/readyz"},
		{"/api/health", "/api/health"},
		{"/metrics", "/metrics"},

		// Registered static API paths pass through verbatim.
		{"/api/auth/login", "/api/auth/login"},
		{"/api/flow/upload", "/api/flow/upload"},

		// Dynamic families that used to fall through to the verbatim /api/
		// case, minting one time series per identifier.
		{"/api/auth/sessions/sess-abc", "/api/auth/sessions/:id"},
		{"/api/auth/tokens/tok-abc", "/api/auth/tokens/:id"},
		{"/api/system/settings/org/org-9", "/api/system/settings/org/:id"},
		// The invite segment is the emailed single-use credential; it must
		// never reach a scraped, retained label.
		{"/api/invites/s3cret-invite-token/accept", "/api/invites/:token/accept"},

		// The Azure AI Foundry registry: per-resource and per-model routes
		// collapse, the static ones map to themselves, anything else under
		// it is no route.
		{"/api/providers/azure-foundry/connections", "/api/providers/azure-foundry/connections"},
		{"/api/providers/azure-foundry/connections/abcdefghij", "/api/providers/azure-foundry/connections/:id"},
		{"/api/providers/azure-foundry/connections/default", "/api/providers/azure-foundry/connections/:id"},
		{"/api/providers/azure-foundry/connections/abcdefghij/discover", "/api/providers/azure-foundry/connections/:id/discover"},
		{"/api/providers/azure-foundry/models", "/api/providers/azure-foundry/models"},
		{"/api/providers/azure-foundry/models/abcdefghij", "/api/providers/azure-foundry/models/:id"},
		{"/api/providers/azure-foundry/test", "/api/providers/azure-foundry/test"},
		{"/api/providers/azure-foundry/config", "/api/providers/azure-foundry/config"},
		{"/api/providers/azure-foundry/connections/abcdefghij/other", routeOther},
		{"/api/providers/azure-foundry/models/a/b", routeOther},
		{"/api/providers/azure-foundry/connections/", routeOther},
		{"/api/providers/azure-foundry/whatever", routeOther},
		// An organisation's: the same fixed set under /api/orgs/:id.
		{"/api/orgs/org-1/providers/azure-foundry/connections", "/api/orgs/:id/providers/azure-foundry/connections"},
		{"/api/orgs/org-1/providers/azure-foundry/connections/abcdefghij", "/api/orgs/:id/providers/azure-foundry/connections/:id"},
		{"/api/orgs/org-1/providers/azure-foundry/connections/abcdefghij/discover", "/api/orgs/:id/providers/azure-foundry/connections/:id/discover"},
		{"/api/orgs/org-1/providers/azure-foundry/models/abcdefghij", "/api/orgs/:id/providers/azure-foundry/models/:id"},
		{"/api/orgs/org-1/providers/azure-foundry/test", "/api/orgs/:id/providers/azure-foundry/test"},
		{"/api/orgs/org-1/providers/azure-foundry/config", "/api/orgs/:id/providers/azure-foundry/config"},
		{"/api/orgs/org-1/providers/azure-foundry/models/a/b", "/api/orgs/:id"},
		{"/api/orgs/org-1/x/providers/azure-foundry/models", "/api/orgs/:id"},

		// Root
		{"/", "/"},
		{"", "/"},

		// Static assets
		{"/assets/main.js", "/static/*"},
		{"/favicon.ico", "/static/*"},

		// The WebSocket upgrade is its own route, not an asset.
		{"/ws", "/ws"},
	}

	known := map[string]struct{}{
		"/api/auth/login":  {},
		"/api/flow/upload": {},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := normalizeRoute(tt.path, known); got != tt.expected {
				t.Errorf("normalizeRoute(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

// TestNormalizeRoute_WebSocketIsNotAStaticAsset: /ws is not under /api/ and
// would otherwise fall through to /static/*. The Metrics layer observes the
// handler's whole lifetime, so every WebSocket's open-to-close duration would
// pollute the asset histogram. It is labelled as itself — and only the exact path: the
// known set is consulted for /api/ routes only, so this must not depend on it.
func TestNormalizeRoute_WebSocketIsNotAStaticAsset(t *testing.T) {
	for _, known := range []map[string]struct{}{nil, {"/ws": {}}} {
		if got := normalizeRoute("/ws", known); got != "/ws" {
			t.Errorf("normalizeRoute(/ws, known=%v) = %q, want %q", known, got, "/ws")
		}
	}
	if got := normalizeRoute("/ws/anything", nil); got != "/static/*" {
		t.Errorf("normalizeRoute(/ws/anything) = %q, want the static fallback (no such route)", got)
	}
}

// Regression: the metrics-cardinality bomb. Metrics runs outside the mux and before auth, so every
// request reaches it; the old `/api/` fallthrough minted one Prometheus series per request until OOM.
// Unregistered paths must collapse to a single bounded label.
func TestNormalizeRoute_UnknownAPIPathsAreBounded(t *testing.T) {
	known := map[string]struct{}{"/api/auth/login": {}}

	seen := map[string]struct{}{}
	for _, p := range []string{
		"/api/aaaaaaaa", "/api/bbbbbbbb", "/api/x/y/z",
		"/api/00000000-0000-0000-0000-000000000000",
		"/api/../api/nope", // already path.Clean'd by the router in practice
	} {
		got := normalizeRoute(p, known)
		if got != routeOther {
			t.Errorf("normalizeRoute(%q) = %q, want %q", p, got, routeOther)
		}
		seen[got] = struct{}{}
	}
	if len(seen) != 1 {
		t.Errorf("unknown paths produced %d distinct labels, want 1", len(seen))
	}

	// The Azure AI Foundry registry's ids never reach a label: any number of
	// resources and models, personal or an organisation's, collapse to a
	// fixed set of patterns.
	azure := map[string]struct{}{}
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("id%08d", i)
		for _, p := range []string{
			"/api/providers/azure-foundry/connections/" + id,
			"/api/providers/azure-foundry/connections/" + id + "/discover",
			"/api/providers/azure-foundry/models/" + id,
			"/api/providers/azure-foundry/" + id,
			"/api/orgs/" + id + "/providers/azure-foundry/connections/" + id,
			"/api/orgs/" + id + "/providers/azure-foundry/connections/" + id + "/discover",
			"/api/orgs/" + id + "/providers/azure-foundry/models/" + id,
			"/api/orgs/" + id + "/providers/azure-foundry/" + id,
		} {
			azure[normalizeRoute(p, known)] = struct{}{}
		}
	}
	if len(azure) != 8 {
		t.Errorf("the registry's paths produced %d labels, want 8: %v", len(azure), azure)
	}

	// A nil set is fail-closed: unrecognized rather than unbounded.
	if got := normalizeRoute("/api/auth/login", nil); got != routeOther {
		t.Errorf("nil known set: got %q, want %q (fail-closed)", got, routeOther)
	}
}

func TestNormalizeMethod(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE"} {
		if got := normalizeMethod(m); got != m {
			t.Errorf("normalizeMethod(%q) = %q, want it verbatim", m, got)
		}
	}
	for _, m := range []string{"FOO", "get", "PROPFIND", "", strings.Repeat("X", 4096)} {
		if got := normalizeMethod(m); got != methodOther {
			t.Errorf("normalizeMethod(%q) = %q, want %q", m, got, methodOther)
		}
	}
}

// TestMetrics_CustomMethodsAreBounded is the regression test for the method
// half of the cardinality bomb: the route label was bounded, but r.Method was
// labelled verbatim, so `LEAK1 /`, `LEAK2 /`, ... each minted a permanent
// series. Drive the real middleware and read the real exposition.
func TestMetrics_CustomMethodsAreBounded(t *testing.T) {
	h := Metrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}), nil)
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.Method = fmt.Sprintf("CARDTEST%d", i)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	rr := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	if strings.Contains(body, `method="CARDTEST`) {
		t.Error("a custom request method reached the method label verbatim")
	}
	if !strings.Contains(body, `method="OTHER",route="/",status="404"`) {
		t.Error(`custom methods were not recorded under method="OTHER"`)
	}
}

// TestMetrics_LabelsTheCanonicalRoute: the route label comes from the path the
// router dispatches on, not the raw one. A request turned away before the
// router has rewritten its path — a 429 from the rate limiter — was labelled
// on the raw string: /api/v1/library/x as /api/other, the /api/v1 alias of a
// registered static route likewise, and a dot segment could pull a path into
// the wrong family altogether.
func TestMetrics_LabelsTheCanonicalRoute(t *testing.T) {
	known := map[string]struct{}{"/api/auth/login": {}}
	serve := func(path string, status int, rewrite bool) {
		Metrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rewrite {
				r.URL.Path = canonicalPath(r.URL.Path)
			}
			w.WriteHeader(status)
		}), known).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	// Each case answers with a status of its own, so its series cannot be
	// mistaken for another case's.
	cases := []struct {
		path    string
		status  int
		rewrite bool // the handler rewrites r.URL.Path in place, as the router does
		want    string
	}{
		// Refused before the router: r.URL.Path is still the raw path.
		{"/api/v1/library/canon-abc", http.StatusTooManyRequests, false, "/api/library/:id"},
		{"/api/v1/auth/login", http.StatusServiceUnavailable, false, "/api/auth/login"},
		{"/api/library/../v1/orgs/canon-org/members", http.StatusForbidden, false, "/api/orgs/:id/members"},
		// Reached the router, which rewrote the path before the handler
		// returned. The label is still one canonicalization of the path as
		// received: /api/v1/v1/library/x dispatches as /api/v1/library/x,
		// which no route matches — not as /api/library/:id.
		{"/api/v1/v1/library/canon-x", http.StatusTeapot, true, routeOther},
	}
	for _, c := range cases {
		serve(c.path, c.status, c.rewrite)
	}
	// Still bounded: an unregistered /api/v1 path is one label, not one each.
	for i := 0; i < 20; i++ {
		serve(fmt.Sprintf("/api/v1/canon-%d", i), http.StatusGone, false)
	}

	rr := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	for _, c := range cases {
		if want := fmt.Sprintf(`method="GET",route=%q,status="%d"`, c.want, c.status); !strings.Contains(body, want) {
			t.Errorf("%s: no series %s", c.path, want)
		}
		for _, wrong := range []string{routeOther, "/api/library/:id"} {
			if wrong == c.want {
				continue
			}
			if series := fmt.Sprintf(`route=%q,status="%d"`, wrong, c.status); strings.Contains(body, series) {
				t.Errorf("%s: labelled %s", c.path, series)
			}
		}
	}
	if !strings.Contains(body, `method="GET",route="/api/other",status="410"`) {
		t.Error("unregistered /api/v1 paths were not recorded under routeOther")
	}
	if strings.Contains(body, "canon-") {
		t.Error("a raw path segment reached the route label")
	}
}
