package api

import (
	"net/http"
	"net/url"
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

// BuildHandler assembles the FULL request-handling stack in this one place (the chi-internal layers live in routes_chi.go's registerRoutes). Resolved order, outermost/first-to-run first: 1 otelhttp; 2 Recovery; 3 RequestTimeout; 4 Compress; 5 AccessLog; 6 Metrics; 6b Recovery again INSIDE 5-6 so a panicking handler is still logged and counted as a 500; 7 rate limiting (cloud only, per-group buckets dispatched by rateLimitGroup; local mode skips it) applied inside the Router via registerRoutes r.Use(); then 8 securityHeaders, 9 corsHeaders, 10 jwtAuth, 11 rlsMiddleware, 12 the matched handler.
// Returns the wrapped handler plus the rate limiters so the caller can bind a listener and stop the limiters on shutdown.
func BuildHandler(router *Router, cfg *config.Config, redisClient *redis.Client) (http.Handler, []*middleware.RateLimiter) {
	var routerWithLimits http.Handler = router
	var rateLimiters []*middleware.RateLimiter

	if cfg.Mode == config.ModeCloud {
		// With Redis, build the limiters on the shared store so the effective
		// limit does not scale with replica count; else in-process.
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

		// Per-user write throttle: one bucket per authenticated user capping their total write volume across endpoints; cloud only (local is single-user and would self-DoS). Installed on the Router; appended to rateLimiters so Stop() runs on shutdown.
		perUserRl := newRL(cfg.Runtime.RateLimitPerUserRPS, cfg.Runtime.RateLimitPerUserBurst).SetGroup("peruser")
		router.SetPerUserLimiter(perUserRl)
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
			group := rateLimitBucket(r.Method, r.URL)
			if group == "" {
				router.ServeHTTP(w, r)
				return
			}
			rl, ok := rateLimitersByGroup[group]
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
							// Inner Recovery, INSIDE AccessLog and Metrics: with only the outer one, a handler panic unwound past both — no access-log line, no 5xx metric. The outer stays as the guard for the middleware layers themselves.
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

// Rate-limit group labels; unexported consts because they double as the limiter's group name
// in metrics/logs.
const (
	rlGroupAuth     = "auth"
	rlGroupAnalysis = "analysis"
	rlGroupChat     = "chat"
	rlGroupUpload   = "upload"
	rlGroupGeneral  = "general"
)

// authRateLimitPaths is the set of auth-shaped endpoints on the tighter auth bucket — deliberately including password recovery: those send email and run bcrypt, so the looser "general" bucket enabled email-flooding / SMTP cost amplification by rotating source IPs.
var authRateLimitPaths = map[string]struct{}{
	"/api/auth/login":           {},
	"/api/auth/refresh":         {},
	"/api/auth/register":        {},
	"/api/auth/change-password": {},
	"/api/auth/forgot-password": {},
	"/api/auth/reset-password":  {},
	// verify-email and sso/exchange are unauthenticated, token-consuming credential endpoints;
	// the tighter bucket keeps them from being flooded by rotating source IPs.
	"/api/auth/verify-email": {},
	"/api/auth/sso/exchange": {},
	// resend-verification sends email for any known unverified address; the tighter bucket is
	// what stops it becoming an SMTP-flooding primitive.
	"/api/auth/resend-verification": {},
}

// expensiveGetPaths: unauthenticated GETs that resolve a flow and run the analyzer — the report cache bounds steady-state cost, but a first-hit wave per leaked link still pays a full walk per flow, so they ride the tighter "analysis" bucket, not "general".
var expensiveGetPaths = map[string]struct{}{
	"/api/shared": {},
	// The share viewer's SPA shell (serveSharedShell): with a token it resolves the share and
	// reads the flow header per hit, unauthenticated — same bucket as the report it fronts.
	"/shared": {},
}

// canonicalRequestPath returns the path the router will ACTUALLY dispatch on — the single definition of that normalization, shared by registerRoutes's version-alias middleware and by rateLimitGroup, which runs OUTSIDE the mux and so before r.URL.Path is normalized.
// That ordering was a rate-limit bypass: POST /api/v1/auth/login and POST /api/auth/./login both reached the login handler but classified "general" — a 12x brute-force amplification. Mirrors the router: path.Clean, then the /api/v1 alias strip ("/api/v1foo" is distinct and left alone). Decoded r.URL.Path only — chi dispatches on RawPath when set, see routePath.
func canonicalRequestPath(p string) string {
	return stripVersionAlias(path.Clean(p))
}

// stripVersionAlias is the /api/v1 → /api half of canonicalRequestPath on its own: the alias applied to r.URL.RawPath, which chi routes verbatim and which must not be cleaned (path.Clean cannot tell "%2F" from a separator).
func stripVersionAlias(p string) string {
	if rest := strings.TrimPrefix(p, "/api/v1/"); rest != p {
		return "/api/" + rest
	}
	if p == "/api/v1" {
		return "/api"
	}
	return p
}

// applyVersionAlias rewrites u to the path chi dispatches on (the router's alias middleware): canonicalRequestPath
// on Path, the alias strip alone on RawPath, which chi routes on when set (an escaped "/" in an id must survive).
func applyVersionAlias(u *url.URL) {
	u.Path = canonicalRequestPath(u.Path)
	if u.RawPath != "" {
		u.RawPath = stripVersionAlias(u.RawPath)
	}
}

// routePath is the path chi dispatches r on, and so the only path a policy decision inside the mux may use (jwtAuth's public routes, rlsMiddleware's exemptions): RawPath verbatim when set, Path otherwise.
// Deciding on the cleaned, decoded Path instead let the two disagree — /api/system/settings/org/..%2F..%2Fhealth decodes and cleans to the public /api/health, so jwtAuth let it through anonymous while chi ran the protected route. publicRoutes matches exactly on this path, so it fails closed.
func routePath(r *http.Request) string {
	if r.URL.RawPath != "" {
		return r.URL.RawPath
	}
	if r.URL.Path == "" {
		return "/"
	}
	return r.URL.Path
}

// inboundRoutePath predicts routePath for a request that has not reached the Router yet (layer 7): path.Clean plus the alias on Path, only the alias on RawPath (see stripVersionAlias).
func inboundRoutePath(u *url.URL) string {
	if u.RawPath != "" {
		return stripVersionAlias(u.RawPath)
	}
	return canonicalRequestPath(u.Path)
}

// rateLimitBucket is the limiter group BuildHandler charges a request to, "" when it is not rate limited.
// /assets/* reads are exempt: content-hashed and immutable, and one SPA load fetches dozens in a burst — counting
// them made an ordinary reload 429 its own lazy chunks. Matched on BOTH the dispatch path (inboundRoutePath) and
// the decoded one (the NotFound fallback serves the decoded, cleaned path).
func rateLimitBucket(method string, u *url.URL) string {
	if method == http.MethodGet || method == http.MethodHead {
		if strings.HasPrefix(inboundRoutePath(u), "/assets/") && strings.HasPrefix(canonicalRequestPath(u.Path), "/assets/") {
			return ""
		}
	}
	return rateLimitGroup(method, u)
}

// rateLimitGroup classifies a request into its rate-limit group; pure (no I/O) so the policy is
// unit-testable. Classifies the dispatch path (inboundRoutePath), not the raw URL — see the above.
func rateLimitGroup(method string, u *url.URL) string {
	path := inboundRoutePath(u)
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
		// The provider connection test fires up to three billed completions per call — the same spend shape as a chat turn, so it rides the chat bucket; the other provider routes (list, the OAuth device flow) cost no model tokens and stay on general.
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

// azureOutboundRoute classifies the registry routes that dial Azure while the handler runs (own and org; dispatch path): POST …/test — one billed model call, the chat bucket — and POST …/connections/{id}/discover — the project listings, the analysis bucket.
// Both are also RLS-exempt (rlsExemptPath): their reads/writes carry explicit owner predicates and Go-layer authz. Every other registry path stays on general and inside the transaction.
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

// isOrgProviderTestPath matches /api/orgs/{id}/providers/azure-foundry/test (dispatch path): the same billed completions as /api/providers/test, so it shares that chat bucket and the RLS exemption (rlsExemptPath).
func isOrgProviderTestPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/orgs/")
	if !ok {
		return false
	}
	orgID, tail, ok := strings.Cut(rest, "/")
	return ok && orgID != "" && tail == "providers/azure-foundry/test"
}

// staticRoutePatterns returns the route patterns without dynamic segments from the router's own chi tree — it bounds the Prometheus "route" label (middleware.Metrics), and deriving it live means the set cannot drift from the routes.
func staticRoutePatterns(mux *chi.Mux) map[string]struct{} {
	out := make(map[string]struct{}, 192)
	// Walk only errors if the walk function does; ours never does.
	_ = chi.Walk(mux, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.ContainsAny(route, "{*") {
			return nil // dynamic — handled by an explicit collapse, or routeOther
		}
		// chi reports nested subrouter patterns with a trailing slash ("/api/orgs/" for a group's
		// own index); requests arrive path.Clean'd, which strips it.
		if route != "/" {
			route = strings.TrimSuffix(route, "/")
		}
		out[route] = struct{}{}
		return nil
	})
	return out
}
