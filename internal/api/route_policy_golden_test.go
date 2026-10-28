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
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"
)

var (
	updateRoutePolicy         = flag.Bool("update-route-policy", false, "rewrite testdata/route_policy.golden")
	updateRoutePolicyDeclared = flag.Bool("update-route-policy-declared", false, "rewrite testdata/route_policy_declared.golden")
)

// TestRoutePolicy_Golden records, for every registered route plus adversarial spellings of it and paths that are
// no route at all, what each legacy policy classifier decides: whether jwtAuth authenticates the request, the
// minimal scope sets a scoped token needs, the rate-limit bucket, the RLS mode, the deadline class, and the route
// chi dispatches to; plus the metrics label, which in every mode is the resolved policy's (Phase 1a). Every value
// comes from the function the middleware itself calls, on the URL as that layer sees it (outer layers: as
// received; inside the mux: after path.Clean and the /api/v1 alias).
//
// The legacy classifiers stay until one release after the flip to declarative (ADR 0006); the declared policy is
// recorded beside it in route_policy_declared.golden. Refresh only for an intended policy change:
// go test ./internal/api/ -run TestRoutePolicy_Golden -update-route-policy
func TestRoutePolicy_Golden(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), true)

	var b strings.Builder
	for _, req := range policyRequests(t, rt) {
		fmt.Fprintf(&b, "%s %s | %s\n", req.method, req.target, legacyDecisionsOf(rt, req.method, req.target))
	}
	compareGolden(t, filepath.Join("testdata", "route_policy.golden"), b.String(), *updateRoutePolicy, "-update-route-policy")
}

// TestRoutePolicyDeclared_Golden records, for the same requests, the policy the resolver gives each (the declared
// one): the kind (a route, a 405, no route) and pattern, who may call it (none, self or jwt), the minimal scope
// sets a scoped token needs, the rate-limit bucket, the request transaction, the deadline class, the metrics label,
// and a 405's Allow list. Where it differs from route_policy.golden on a 405, OPTIONS or no-route line, the
// difference is deliberate and reviewed here; on a route it must not differ (TestRoutePolicy_DeclaredMatchesLegacy).
// Refresh only for an intended policy change:
// go test ./internal/api/ -run TestRoutePolicyDeclared_Golden -update-route-policy-declared
func TestRoutePolicyDeclared_Golden(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), true)

	var b strings.Builder
	for _, req := range policyRequests(t, rt) {
		fmt.Fprintf(&b, "%s %s | %s\n", req.method, req.target, declaredDecisionsOf(rt, req.method, req.target))
	}
	compareGolden(t, filepath.Join("testdata", "route_policy_declared.golden"), b.String(), *updateRoutePolicyDeclared, "-update-route-policy-declared")
}

// TestRoutePolicy_DeclaredMatchesLegacy: for every request that resolves to a route, the declared policy decides
// exactly what the legacy classifiers decide (the metrics label excepted, which is now the pattern), and names the
// route chi dispatches to. This is what makes the flip to declarative a no-op for routes.
func TestRoutePolicy_DeclaredMatchesLegacy(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), true)
	compared := 0
	for _, req := range policyRequests(t, rt) {
		d := declaredDecisionsOf(rt, req.method, req.target)
		if d.kind != route.KindEndpoint.String() {
			continue
		}
		compared++
		l := legacyDecisionsOf(rt, req.method, req.target)
		legacyAuth := l.auth
		declaredAuth := d.auth
		if declaredAuth == route.AuthSelf.String() {
			declaredAuth = route.AuthNone.String()
		}
		rls := "none"
		if d.auth == route.AuthRequired.String() {
			rls = map[string]string{"tx": "tx", "none": "exempt"}[d.tx]
		}
		for _, c := range []struct{ name, legacy, declared string }{
			{"route", route.NormalizePattern(l.route), d.pattern},
			{"auth", legacyAuth, declaredAuth},
			{"scopes", l.scopes, d.scopes},
			{"ratelimit", l.ratelimit, d.ratelimit},
			{"rls", l.rls, rls},
			{"deadline", l.timeout, d.deadline},
		} {
			if c.legacy != c.declared {
				t.Errorf("%s %s: %s legacy %q, declared %q", req.method, req.target, c.name, c.legacy, c.declared)
			}
		}
	}
	if compared < 4*len(rt.routes.Policies()) {
		t.Fatalf("compared %d route requests; policyRequests or the resolver is broken", compared)
	}
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

