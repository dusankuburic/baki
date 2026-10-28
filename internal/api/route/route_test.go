package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/auth"
)

// patternRecorder answers 204 and records the pattern chi reports at dispatch.
type patternRecorder struct{ last string }

func (p *patternRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.last = chi.RouteContext(r.Context()).RoutePattern()
	w.WriteHeader(http.StatusNoContent)
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

// buildSample registers a small tree in the shapes the real router uses: a subrouter root ("/"), a nested
// parameter subrouter, a prefix shared by two modules, a catch-all and an every-method Handle.
func buildSample(t *testing.T, rec http.HandlerFunc) (*chi.Mux, *Table) {
	t.Helper()
	mux := chi.NewRouter()
	rb := NewBuilder(mux)
	rb.Route("/api/library", func(rb *Builder) {
		rb.Get("/", rec)
		rb.Post("/", rec)
		rb.Route("/{id}", func(rb *Builder) {
			rb.Get("/", rec)
			rb.Get("/content", rec, NoTx())
		})
	})
	rb.Route("/api/admin", func(rb *Builder) { rb.Get("/audit", rec) }, Scopes(auth.ScopeAdmin))
	rb.Route("/api/admin", func(rb *Builder) { rb.Get("/system/health", rec) }, Scopes(auth.ScopeAdmin))
	rb.Route("/api/auth", func(rb *Builder) {
		rb.Post("/login", rec, Public(), RateLimit(RateAuth))
		rb.Get("/me", rec, Scopes(auth.ScopeRead))
		rb.Get("/sessions", rec)
	}, DenyTokens())
	rb.Get("/swagger/*", rec, SelfAuthenticated())
	rb.Handle("/metrics", rec, SelfAuthenticated())
	tbl, err := rb.Build()
	if err != nil {
		t.Fatal(err)
	}
	return mux, tbl
}

func TestBuild_KeysPoliciesByTheDispatchPattern(t *testing.T) {
	rec := &patternRecorder{}
	mux, tbl := buildSample(t, rec.ServeHTTP)
	for _, tc := range []struct{ method, path, want string }{
		{http.MethodGet, "/api/library", "/api/library"},
		{http.MethodPost, "/api/library", "/api/library"},
		{http.MethodGet, "/api/library/x", "/api/library/{id}"},
		{http.MethodGet, "/api/library/x/content", "/api/library/{id}/content"},
		{http.MethodGet, "/api/admin/audit", "/api/admin/audit"},
		{http.MethodGet, "/api/admin/system/health", "/api/admin/system/health"},
		{http.MethodGet, "/swagger/index.html", "/swagger/*"},
		{http.MethodDelete, "/metrics", "/metrics"},
	} {
		found := mux.Find(chi.NewRouteContext(), tc.method, tc.path)
		p := tbl.Lookup(tc.method, found)
		if p == nil || p.Pattern != tc.want || p.Method != tc.method {
			t.Errorf("%s %s: Find=%q policy=%+v, want pattern %q", tc.method, tc.path, found, p, tc.want)
			continue
		}
		rec.last = ""
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != http.StatusNoContent || rec.last != tc.want {
			t.Errorf("%s %s: dispatched with status %d to pattern %q, want %q", tc.method, tc.path, rr.Code, rec.last, tc.want)
		}
	}
	// chi's Find stops at a mount's stub, which answers every method: a method no route registers has no policy.
	if p := tbl.Lookup(http.MethodPut, mux.Find(chi.NewRouteContext(), http.MethodPut, "/api/library")); p != nil {
		t.Errorf("PUT /api/library resolved to %+v, want no policy", p)
	}
}

func TestBuild_AppliesDefaultsAndGroupOptions(t *testing.T) {
	_, tbl := buildSample(t, ok)
	get := func(method, pattern string) *Policy {
		t.Helper()
		p := tbl.Lookup(method, pattern)
		if p == nil {
			t.Fatalf("no policy for %s %s", method, pattern)
		}
		return p
	}
	if p := get(http.MethodPost, "/api/library"); p.Access != AuthRequired || strings.Join(p.Scopes, ",") != auth.ScopeWrite || p.RateLimit != RateGeneral || p.Tx != TxRequest || p.Label != "/api/library" {
		t.Errorf("POST /api/library defaults: %+v", p)
	}
	if p := get(http.MethodGet, "/api/library/{id}/content"); p.Tx != TxNone || strings.Join(p.Scopes, ",") != auth.ScopeRead {
		t.Errorf("content: %+v", p)
	}
	if p := get(http.MethodGet, "/api/admin/system/health"); strings.Join(p.Scopes, ",") != auth.ScopeAdmin {
		t.Errorf("shared prefix lost its group option: %+v", p)
	}
	if p := get(http.MethodPost, "/api/auth/login"); p.Access != AuthNone || p.TokensDenied || p.Tx != TxNone || p.RateLimit != RateAuth {
		t.Errorf("login: %+v", p)
	}
	if p := get(http.MethodGet, "/api/auth/me"); p.TokensDenied || !p.AllowsToken([]string{auth.ScopeRead}) {
		t.Errorf("me: %+v", p)
	}
	if p := get(http.MethodGet, "/api/auth/sessions"); !p.TokensDenied || p.AllowsToken(auth.ValidTokenScopes) {
		t.Errorf("sessions: %+v", p)
	}
	if p := get(http.MethodTrace, "/metrics"); p.Access != AuthSelf {
		t.Errorf("metrics: %+v", p)
	}
}

func TestBuild_RefusesARouteWithoutPolicy(t *testing.T) {
	mux := chi.NewRouter()
	rb := NewBuilder(mux)
	rb.Get("/api/ok", ok)
	mux.Get("/api/sneaky", ok)
	if _, err := rb.Build(); err == nil || !strings.Contains(err.Error(), "/api/sneaky") {
		t.Fatalf("Build() error = %v, want the unregistered route named", err)
	}
}

func TestBuild_WithRunsMiddlewareInsideTheEndpoint(t *testing.T) {
	mux := chi.NewRouter()
	rb := NewBuilder(mux)
	var order []string
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "mw")
			next.ServeHTTP(w, r)
		})
	}
	rb.With(mw).Get("/x", func(w http.ResponseWriter, r *http.Request) { order = append(order, "handler"); ok(w, r) })
	tbl, err := rb.Build()
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Lookup(http.MethodGet, "/x") == nil {
		t.Fatal("route registered with With has no policy")
	}
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if strings.Join(order, ",") != "mw,handler" {
		t.Fatalf("order = %v", order)
	}
}

