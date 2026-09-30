package api

import (
	"net/http"
	"path"
	"strings"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/config"
	"pad-core/logger"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// This file is the single place that assembles the FULL request-handling
// middleware stack, both the layers that wrap the router from the outside
// (tracing, panic recovery, timeout, compression, access logging, metrics,
// rate limiting) and — by cross-reference — the layers chi applies internally
// via registerRoutes (routes_chi.go). Previously these were split across
// main.go (the outer layers, hand-nested) and this package (the inner layers,
// via chi's r.Use()), so auditing or changing the order required reasoning
// across two files in two different packages. Consolidating the outer-layer
// assembly here — right next to registerRoutes — means the full stack is one
// directory listing away instead of a cross-package hunt, with zero change to
// the actual composition or order.
//
// The full, resolved middleware order (outermost/first-to-run at top):
//
//  1. otelhttp            — OpenTelemetry tracing span
//  2. middleware.Recovery  — panic recovery for the middleware layers below
//  3. middleware.RequestTimeout — per-request deadline
//  4. middleware.Compress  — response compression
//  5. middleware.AccessLog — structured request logging
//  6. middleware.Metrics   — Prometheus request metrics
//     6b. middleware.Recovery — panic recovery for HANDLERS, inside 5-6 so a
//     panicking request is still logged and counted as a 500
//  7. rate limiting (cloud mode only) — per-group token bucket, dispatched by
//     rateLimitGroup(method, path); local mode skips this layer entirely
//     -- inside the *Router (chi.Mux), applied via registerRoutes's r.Use() --
//  8. Router.securityHeaders
//  9. Router.corsHeaders
//  10. Router.jwtAuth
//  11. Router.rlsMiddleware
//  12. the matched route handler
//
// BuildHandler returns the fully-wrapped handler (layers 1-7) plus the rate
// limiters it constructed, so the caller (main.go's startServer) can bind a
// listener to it and stop the limiters on shutdown. Layers 8-11 are applied
// inside router itself (registerRoutes ran during NewRouter) and are not
// re-described here.
func BuildHandler(router *Router, cfg *config.Config, redisClient *redis.Client) (http.Handler, []*middleware.RateLimiter) {
	var routerWithLimits http.Handler = router
	var rateLimiters []*middleware.RateLimiter

	if cfg.Mode == config.ModeCloud {
		// When the Redis backplane is configured, build the limiters on the
		// shared store so the effective limit does not scale with replica count
		// (#1). Otherwise each limiter is in-process (correct for single-replica).
		newRL := func(rps, burst float64) *middleware.RateLimiter {
			return middleware.NewRateLimiterRedis(redisClient, rps, burst, cfg.Server.TrustedProxies)
		}
		generalRl := newRL(cfg.Runtime.RateLimitGeneralRPS, cfg.Runtime.RateLimitGeneralBurst).SetGroup("general")
		authRl := newRL(cfg.Runtime.RateLimitAuthRPS, cfg.Runtime.RateLimitAuthBurst).SetGroup("auth")
		analysisRl := newRL(cfg.Runtime.RateLimitExpensiveRPS, cfg.Runtime.RateLimitExpensiveBurst).SetGroup("analysis")
		chatRl := newRL(cfg.Runtime.RateLimitChatRPS, cfg.Runtime.RateLimitChatBurst).SetGroup("chat")
		uploadRl := newRL(cfg.Runtime.RateLimitUploadRPS, cfg.Runtime.RateLimitUploadBurst).SetGroup("upload")
		if redisClient != nil {
			logger.Info("rate limiting: using shared Redis backplane", "url_set", cfg.Redis.URL != "")
		}
		rateLimiters = append(rateLimiters, generalRl, authRl, analysisRl, chatRl, uploadRl)

		// Per-user write throttle: a single bucket per authenticated user capping
		// their total write volume across endpoints (Track 5). Cloud mode only —
		// local mode is single-user, so it would self-DoS (all traffic → one
		// "local" identity). Stored on the Router so the post-jwtAuth chi
		// middleware reads it at request time; appended to rateLimiters so its
		// Stop() runs on shutdown alongside the per-IP limiters.
		perUserRl := newRL(cfg.Runtime.RateLimitPerUserRPS, cfg.Runtime.RateLimitPerUserBurst).SetGroup("peruser")
		router.perUserLimiter = perUserRl
		rateLimiters = append(rateLimiters, perUserRl)

		// rateLimitersByGroup maps the rateLimitGroup classifier to its limiter
		// so the per-request dispatch is a single lookup.
		rateLimitersByGroup := map[string]*middleware.RateLimiter{
			"general":  generalRl,
			"auth":     authRl,
			"analysis": analysisRl,
			"chat":     chatRl,
			"upload":   uploadRl,
		}

		routerWithLimits = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// /assets/* is exempt from rate limiting. Those files are
			// content-hashed and immutable, and a single SPA load fetches
			// dozens of them in one burst — counting them against the per-IP
			// general bucket made an ordinary reload 429 its own lazy chunks,
			// which the browser surfaces as "Failed to fetch dynamically
			// imported module". APIs (including the SPA's / index.html) stay
			// throttled as designed.
			if method := r.Method; method == http.MethodGet || method == http.MethodHead {
				if strings.HasPrefix(canonicalRequestPath(r.URL.Path), "/assets/") {
					router.ServeHTTP(w, r)
					return
				}
			}
			rl, ok := rateLimitersByGroup[rateLimitGroup(r.Method, r.URL.Path)]
			if !ok {
				rl = generalRl
			}
			rl.Limit(router).ServeHTTP(w, r)
		})
	}

	timeoutDur := 30 * time.Second
	if d, err := time.ParseDuration(cfg.Runtime.RequestTimeout); err == nil {
		timeoutDur = d
	}

	handler := otelhttp.NewHandler(
		middleware.Recovery(
			middleware.RequestTimeout(timeoutDur)(
				middleware.Compress(
					middleware.AccessLog(cfg.Server.TrustedProxies)(
						middleware.Metrics(
							// Inner Recovery, INSIDE AccessLog and Metrics. Neither of
							// those defers its bookkeeping, so with only the outer
							// Recovery a handler panic unwound straight past both: no
							// access-log line and no http_requests_total{5xx} sample
							// for exactly the requests a 5xx alert exists for. The
							// outer Recovery stays as the guard for the middleware
							// layers themselves.
							middleware.Recovery(routerWithLimits),
							staticRoutePatterns(router.mux),
						),
					),
				),
			),
		),
		"http.server",
	)

	return handler, rateLimiters
}

