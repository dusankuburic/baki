package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/reqinfo"
)

// Route policy (ADR 0006). Every route declares its policy where it is registered (route.Builder), and the
// request's policy is resolved once, at the outermost layer, before any layer that decides on it: authentication,
// token scopes, the rate-limit bucket, the RLS transaction, the deadline and the metrics label. The legacy path
// tables stay until one release after the flip to declarative (PAD_ROUTE_POLICY); until then shadow mode lets them
// decide and counts every disagreement.

// routePolicyMode is the PAD_ROUTE_POLICY in force: shadow when unset.
func routePolicyMode(cfg *config.Config) config.RoutePolicyMode {
	if m := cfg.Server.RoutePolicy; m != "" {
		return m
	}
	return config.RoutePolicyShadow
}

// finishRoutes completes the routes NewRouter registered through rb, once rt.routes holds the table rb.Build
// returned: it builds the resolver, installs the endpoint guard (guardEndpoint) on every route registered through
// rb, guards the 404 and 405 fallbacks, and checks that every route resolves to its own policy. NewRouter fails
// on its error: a route the resolver cannot reach would run under another route's policy.
func (rt *Router) finishRoutes(rb *route.Builder) error {
	if rt.routes == nil {
		return errors.New("route policy: no route table (set rt.routes from rb.Build first)")
	}
	rt.resolver = route.NewResolver(rt.mux, rt.routes)
	rb.SetGuard(rt.guardEndpoint)
	rt.guardFallbacks()
	return rt.selfTestRoutes()
}

// selfTestRoutes resolves a sample path of every route (route.SamplePath) and fails unless each resolves to the
// route's own policy. It also refuses a bucket BuildHandler has no limiter for, which would silently fall back to
// the general one, and an Idempotent route the idempotency layer cannot honour.
func (rt *Router) selfTestRoutes() error {
	var errs []error
	for _, p := range rt.routes.Policies() {
		sample := route.SamplePath(p.Pattern)
		if got := rt.resolver.Resolve(p.Method, &url.URL{Path: sample}); got != p {
			errs = append(errs, fmt.Errorf("route policy: %s %s (sample %s) resolves to the %s policy %q", p.Method, p.Pattern, sample, got.Kind, got.Label))
		}
		switch p.RateLimit {
		case "", route.RateNone, route.RateGeneral, route.RateAuth, route.RateAnalysis, route.RateChat, route.RateUpload:
		default:
			errs = append(errs, fmt.Errorf("route policy: %s %s: unknown rate-limit bucket %q", p.Method, p.Pattern, p.RateLimit))
		}
		if err := idempotentRouteError(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// resolvePolicy is the outer resolution layer (BuildHandler): it records the request's route policy on its
// reqinfo.Info, creating that if no outer layer did. It never touches r.URL: the router cleans the path and strips
// the /api/v1 alias later, and the resolver predicts both.
func (rt *Router) resolvePolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, rt.withPolicy(r))
	})
}

// withPolicy returns r with a reqinfo.Info carrying its resolved policy, installing whichever is missing.
// Router.ServeHTTP calls it too, for requests served without the outer layer (tests).
func (rt *Router) withPolicy(r *http.Request) *http.Request {
	info := reqinfo.From(r.Context())
	if info == nil {
		// The address RequestInfo would have recorded, so an audit row keeps it without the outer layer too.
		info = &reqinfo.Info{ClientIP: middleware.ClientIP(r, rt.trustedProxies)}
		r = r.WithContext(reqinfo.With(r.Context(), info))
	}
	if info.Policy == nil && rt.resolver != nil {
		info.Policy = rt.resolver.Resolve(r.Method, r.URL)
	}
	return r
}

// guardEndpoint runs before every route's handler (route.Handler). The policy the middleware applied must be the
// route's own: when chi dispatched elsewhere than the outer layer resolved, the request was authenticated, scoped,
// rate limited and given a transaction for another route. That is a resolver bug: it is logged and counted, and
// in declarative mode, where that policy decided, the request is refused before the handler runs. A route whose
// Needs the deployment lacks answers 503, in every mode.
func (rt *Router) guardEndpoint(w http.ResponseWriter, r *http.Request, p *route.Policy) bool {
	if resolved, _ := middleware.RequestPolicy(r); resolved != p && !rt.policyMismatch(w, r, resolved, p.Label) {
		return false
	}
	if len(p.Needs) > 0 && !rt.caps.Has(p.Needs...) {
		// Configuration, not failure, and any caller can ask: no ERROR line per request (render.ErrorUnlogged).
		render.ErrorUnlogged(w, fmt.Errorf("%s %s needs %s, which this deployment does not offer", p.Method, p.Pattern, joinCaps(p.Needs)), http.StatusServiceUnavailable)
		return false
	}
	return true
}

func joinCaps(caps []capability.Capability) string {
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}

// Names of the fallbacks chi dispatches to, as guard mismatches label them.
const (
	dispatchedNotFound         = "(no route)"
	dispatchedMethodNotAllowed = "(method not allowed)"
)

// policyMismatch reports a request chi dispatched (to the route or fallback named dispatched) elsewhere than its
// resolved policy says, and in declarative mode answers it with a 500. It returns whether the request may go on.
func (rt *Router) policyMismatch(w http.ResponseWriter, r *http.Request, resolved *route.Policy, dispatched string) bool {
	middleware.ReportGuardMismatch(r, resolved, dispatched)
	if rt.policyMode == config.RoutePolicyDeclarative {
		render.Error(w, errors.New("route policy mismatch"), http.StatusInternalServerError)
		return false
	}
	return true
}