// Billed marks a route that reaches a billed provider call and charges it to a cost bucket: the chat bucket when the
// route named none (or the general one), the route's own cost bucket otherwise (R-security-c22).
func TestBilled_MarksTheRouteAndDerivesItsCostBucket(t *testing.T) {
	mux := chi.NewRouter()
	rb := NewBuilder(mux)
	rb.Post("/a", ok, Billed())
	rb.Post("/b", ok, RateLimit(RateEmbed), Billed())
	rb.Post("/c", ok, Billed(), RateLimit(RateEmbed))
	rb.Post("/d", ok)
	tbl, err := rb.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path   string
		billed bool
		bucket string
	}{
		{"/a", true, RateChat},
		{"/b", true, RateEmbed},
		{"/c", true, RateEmbed},
		{"/d", false, RateGeneral},
	} {
		p := tbl.Lookup(http.MethodPost, tc.path)
		if p.Billed != tc.billed || p.RateLimit != tc.bucket {
			t.Errorf("POST %s: billed %v bucket %q, want %v %q", tc.path, p.Billed, p.RateLimit, tc.billed, tc.bucket)
		}
	}
	for _, b := range []string{RateChat, RateEmbed} {
		if !CostBucket(b) {
			t.Errorf("CostBucket(%q) = false", b)
		}
	}
	for _, b := range []string{"", RateGeneral, RateNone, RateAnalysis, RateUpload, RateControl, RateAuth} {
		if CostBucket(b) {
			t.Errorf("CostBucket(%q) = true: a billed route there shares a bucket with free traffic", b)
		}
	}
}
