package api

import (
	"net/http"
	"strings"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/config"
	"pad-core/logger"

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
//  2. middleware.Recovery  — panic recovery (must wrap everything below)
//  3. middleware.RequestTimeout — per-request deadline
//  4. middleware.Compress  — response compression
//  5. middleware.AccessLog — structured request logging
//  6. middleware.Metrics   — Prometheus request metrics
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
					middleware.AccessLog(cfg.Server.TrustedProxies)(middleware.Metrics(routerWithLimits)),
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
}

// rateLimitGroup classifies a request into its rate-limit group. It is a pure
// function (no I/O) so the routing policy can be unit-tested independently of
// the fx wiring. Order matters only in that the explicit checks take precedence
// over the general fallback.
func rateLimitGroup(method, path string) string {
	if _, ok := authRateLimitPaths[path]; ok {
		return rlGroupAuth
	}
	if method == "POST" {
		if strings.HasPrefix(path, "/api/analysis/") {
			return rlGroupAnalysis
		}
		if path == "/api/chat/stream" {
			return rlGroupChat
		}
		if path == "/api/flow/upload" {
			return rlGroupUpload
		}
	}
	return rlGroupGeneral
}
