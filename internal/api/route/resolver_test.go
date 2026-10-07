package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/auth"
)

// DispatchPath names the path chi routes on: the router strips the /api/v1 alias from RawPath too (it cleans only
// Path), and never cleans RawPath, whose escaped "/" chi keeps inside one segment.
func TestDispatchPath(t *testing.T) {
	for target, want := range map[string]string{
		"/api/v1/orgs/o%2Dx/knowledge/upload": "/api/orgs/o%2Dx/knowledge/upload",
		"/api/orgs/o%2Dx/knowledge/upload":    "/api/orgs/o%2Dx/knowledge/upload",
		"/api/v1/probe/a%2F..%2Fb":            "/api/probe/a%2F..%2Fb", // RawPath: never cleaned
		"/api/v1foo/a%2Fb":                    "/api/v1foo/a%2Fb",      // not the alias
		"/api/v1/./probe//x":                  "/api/probe/x",          // no RawPath: cleaned, then de-aliased
		"/api/v1":                             "/api",
		"/api/v1/v1/library":                  "/api/v1/library", // one strip, as the router does
		"/api/library/../../ws":               "/ws",
	} {
		if got := DispatchPath(httptest.NewRequest(http.MethodGet, target, nil).URL); got != want {
			t.Errorf("DispatchPath(%s) = %q, want %q", target, got, want)
		}
	}
	if got := DispatchPath(&url.URL{}); got != "." {
		t.Errorf("DispatchPath(empty) = %q, want %q (path.Clean)", got, ".")
	}
	if got := CanonicalPath("/api/v1/x/../y/"); got != "/api/y" {
		t.Errorf("CanonicalPath = %q", got)
	}
	for in, want := range map[string]string{"/api/v1/x": "/api/x", "/api/v1": "/api", "/api/v1foo": "/api/v1foo", "/x/api/v1/y": "/x/api/v1/y"} {
		if got := StripVersionAlias(in); got != want {
			t.Errorf("StripVersionAlias(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestSamplePath(t *testing.T) {
	for pattern, want := range map[string]string{
		"/api/library":                         "/api/library",
		"/api/library/{id}/versions/{vn}":      "/api/library/x/versions/x",
		"/swagger/*":                           "/swagger/x",
		"/api/x/{id:[0-9]{3}}/y":               "/api/x/x/y",
		"/":                                    "/",
		"/api/orgs/{id}/members/{userId}/role": "/api/orgs/x/members/x/role",
	} {
		if got := SamplePath(pattern); got != want {
			t.Errorf("SamplePath(%s) = %q, want %q", pattern, got, want)
		}
	}
}

func newSampleResolver(t *testing.T) (*chi.Mux, *Table, *Resolver) {
	t.Helper()
	mux, tbl := buildSample(t, ok)
	return mux, tbl, NewResolver(mux, tbl)
}

func resolve(r *Resolver, method, target string) *Policy {
	return r.Resolve(method, httptest.NewRequest(method, target, nil).URL)
}

// A request for a route resolves to that route's policy, whatever its spelling: the /api/v1 alias, dot segments
// and duplicate slashes in Path, an escaped "/" inside a parameter, a subrouter root with a trailing slash. A
// RawPath is never cleaned, by chi or the resolver: /api//library/a%2Fb matches no route.
func TestResolve_Endpoints(t *testing.T) {
	_, tbl, r := newSampleResolver(t)
	for _, c := range []struct{ method, target, pattern string }{
		{http.MethodGet, "/api/library", "/api/library"},
		{http.MethodGet, "/api/library/", "/api/library"},
		{http.MethodPost, "/api/v1/library", "/api/library"},
		{http.MethodGet, "/api/./library/x", "/api/library/{id}"},
		{http.MethodGet, "/api//library/x", "/api/library/{id}"},
		{http.MethodGet, "/api/v1/library/a%2Fb/content", "/api/library/{id}/content"},
		{http.MethodGet, "/api/admin/system/health", "/api/admin/system/health"},
		{http.MethodGet, "/swagger/index.html", "/swagger/*"},
		{http.MethodTrace, "/metrics", "/metrics"},
	} {
		got := resolve(r, c.method, c.target)
		if want := tbl.Lookup(c.method, c.pattern); got == nil || got != want {
			t.Errorf("%s %s resolved to %+v, want the policy of %s", c.method, c.target, got, c.pattern)
		}
	}
	if p := resolve(r, http.MethodGet, "/api//library/a%2Fb"); p.Kind != KindNoRoute {
		t.Errorf("GET /api//library/a%%2Fb resolved to %+v, want no route", p)
	}
	// The resolver predicts chi: a cleaned Path resolves the same.
	u := httptest.NewRequest(http.MethodGet, "/api/v1/library/./x/", nil).URL
	before := r.Resolve(http.MethodGet, u)
	u.Path = path.Clean(u.Path)
	if after := r.Resolve(http.MethodGet, u); after != before {
		t.Errorf("resolution changed with path.Clean: %+v vs %+v", before, after)
	}
}

// A path whose method has no route resolves to a 405 policy built from the routes registered on it.
func TestResolve_MethodNotAllowed(t *testing.T) {
	_, _, r := newSampleResolver(t)

	put := resolve(r, http.MethodPut, "/api/library")
	if put.Kind != KindMethodNotAllowed || put.Pattern != "/api/library" || put.Label != "/api/library" ||
		strings.Join(put.Allowed, ",") != "GET,POST" || put.Access != AuthRequired || put.Tx != TxNone ||
		put.RateLimit != RateGeneral || !put.AllowsToken(nil) || put.Method != "" {
		t.Errorf("PUT /api/library: %+v", put)
	}
	if again := resolve(r, http.MethodPut, "/api/v1/library/"); again != put {
		t.Error("the same 405 resolved to a second policy: 405 policies must be memoized for pointer comparison")
	}
	if patch := resolve(r, http.MethodPatch, "/api/library"); patch != put {
		t.Error("another method with the same probe result must share the policy")
	}

	// Public only when every route on the path is: GET /api/auth/login stays a public 405, charged to the auth
	// bucket like the login it probes.
	login := resolve(r, http.MethodGet, "/api/auth/login")
	if login.Kind != KindMethodNotAllowed || login.Access != AuthNone || login.RateLimit != RateAuth || strings.Join(login.Allowed, ",") != "POST" {
		t.Errorf("GET /api/auth/login: %+v", login)
	}
	// A CORS preflight stays on general, unless the path's bucket is auth.
	if p := resolve(r, http.MethodOptions, "/api/auth/login"); p.RateLimit != RateAuth || p == login {
		t.Errorf("OPTIONS /api/auth/login: %+v (must keep auth, and be a policy of its own)", p)
	}

	// A method chi does not route: chi's 405 handler answers it with no Allow header.
	if p := resolve(r, "PROPFIND", "/api/library"); p.Kind != KindMethodNotAllowed || len(p.Allowed) != 0 || p.Access != AuthRequired || p.Label != "/api/library" {
		t.Errorf("PROPFIND /api/library: %+v", p)
	}
	if p := resolve(r, "PROPFIND", "/nothing"); p.Kind != KindMethodNotAllowed || len(p.Allowed) != 0 || p.Access != AuthNone || p.Label != labelStatic {
		t.Errorf("PROPFIND /nothing: %+v", p)
	}
	if p := resolve(r, "PROPFIND", "/api/nothing"); p.Kind != KindMethodNotAllowed || p.Access != AuthRequired || p.Label != labelAPIOther {
		t.Errorf("PROPFIND /api/nothing: %+v", p)
	}
}

// The 405 bucket is the strictest of the path's routes, so changing the method buys no looser bucket; preflights
// pay general unless that bucket is auth.
func TestResolve_MethodNotAllowedBucket(t *testing.T) {
	mux := chi.NewRouter()
	rb := NewBuilder(mux)
	rb.Get("/api/stream", ok)
	rb.Post("/api/stream", ok, RateLimit(RateChat))
	rb.Get("/api/open", ok, Public())
	rb.Post("/api/open", ok)
	tbl, err := rb.Build()
	if err != nil {
		t.Fatal(err)
	}
	r := NewResolver(mux, tbl)
	if p := resolve(r, http.MethodPut, "/api/stream"); p.RateLimit != RateChat || p.Label != "/api/stream" {
		t.Errorf("PUT /api/stream: %+v, want the chat bucket", p)
	}
	if p := resolve(r, http.MethodOptions, "/api/stream"); p.RateLimit != RateGeneral {
		t.Errorf("OPTIONS /api/stream: %+v, want general", p)
	}
	if p := resolve(r, http.MethodPut, "/api/open"); p.Access != AuthRequired {
		t.Errorf("PUT /api/open: %+v, want authentication (one route on the path requires it)", p)
	}
}

// A path no route matches gets one of a few singletons, from the path chi dispatches on and the decoded one the
// NotFound handler serves.
func TestResolve_NoRoute(t *testing.T) {
	_, _, r := newSampleResolver(t)
	for _, c := range []struct {
		method, target string
		want           *Policy
	}{
		{http.MethodGet, "/api/nope", noRouteAPI.notFound},
		{http.MethodGet, "/api/library/x/nope", noRouteAPI.notFound},
		{http.MethodGet, "/api/v1/v1/library", noRouteAPI.notFound},
		{http.MethodGet, "/", noRouteRoot.notFound},
		{http.MethodPost, "/index.html", noRouteStatic.notFound},
		{http.MethodGet, "/api", noRouteStatic.notFound},
		{http.MethodGet, "/assets/app.js", noRouteAsset.notFound},
		{http.MethodHead, "/assets/x%2F..%2Fapp.js", noRouteAsset.notFound},
		{http.MethodPost, "/assets/app.js", noRouteStatic.notFound},
		{http.MethodGet, "/assets/x%2F..%2Fapp.js", noRouteAsset.notFound},   // dispatched and served under /assets/
		{http.MethodGet, "/assets/..%2Fsome%2Fpage", noRouteStatic.notFound}, // served as /some/page
		{http.MethodGet, "/assets/%2e%2e/robots.txt", noRouteStatic.notFound},
		{http.MethodGet, "/shared?token=t", noRouteShared.notFound},
		{http.MethodGet, "/x%2F..%2Fshared?token=t", noRouteShared.notFound}, // served as /shared
		{http.MethodGet, "/api/x%2F..%2F..%2Fshared", noRouteAPIShared.notFound},
	} {
		if got := resolve(r, c.method, c.target); got != c.want {
			t.Errorf("%s %s resolved to %+v, want %+v", c.method, c.target, got, c.want)
		}
	}
	for _, p := range []*Policy{noRouteAPI.notFound, noRouteAPIShared.notFound} {
		if p.Access != AuthRequired || p.Tx != TxNone || !p.AllowsToken([]string{auth.ScopeChat}) || p.Label != labelAPIOther {
			t.Errorf("API no-route policy %+v: want authentication, no transaction, no scope", p)
		}
	}
	if noRouteAsset.notFound.RateLimit != RateNone || noRouteShared.notFound.RateLimit != RateAnalysis || noRouteAPIShared.notFound.RateLimit != RateAnalysis {
		t.Error("asset or share-shell bucket wrong")
	}
}

// Resolve is called for every request, concurrently: the memoized 405 policies must stay one pointer each.
func TestResolve_ConcurrentMemoization(t *testing.T) {
	_, _, r := newSampleResolver(t)
	var wg sync.WaitGroup
	got := make([]*Policy, 64)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = resolve(r, http.MethodDelete, fmt.Sprintf("/api/library/id-%d", i))
		}(i)
	}
	wg.Wait()
	for _, p := range got[1:] {
		if p != got[0] || p.Kind != KindMethodNotAllowed {
			t.Fatalf("concurrent 405s resolved to different policies: %p %p", p, got[0])
		}
	}
}

// SetGuard reaches every endpoint registered through the Builder or one derived from it, including those
// registered before it was set; a guard that answers keeps the handler from running.
func TestSetGuard_RunsBeforeEveryEndpoint(t *testing.T) {
	mux := chi.NewRouter()
	rb := NewBuilder(mux)
	ran := 0
	h := func(w http.ResponseWriter, _ *http.Request) { ran++; w.WriteHeader(http.StatusNoContent) }
	rb.Get("/a", h)
	rb.Route("/api/x", func(rb *Builder) { rb.Get("/{id}", h, NoTx()) })
	if _, err := rb.Build(); err != nil {
		t.Fatal(err)
	}
	var guarded []string
	refuse := false
	rb.SetGuard(func(w http.ResponseWriter, r *http.Request, p *Policy) bool {
		guarded = append(guarded, p.Pattern)
		if refuse {
			w.WriteHeader(http.StatusTeapot)
			return false
		}
		return true
	})
	rb.With(func(next http.Handler) http.Handler { return next }).Get("/late", h) // after SetGuard
	if _, err := rb.Build(); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"/a", "/api/x/1", "/late"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != http.StatusNoContent {
			t.Errorf("GET %s = %d", target, rr.Code)
		}
	}
	if strings.Join(guarded, ",") != "/a,/api/x/{id},/late" || ran != 3 {
		t.Errorf("guard saw %v, handler ran %d times", guarded, ran)
	}
	refuse = true
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/a", nil))
	if rr.Code != http.StatusTeapot || ran != 3 {
		t.Errorf("a refusing guard: %d, handler ran %d times", rr.Code, ran)
	}
}