// legacyDecisions is what the legacy classifiers decide for one request, as route_policy.golden records it; billed,
// like the metrics label, is the resolved policy's (route.Billed: the route can reach a billed provider call), marked
// billed=yes so a billed route's bucket is reviewed beside it (R-security-c22).
type legacyDecisions struct {
	route, auth, scopes, ratelimit, rls, timeout, metric string
	billed                                               bool
}

func (d legacyDecisions) String() string {
	s := fmt.Sprintf("route=%s auth=%s scopes=%s ratelimit=%s rls=%s timeout=%s metric=%s",
		d.route, d.auth, d.scopes, d.ratelimit, d.rls, d.timeout, d.metric)
	if d.billed {
		s += " billed=yes"
	}
	return s
}

// legacyDecisionsOf runs the legacy classifiers on method + target.
func legacyDecisionsOf(rt *Router, method, target string) legacyDecisions {
	r := httptest.NewRequest(method, target, nil)

	// Outer layers, before the mux (middleware_chain.go): the URL as received.
	bucket := rateLimitBucket(method, r.URL)
	if bucket == "" {
		bucket = "none"
	}
	deadline := middleware.TimeoutClass(r)
	resolved := rt.resolver.Resolve(method, r.URL)
	label := resolved.Label // what Metrics records, in every mode

	// Inside the mux: Router.ServeHTTP cleans Path, the alias middleware rewrites the URL, then jwtAuth and
	// rlsMiddleware judge routePath.
	u := *r.URL
	u.Path = path.Clean(u.Path)
	applyVersionAlias(&u)
	in := r.Clone(r.Context())
	in.URL = &u
	p := routePath(in)

	authn, scopes, rls := "jwt", minimalScopeSets(func(set []string) bool { return auth.RouteAllowed(method, p, set) }), "tx"
	switch {
	case !strings.HasPrefix(p, "/api/") || publicRoutes[p]:
		authn, scopes, rls = "none", "-", "none"
	case rlsExemptPath(p):
		rls = "exempt"
	}
	dispatched := rt.mux.Find(chi.NewRouteContext(), method, p)
	if dispatched == "" {
		dispatched = "-"
	}
	return legacyDecisions{dispatched, authn, scopes, bucket, rls, deadline, label, resolved.Billed}
}

// declaredDecisions is the policy the resolver gives one request, as route_policy_declared.golden records it.
type declaredDecisions struct {
	kind, pattern, auth, scopes, ratelimit, tx, deadline, label, allow string
	billed                                                             bool
}

func (d declaredDecisions) String() string {
	s := fmt.Sprintf("kind=%s pattern=%s auth=%s scopes=%s ratelimit=%s tx=%s deadline=%s label=%s",
		d.kind, d.pattern, d.auth, d.scopes, d.ratelimit, d.tx, d.deadline, d.label)
	if d.allow != "" {
		s += " allow=" + d.allow
	}
	if d.billed {
		s += " billed=yes"
	}
	return s
}

// declaredDecisionsOf resolves method + target as the outer layer does, before the router touches the URL.
func declaredDecisionsOf(rt *Router, method, target string) declaredDecisions {
	p := rt.resolver.Resolve(method, httptest.NewRequest(method, target, nil).URL)
	d := declaredDecisions{
		kind: p.Kind.String(), pattern: p.Pattern, auth: p.Access.String(), scopes: "-", ratelimit: p.RateLimit,
		tx: p.Tx.String(), deadline: p.Deadline.String(), label: p.Label, billed: p.Billed,
	}
	if d.pattern == "" {
		d.pattern = "-"
	}
	if d.ratelimit == "" {
		d.ratelimit = route.RateGeneral
	}
	if p.Access == route.AuthRequired {
		d.scopes = minimalScopeSets(p.AllowsToken)
	}
	if p.Kind == route.KindMethodNotAllowed {
		d.allow = strings.Join(p.Allowed, ",")
		if d.allow == "" {
			d.allow = "-"
		}
	}
	return d
}

// minimalScopeSets lists the smallest scope sets with which a scoped token may make a request allowed accepts
// ("any" when no scope is needed, "deny" when none suffices), e.g. "chat+write" for approving a chat fix.
func minimalScopeSets(allowed func(scopes []string) bool) string {
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
			if len(set) != size || !allowed(set) {
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