// guardFallbacks wraps the NotFound and MethodNotAllowed handlers of the router and of every subrouter mounted on
// it (a 404 or 405 inside a subrouter is answered by that subrouter's own): the resolved policy must be a no-route
// or a 405 policy respectively. Children go first, so a parent's handler propagating to a child that has none is
// not wrapped twice.
func (rt *Router) guardFallbacks() {
	if rt.fallbacksGuarded {
		return
	}
	rt.fallbacksGuarded = true
	var guard func(m *chi.Mux)
	guard = func(m *chi.Mux) {
		for _, r := range m.Routes() {
			if sub, ok := r.SubRoutes.(*chi.Mux); ok {
				guard(sub)
			}
		}
		m.NotFound(rt.guardNotFound(m.NotFoundHandler()))
		m.MethodNotAllowed(rt.guardMethodNotAllowed(m.MethodNotAllowedHandler()))
	}
	guard(rt.mux)
}

func (rt *Router) guardNotFound(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, _ := middleware.RequestPolicy(r); p.Kind != route.KindNoRoute && !rt.policyMismatch(w, r, p, dispatchedNotFound) {
			return
		}
		next(w, r)
	}
}

// guardMethodNotAllowed also restores chi's Allow header: chi passes the allowed methods to its own default 405
// handler only, so the replacement takes them from the 405 policy (the same methods; chi's order is random).
func (rt *Router) guardMethodNotAllowed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := middleware.RequestPolicy(r)
		if p.Kind != route.KindMethodNotAllowed && !rt.policyMismatch(w, r, p, dispatchedMethodNotAllowed) {
			return
		}
		for _, m := range p.Allowed {
			w.Header().Add("Allow", m)
		}
		next(w, r)
	}
}

// shadowed reports whether shadow mode compares the legacy decisions for a request with policy p against p. Only
// routes are compared: the 405 and no-route policies differ from the legacy tables by design (ADR 0006), and
// those differences are reviewed in testdata/route_policy_declared.golden instead of counted on every preflight.
func (rt *Router) shadowed(p *route.Policy, resolved bool) bool {
	return rt.policyMode == config.RoutePolicyShadow && resolved && p.Kind == route.KindEndpoint
}

// authenticates reports whether jwtAuth authenticates r: the declared policy's Access in declarative mode, else
// the legacy rule (every /api/ path but publicRoutes, judged on the path chi dispatches on).
func (rt *Router) authenticates(r *http.Request, p *route.Policy, resolved bool) bool {
	declared := p.Access == route.AuthRequired
	if rt.policyMode == config.RoutePolicyDeclarative {
		return declared
	}
	path := routePath(r)
	legacy := strings.HasPrefix(path, "/api/") && !publicRoutes[path]
	if legacy != declared && rt.shadowed(p, resolved) {
		middleware.ReportPolicyMismatch(r, p, middleware.DecisionAuth, authName(legacy), authName(declared))
	}
	return legacy
}

func authName(authenticate bool) string {
	if authenticate {
		return "jwt"
	}
	return "none"
}

// tokenAllowed reports whether a scoped API token holding scopes may call r's route: the declared policy in
// declarative mode, else auth.RouteAllowed on the path chi dispatches on.
func (rt *Router) tokenAllowed(r *http.Request, p *route.Policy, resolved bool, scopes []string) bool {
	declared := p.AllowsToken(scopes)
	if rt.policyMode == config.RoutePolicyDeclarative {
		return declared
	}
	legacy := auth.RouteAllowed(r.Method, routePath(r), scopes)
	if legacy != declared && rt.shadowed(p, resolved) {
		middleware.ReportPolicyMismatch(r, p, middleware.DecisionScope, allowName(legacy), allowName(declared))
	}
	return legacy
}

func allowName(allowed bool) string {
	if allowed {
		return "allowed"
	}
	return "refused"
}

// wantsTx reports whether rlsMiddleware runs r inside the per-request RLS transaction: an authenticated route
// that has not opted out (route.NoTx) in declarative mode, else the legacy rule (every /api/ path but publicRoutes
// and the rlsExemptPath list, judged on the path chi dispatches on).
func (rt *Router) wantsTx(r *http.Request) bool {
	p, resolved := middleware.RequestPolicy(r)
	declared := p.Access == route.AuthRequired && p.Tx == route.TxRequest
	if rt.policyMode == config.RoutePolicyDeclarative {
		return declared
	}
	path := routePath(r)
	legacy := strings.HasPrefix(path, "/api/") && !publicRoutes[path] && !rlsExemptPath(path)
	if legacy != declared && rt.shadowed(p, resolved) {
		middleware.ReportPolicyMismatch(r, p, middleware.DecisionTx, txName(legacy), txName(declared))
	}
	return legacy
}

func txName(tx bool) string {
	if tx {
		return "tx"
	}
	return "none"
}

// rateLimitBucketOf returns the rate-limit bucket BuildHandler charges r to, "" when r is not rate limited: the
// declared policy's in declarative mode, else the legacy classifier's (rateLimitBucket, judged on the path chi
// will dispatch on).
func (rt *Router) rateLimitBucketOf(r *http.Request) string {
	p, resolved := middleware.RequestPolicy(r)
	declared := p.RateLimit
	switch declared {
	case route.RateNone:
		declared = ""
	case "":
		declared = route.RateGeneral
	}
	if rt.policyMode == config.RoutePolicyDeclarative {
		return declared
	}
	legacy := rateLimitBucket(r.Method, r.URL)
	if legacy != declared && rt.shadowed(p, resolved) {
		middleware.ReportPolicyMismatch(r, p, middleware.DecisionRateLimit, bucketName(legacy), bucketName(declared))
	}
	return legacy
}

func bucketName(bucket string) string {
	if bucket == "" {
		return route.RateNone
	}
	return bucket
}
