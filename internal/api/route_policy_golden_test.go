package api

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"
)

var updateRoutePolicy = flag.Bool("update-route-policy", false, "rewrite testdata/route_policy.golden")

// TestRoutePolicy_Golden records, for every registered route plus adversarial spellings of it and paths that are
// no route at all, what each legacy policy classifier decides: whether jwtAuth authenticates the request, the
// minimal scope sets a scoped token needs, the rate-limit bucket, the RLS mode, the deadline class, the metrics
// label, and the route chi dispatches to. Every value comes from the function the middleware itself calls, on the
// URL as that layer sees it (outer layers: as received; inside the mux: after path.Clean and the /api/v1 alias).
//
// Phase 1a (docs/architecture/backend-refactoring-proposal.md) moves this policy onto the routes; its resolver
// must reproduce this file before any legacy table is deleted. Refresh only for an intended policy change:
// go test ./internal/api/ -run TestRoutePolicy_Golden -update-route-policy
func TestRoutePolicy_Golden(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), true)
	known := staticRoutePatterns(rt.mux)

	var b strings.Builder
	for _, req := range policyRequests(t, rt) {
		b.WriteString(describePolicy(rt, known, req.method, req.target))
		b.WriteByte('\n')
	}
	compareGolden(t, filepath.Join("testdata", "route_policy.golden"), b.String(), *updateRoutePolicy, "-update-route-policy")
}

type policyRequest struct{ method, target string }

var routeParamRE = regexp.MustCompile(`\{([^}:]+)(:[^}]*)?\}`)

// policyRequests lists every registered (method, pattern) with its parameters filled in, in the spellings that
// have produced policy bypasses before (/api/v1 alias, trailing slash, dot segment, encoded separator in a
// parameter, HEAD and OPTIONS), plus paths that match no route.
func policyRequests(t *testing.T, rt *Router) []policyRequest {
	t.Helper()
	type route struct{ method, pattern string }
	var routes []route
	if err := chi.Walk(rt.mux, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, route{method, pattern})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].pattern != routes[j].pattern {
			return routes[i].pattern < routes[j].pattern
		}
		return routes[i].method < routes[j].method
	})

	seen := map[policyRequest]bool{}
	var out []policyRequest
	add := func(method, target string) {
		r := policyRequest{method, target}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, r := range routes {
		concrete := routeParamRE.ReplaceAllString(r.pattern, "${1}1")
		concrete = strings.ReplaceAll(concrete, "*", "w")
		variants := []string{concrete}
		if rest, ok := strings.CutPrefix(concrete, "/api/"); ok {
			variants = append(variants, "/api/v1/"+rest, "/api/./"+rest)
		}
		if concrete != "/" && !strings.HasSuffix(concrete, "/") {
			variants = append(variants, concrete+"/")
		}
		if loc := routeParamRE.FindStringIndex(r.pattern); loc != nil {
			// An encoded "/" inside the first parameter: chi routes on the raw spelling.
			variants = append(variants, r.pattern[:loc[0]]+"a%2Fb"+routeParamRE.ReplaceAllString(strings.ReplaceAll(r.pattern[loc[1]:], "*", "w"), "${1}1"))
		}
		for _, v := range variants {
			add(r.method, v)
		}
		if r.method == http.MethodGet {
			add(http.MethodHead, concrete)
		}
		add(http.MethodOptions, concrete)
	}
	for _, extra := range []string{
		"/", "/index.html", "/assets/app.js", "/assets/../api/library", "/favicon.ico",
		"/shared", "/shared?token=t", "/x%2F..%2Fshared?token=t",
		"/api", "/api/v1", "/api/v1/v1/library", "/api/does-not-exist", "/api/library/a%2F..%2F..%2Fchat%2Fstream",
		"/ws", "/api/../ws", "/swagger", "/swagger/index.html", "/metrics", "/debug/pprof/heap", "/healthz", "/readyz",
	} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			add(m, extra)
		}
	}
	return out
}

