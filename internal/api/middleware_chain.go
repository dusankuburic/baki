package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/config"
	"pad-core/logger"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// BuildHandler assembles the FULL request-handling stack in this one place (the chi-internal layers live in routes_chi.go's registerRoutes). Resolved order, outermost/first-to-run first: 1 otelhttp; 2 Recovery; 3 RequestInfo (the request's id, addresses and the actor authentication records later); 4 the route-policy resolution (resolvePolicy: it records the policy every layer below decides on, and never rewrites the URL); 5 RequestTimeout; 6 Compress; 7 AccessLog; 8 Metrics; 8b Recovery again INSIDE 7-8 so a panicking handler is still logged and counted as a 500; 9 rate limiting (cloud only, the bucket from rateLimitBucketOf; local mode skips it); then, inside the Router via registerRoutes r.Use(), 10 securityHeaders, 11 corsHeaders, 12 the /api/v1 alias, 13 jwtAuth, 14 the per-user limit, 15 rlsMiddleware, 16 idempotency (route.Idempotent creates, inside the request transaction), 17 the matched handler behind its policy guard.
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
		generalRl := newRL(cfg.Runtime.RateLimitGeneralRPS, cfg.Runtime.RateLimitGeneralBurst).SetGroup(route.RateGeneral)
		authRl := newRL(cfg.Runtime.RateLimitAuthRPS, cfg.Runtime.RateLimitAuthBurst).SetGroup(route.RateAuth)
		analysisRl := newRL(cfg.Runtime.RateLimitExpensiveRPS, cfg.Runtime.RateLimitExpensiveBurst).SetGroup(route.RateAnalysis)
		chatRl := newRL(cfg.Runtime.RateLimitChatRPS, cfg.Runtime.RateLimitChatBurst).SetGroup(route.RateChat)
		uploadRl := newRL(cfg.Runtime.RateLimitUploadRPS, cfg.Runtime.RateLimitUploadBurst).SetGroup(route.RateUpload)
		channelTestRl := newRL(cfg.Runtime.RateLimitChannelTestRPS, cfg.Runtime.RateLimitChannelTestBurst).SetGroup(route.RateChannelTest)
		avatarRl := newRL(cfg.Runtime.RateLimitAvatarRPS, cfg.Runtime.RateLimitAvatarBurst).SetGroup(route.RateAvatar)
		if redisClient != nil {
			logger.Info("rate limiting: using shared Redis backplane", "url_set", cfg.Redis.URL.IsSet())
		}
		rateLimiters = append(rateLimiters, generalRl, authRl, analysisRl, chatRl, uploadRl, channelTestRl, avatarRl)

		// Per-user write throttle: one bucket per authenticated user capping their total write volume across endpoints; cloud only (local is single-user and would self-DoS). Installed on the Router; appended to rateLimiters so Stop() runs on shutdown.
		perUserRl := newRL(cfg.Runtime.RateLimitPerUserRPS, cfg.Runtime.RateLimitPerUserBurst).SetGroup(route.RatePerUser)
		router.SetPerUserLimiter(perUserRl)
		rateLimiters = append(rateLimiters, perUserRl)

		// rateLimitersByGroup maps a bucket (rateLimitBucketOf) to its limiter
		// so the per-request dispatch is a single lookup.
		rateLimitersByGroup := map[string]*middleware.RateLimiter{
			route.RateGeneral:     generalRl,
			route.RateAuth:        authRl,
			route.RateAnalysis:    analysisRl,
			route.RateChat:        chatRl,
			route.RateUpload:      uploadRl,
			route.RateChannelTest: channelTestRl,
			route.RateAvatar:      avatarRl,
		}

		routerWithLimits = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			group := router.rateLimitBucketOf(r)
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

	// config.Load refuses a non-positive one; 0 is a Config built without config.Default (tests).
	timeoutDur := cfg.Runtime.RequestTimeout.Std()
	if timeoutDur <= 0 {
		timeoutDur = 30 * time.Second
	}

	handler := otelhttp.NewHandler(
		middleware.Recovery(
			middleware.RequestInfo(cfg.Server.TrustedProxies)(
				router.resolvePolicy(
					middleware.RequestTimeout(timeoutDur, router.policyMode)(
						middleware.Compress(
							middleware.AccessLog(cfg.Server.TrustedProxies)(
								middleware.Metrics(
									// Inner Recovery, INSIDE AccessLog and Metrics: with only the outer one, a handler panic unwound past both — no access-log line, no 5xx metric. The outer stays as the guard for the middleware layers themselves.
									middleware.Recovery(routerWithLimits),
								),
							),
						),
					),
				),
			),
		),
		"http.server",
	)

	return handler, rateLimiters
}

