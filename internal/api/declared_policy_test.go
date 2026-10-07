package api

import (
	"fmt"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
)

// TestDeclaredPolicies_MatchLegacy: every route's declared policy makes the decisions today's path tables make for
// it, through the same classifiers describePolicy records in route_policy.golden: jwtAuth's public routes, the
// scope rules, the rate-limit bucket, the RLS exemptions and the deadline class. Each route is judged on a
// concrete path built from its pattern. The tables still decide, so a mismatch is a declaration bug; a deliberate
// policy change belongs in its own reviewed step once the declarations decide.
func TestDeclaredPolicies_MatchLegacy(t *testing.T) {
	rt := newTestRouterSSO(testutil.NewFakeBackend(), true, nil, nil, func(cfg *config.Config) {
		cfg.Server.PprofEnabled = true // mounts /debug/pprof/* as well
	})
	policies := rt.routes.Policies()
	if len(policies) == 0 {
		t.Fatal("the router has no route policies")
	}
	for _, p := range policies {
		target := strings.ReplaceAll(routeParamRE.ReplaceAllString(p.Pattern, "${1}1"), "*", "w")
		if got := route.NormalizePattern(rt.mux.Find(chi.NewRouteContext(), p.Method, target)); got != p.Pattern {
			t.Errorf("%s %s: %s dispatches to %q, not to the route", p.Method, p.Pattern, target, got)
			continue
		}
		for _, m := range legacyMismatches(p, target) {
			t.Errorf("%s %s: %s", p.Method, p.Pattern, m)
		}
	}
}

// legacyMismatches lists where p disagrees with the legacy classifiers on a request for target, each judged on
// the URL its layer sees, as describePolicy does.
func legacyMismatches(p *route.Policy, target string) []string {
	var out []string
	differ := func(what string, declared, legacy any) {
		out = append(out, fmt.Sprintf("%s: declared %v, legacy %v", what, declared, legacy))
	}
	r := httptest.NewRequest(p.Method, target, nil)

	// Outer layers, before the mux: the URL as received.
	bucket := rateLimitBucket(p.Method, r.URL)
	if bucket == "" {
		bucket = route.RateNone
	}
	deadline := middleware.TimeoutClass(r)

	// Inside the mux: jwtAuth and rlsMiddleware judge routePath after path.Clean and the /api/v1 alias.
	u := *r.URL
	u.Path = path.Clean(u.Path)
	applyVersionAlias(&u)
	in := r.Clone(r.Context())
	in.URL = &u
	rp := routePath(in)
	authenticated := strings.HasPrefix(rp, "/api/") && !publicRoutes[rp]

	// AuthNone and AuthSelf alike: jwtAuth leaves the request alone.
	if declared := p.Access == route.AuthRequired; declared != authenticated {
		differ("authenticated by jwtAuth", declared, authenticated)
	}
	if authenticated {
		for _, scopes := range tokenScopeSets() {
			if declared, legacy := p.AllowsToken(scopes), auth.RouteAllowed(p.Method, rp, scopes); declared != legacy {
				differ(fmt.Sprintf("token with scopes %v allowed", scopes), declared, legacy)
			}
		}
	}
	declaredBucket := p.RateLimit
	if declaredBucket == "" {
		declaredBucket = route.RateGeneral
	}
	if declaredBucket != bucket {
		differ("rate-limit bucket", declaredBucket, bucket)
	}
	if declared, legacy := p.Tx == route.TxNone, !authenticated || rlsExemptPath(rp); declared != legacy {
		differ("outside the request transaction", declared, legacy)
	}
	if declared := p.Deadline.String(); declared != deadline {
		differ("deadline", declared, deadline)
	}
	return out
}

// tokenScopeSets returns every subset of the token scopes, the empty one included.
func tokenScopeSets() [][]string {
	all := auth.ValidTokenScopes
	sets := make([][]string, 0, 1<<len(all))
	for mask := range 1 << len(all) {
		var set []string
		for i, s := range all {
			if mask&(1<<i) != 0 {
				set = append(set, s)
			}
		}
		sets = append(sets, set)
	}
	return sets
}