// describePolicy renders one golden line for method + target.
func describePolicy(rt *Router, known map[string]struct{}, method, target string) string {
	r := httptest.NewRequest(method, target, nil)

	// Outer layers, before the mux (middleware_chain.go): the URL as received.
	bucket := rateLimitBucket(method, r.URL)
	if bucket == "" {
		bucket = "none"
	}
	deadline := middleware.TimeoutClass(r)
	label := middleware.RouteLabel(r.URL, known)

	// Inside the mux: Router.ServeHTTP cleans Path, the alias middleware rewrites the URL, then jwtAuth and
	// rlsMiddleware judge routePath.
	u := *r.URL
	u.Path = path.Clean(u.Path)
	applyVersionAlias(&u)
	in := r.Clone(r.Context())
	in.URL = &u
	p := routePath(in)

	authn, scopes, rls := "jwt", minimalScopeSets(method, p), "tx"
	switch {
	case !strings.HasPrefix(p, "/api/") || publicRoutes[p]:
		authn, scopes, rls = "none", "-", "none"
	case rlsExemptPath(p):
		rls = "exempt"
	}
	route := rt.mux.Find(chi.NewRouteContext(), method, p)
	if route == "" {
		route = "-"
	}
	return fmt.Sprintf("%s %s | route=%s auth=%s scopes=%s ratelimit=%s rls=%s timeout=%s metric=%s",
		method, target, route, authn, scopes, bucket, rls, deadline, label)
}

// minimalScopeSets lists the smallest scope sets with which a scoped token may make the request ("any" when no
// scope is needed, "deny" when none suffices), e.g. "chat+write" for approving a chat fix.
func minimalScopeSets(method, p string) string {
	all := auth.ValidTokenScopes
	var minimal [][]string
	for size := 0; size <= len(all); size++ {
		for mask := 0; mask < 1<<len(all); mask++ {
			var set []string
			for i, s := range all {
				if mask&(1<<i) != 0 {
					set = append(set, s)
				}
			}
			if len(set) != size || !auth.RouteAllowed(method, p, set) {
				continue
			}
			redundant := false
			for _, m := range minimal {
				if containsAll(set, m) {
					redundant = true
					break
				}
			}
			if !redundant {
				minimal = append(minimal, set)
			}
		}
	}
	switch {
	case len(minimal) == 0:
		return "deny"
	case len(minimal[0]) == 0:
		return "any"
	}
	names := make([]string, len(minimal))
	for i, m := range minimal {
		names[i] = strings.Join(m, "+")
	}
	return strings.Join(names, "|")
}

func containsAll(set, sub []string) bool {
	for _, s := range sub {
		found := false
		for _, x := range set {
			if x == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// compareGolden diffs got against the golden file at path (CRLF-tolerant), or rewrites it when update is set.
func compareGolden(t *testing.T, path, got string, update bool, flagName string) {
	t.Helper()
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (create it with %s)", path, err, flagName)
	}
	want := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if got == want {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	wantSet := map[string]bool{}
	for _, l := range wantLines {
		wantSet[l] = true
	}
	gotSet := map[string]bool{}
	var added, removed []string
	for _, l := range gotLines {
		gotSet[l] = true
		if !wantSet[l] {
			added = append(added, l)
		}
	}
	for _, l := range wantLines {
		if !gotSet[l] {
			removed = append(removed, l)
		}
	}
	const max = 20
	if len(added) > max {
		added = append(added[:max], "…")
	}
	if len(removed) > max {
		removed = append(removed[:max], "…")
	}
	t.Fatalf("%s differs from the current policy.\nonly in golden:\n  %s\nonly now:\n  %s\nIf the change is intended, rerun with %s and review the diff.",
		path, strings.Join(removed, "\n  "), strings.Join(added, "\n  "), flagName)
}