// Rate-limit group labels: the route package's bucket names, which double as the limiter's group name in metrics,
// logs and Redis keys (testdata/ratelimit_groups.golden pins them).
const (
	rlGroupAuth     = route.RateAuth
	rlGroupAnalysis = route.RateAnalysis
	rlGroupChat     = route.RateChat
	rlGroupUpload   = route.RateUpload
	rlGroupGeneral  = route.RateGeneral
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

// applyVersionAlias rewrites u to the path chi dispatches on (the router's alias middleware): route.CanonicalPath on
// Path, the alias strip alone on RawPath, which chi routes on when set (an escaped "/" in an id must survive).
func applyVersionAlias(u *url.URL) {
	u.Path = route.CanonicalPath(u.Path)
	if u.RawPath != "" {
		u.RawPath = route.StripVersionAlias(u.RawPath)
	}
}

// routePath is the path chi dispatches r on once inside the mux, after applyVersionAlias (route.DispatchPath names
// it before): RawPath verbatim when set, Path otherwise. The legacy decisions inside the mux (jwtAuth's public
// routes, rlsMiddleware's exemptions) are judged on it. Deciding on the cleaned, decoded Path instead let the two
// disagree: /api/system/settings/org/..%2F..%2Fhealth decodes and cleans to the public /api/health, so jwtAuth let
// it through anonymous while chi ran the protected route. publicRoutes matches exactly on this path, so it fails
// closed.
func routePath(r *http.Request) string {
	if r.URL.RawPath != "" {
		return r.URL.RawPath
	}
	if r.URL.Path == "" {
		return "/"
	}
	return r.URL.Path
}

// rateLimitBucket is the legacy limiter group of a request, "" when it is not rate limited.
// /assets/* reads are exempt: content-hashed and immutable, and one SPA load fetches dozens in a burst — counting
// them made an ordinary reload 429 its own lazy chunks. Matched on BOTH the dispatch path (route.DispatchPath) and
// the decoded one (the NotFound fallback serves the decoded, cleaned path).
func rateLimitBucket(method string, u *url.URL) string {
	if method == http.MethodGet || method == http.MethodHead {
		if strings.HasPrefix(route.DispatchPath(u), "/assets/") && strings.HasPrefix(route.CanonicalPath(u.Path), "/assets/") {
			return ""
		}
	}
	return rateLimitGroup(method, u)
}

// rateLimitGroup classifies a request into its legacy rate-limit group; pure (no I/O) so the policy is
// unit-testable. It classifies the dispatch path (route.DispatchPath), not the raw URL: rate limiting runs outside
// the mux, before its path.Clean and alias strip, and on the raw string POST /api/v1/auth/login and
// /api/auth/./login read as "general", a 12x brute-force amplification.
func rateLimitGroup(method string, u *url.URL) string {
	path := route.DispatchPath(u)
	if _, ok := authRateLimitPaths[path]; ok {
		return rlGroupAuth
	}
	if _, ok := expensiveGetPaths[path]; ok {
		return rlGroupAnalysis
	}
	if method == "POST" {
		if strings.HasPrefix(path, "/api/analysis/") || path == "/api/integrations/ci" {
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
		if path == "/api/auth/profile/avatar" {
			return route.RateAvatar
		}
		// The channel test makes the server POST to an admin-chosen URL for up to 15 s (B13).
		if isOrgChannelTestPath(path) {
			return route.RateChannelTest
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