// Rate-limit group labels returned by rateLimitGroup. Kept as unexported consts
// (not string-typed enums) because they double as the limiter's group name in
// metrics/logs.
const (
	rlGroupAuth     = "auth"
	rlGroupAnalysis = "analysis"
	rlGroupChat     = "chat"
	rlGroupUpload   = "upload"
	rlGroupGeneral  = "general"
)

// authRateLimitPaths is the set of auth-shaped endpoints that share the tighter
// auth rate-limit bucket. It deliberately includes the password-recovery
// endpoints (forgot-password / reset-password): those send email and run bcrypt
// on the reset path, so leaving them on the looser "general" bucket enabled
// email-flooding / SMTP cost amplification by attackers rotating source IPs.
var authRateLimitPaths = map[string]struct{}{
	"/api/auth/login":           {},
	"/api/auth/refresh":         {},
	"/api/auth/register":        {},
	"/api/auth/change-password": {},
	"/api/auth/forgot-password": {},
	"/api/auth/reset-password":  {},
	// verify-email and sso/exchange are unauthenticated, token-consuming
	// credential endpoints; keep them on the tighter auth bucket rather than the
	// looser general one so they can't be flooded by rotating source IPs.
	"/api/auth/verify-email": {},
	"/api/auth/sso/exchange": {},
	// resend-verification sends email for any known unverified address; the
	// tighter bucket is what stops it becoming an SMTP-flooding primitive.
	"/api/auth/resend-verification": {},
}

// expensiveGetPaths are unauthenticated GETs that resolve a flow and run the
// analyzer (B1.9): the per-flow report cache bounds steady-state cost, but a
// first-hit wave per leaked link still pays a full walk per flow — they ride
// the tighter "analysis" bucket, not "general".
var expensiveGetPaths = map[string]struct{}{
	"/api/shared": {},
}

// canonicalRequestPath returns the path the router will ACTUALLY dispatch on,
// given a raw inbound request path. It is the single definition of that
// normalization, shared by two callers that must never disagree:
//
//   - registerRoutes's version-alias middleware (inside the chi mux), and
//   - rateLimitGroup, which runs in BuildHandler's layer 7 — OUTSIDE the mux,
//     and therefore before any of this has been applied to r.URL.Path.
//
// That ordering was a rate-limit bypass. The classifier saw the raw string, so
// `POST /api/v1/auth/login` and `POST /api/auth/./login` both routed to the
// real login handler but classified as "general" (60/min by default) instead
// of "auth" (5/min) — a 12x brute-force and password-reset-email
// amplification. The same trick moved chat spend and uploads off their
// dedicated buckets. Any future normalization the router grows belongs here,
// so the classifier inherits it automatically.
//
// The two steps mirror the router exactly and in the same order: path.Clean
// (Router.ServeHTTP) collapses ".", "..", and duplicate slashes; the /api/v1
// alias strip (registerRoutes) follows. "/api/v1" must be a whole segment —
// "/api/v1foo" is a distinct path and is left alone.
func canonicalRequestPath(p string) string {
	p = path.Clean(p)
	if rest := strings.TrimPrefix(p, "/api/v1/"); rest != p {
		return "/api/" + rest
	}
	if p == "/api/v1" {
		return "/api"
	}
	return p
}

// rateLimitGroup classifies a request into its rate-limit group. It is a pure
// function (no I/O) so the routing policy can be unit-tested independently of
// the fx wiring. Order matters only in that the explicit checks take precedence
// over the general fallback.
//
// The path is normalized first: this runs outside the mux, so a raw
// non-canonical spelling would otherwise be classified on a string that never
// reaches a handler (see canonicalRequestPath).
func rateLimitGroup(method, path string) string {
	path = canonicalRequestPath(path)
	if _, ok := authRateLimitPaths[path]; ok {
		return rlGroupAuth
	}
	if _, ok := expensiveGetPaths[path]; ok {
		return rlGroupAnalysis
	}
	if method == "POST" {
		if strings.HasPrefix(path, "/api/analysis/") {
			return rlGroupAnalysis
		}
		if path == "/api/chat/stream" {
			return rlGroupChat
		}
		// The provider connection test fires up to three real, billed chat
		// completions per call (connectionTestModels) — the same spend shape
		// as a chat turn, so it rides the tight chat bucket, not general.
		// The other provider routes (list, the OAuth device flow) cost no
		// model tokens and stay on general.
		if path == "/api/providers/test" || isOrgProviderTestPath(path) {
			return rlGroupChat
		}
		if group, ok := azureOutboundRoute(path); ok {
			return group
		}
		if path == "/api/flow/upload" {
			return rlGroupUpload
		}
	}
	return rlGroupGeneral
}

// azureOutboundRoute classifies the Azure AI Foundry registry routes that
// dial Azure while the handler runs, the caller's own and an
// organisation's (/api/orgs/{id}/…; path already canonical —
// canonicalRequestPath):
//
//   - POST …/providers/azure-foundry/test — a resource probe or one tiny
//     model call Azure bills: the chat bucket, like /api/providers/test;
//   - POST …/providers/azure-foundry/connections/{id}/discover — the
//     project's deployment and agent listings (a sign-in, then every page of
//     both): the analysis bucket.
//
// Both are also exempt from the request-long RLS transaction (rlsExemptPath):
// held across the upstream dial it pinned a pooled connection, while their
// registry reads and writes carry explicit owner predicates and the handlers'
// role checks are Go-layer authz. ok is false for every other path — the
// registry's CRUD routes stay on the general bucket and inside the
// transaction.
func azureOutboundRoute(path string) (group string, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/")
	if !found {
		return "", false
	}
	if orgRest, isOrg := strings.CutPrefix(rest, "orgs/"); isOrg {
		orgID, tail, cut := strings.Cut(orgRest, "/")
		if !cut || orgID == "" {
			return "", false
		}
		rest = tail
	}
	tail, found := strings.CutPrefix(rest, "providers/azure-foundry/")
	if !found {
		return "", false
	}
	if tail == "test" {
		return rlGroupChat, true
	}
	if segs := strings.Split(tail, "/"); len(segs) == 3 && segs[0] == "connections" && segs[1] != "" && segs[2] == "discover" {
		return rlGroupAnalysis, true
	}
	return "", false
}

// isOrgProviderTestPath matches an org admin's connection test of the
// organisation's shared credential, /api/orgs/{id}/providers/azure-foundry/test
// (path already canonical): the same billed chat completions as
// /api/providers/test, so it shares that route's chat bucket and its
// exemption from the request-long RLS transaction (rlsExemptPath).
func isOrgProviderTestPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/orgs/")
	if !ok {
		return false
	}
	orgID, tail, ok := strings.Cut(rest, "/")
	return ok && orgID != "" && tail == "providers/azure-foundry/test"
}

// staticRoutePatterns returns the set of registered route patterns that carry
// no dynamic segment, read from the router's own chi route tree.
//
// It bounds the Prometheus "route" label (see middleware.Metrics). Deriving it
// from the live tree rather than a hand-written list is the point: the list
// cannot drift from the routes, and a route added tomorrow is recognized
// without anyone remembering to update a second place.
func staticRoutePatterns(mux *chi.Mux) map[string]struct{} {
	out := make(map[string]struct{}, 192)
	// Walk only errors if the walk function does; ours never does.
	_ = chi.Walk(mux, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.ContainsAny(route, "{*") {
			return nil // dynamic — handled by an explicit collapse, or routeOther
		}
		// chi reports nested subrouter patterns with a trailing slash
		// ("/api/orgs/" for a Route group's own index); requests arrive
		// path.Clean'd, which strips it.
		if route != "/" {
			route = strings.TrimSuffix(route, "/")
		}
		out[route] = struct{}{}
		return nil
	})
	return out
}
