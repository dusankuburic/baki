package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync/atomic"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
)

// SetPerUserLimiter installs (or, with nil, removes) the limiter capping one
// authenticated user's total write throughput. BuildHandler calls it in cloud
// mode AFTER NewRouter registered the chi chain — hence the per-request read
// below.
func (rt *Router) SetPerUserLimiter(rl *middleware.RateLimiter) {
	rt.perUserLimiter.Store(rl)
}

// perUserLimitHandler is one limiter together with the chain position it was
// wrapped around, so the LimitByKey handler is built once per limiter value
// rather than once per request.
type perUserLimitHandler struct {
	limiter *middleware.RateLimiter
	handler http.Handler
}

// perUserRateLimit is the chi inner-chain middleware (after jwtAuth, claims
// on the context) that caps one user's total write throughput. With no
// limiter installed (local mode) it passes through — local single-user mode
// never self-DoSes. Reads bypass it; the per-IP limiter covers them.
//
// The limiter is loaded on EVERY request: chi evaluates each Use middleware
// once (inside NewRouter), and BuildHandler installs the limiter only
// afterwards — a construction-time nil check returned next for the life of
// the process and the throttle was a silent no-op (pinned by
// per_user_limiter_wiring_test.go).
func (rt *Router) perUserRateLimit(next http.Handler) http.Handler {
	// Per chain position (closes over next, so it cannot live on the Router);
	// rebuilt only when the installed limiter changes — two racing builds
	// both allocate and the last store wins, harmlessly.
	var cached atomic.Pointer[perUserLimitHandler]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rl := rt.perUserLimiter.Load()
		if rl == nil {
			next.ServeHTTP(w, r)
			return
		}
		c := cached.Load()
		if c == nil || c.limiter != rl {
			c = &perUserLimitHandler{limiter: rl, handler: rl.LimitByKey(next, rt.perUserKey)}
			cached.Store(c)
		}
		c.handler.ServeHTTP(w, r)
	})
}

// perUserKey derives the per-user bucket key for a request:
//   - Reads (GET/HEAD/OPTIONS) → "" (skip; the per-IP limiter covers reads).
//   - The stream commands (Stop, an approval decision; streamControl) → "" (skip): they reduce spend and act only on
//     the caller's own stream, and the per-address control bucket bounds them. Charged here, a user whose own
//     writes drained the cap could not stop a billed stream (R-streaming-c20). Deliberately no per-user control
//     ceiling either: like reads, they are bounded per address only, and one costs at most an owner lookup (a Redis
//     read for another replica's stream). A separate per-user bucket on this limiter would share its rate, so an
//     operator who tightens the write cap would tighten Stop with it.
//   - Authenticated write → "ratelimit:peruser:" + hash(userID). The userID is
//     SHA-256-hex'd before keying to avoid delimiter collisions, unbounded Redis
//     key length, and control characters in keys/metrics/logs.
//   - Unauthenticated write (public POST like /api/auth/login) → per-IP fallback
//     so credential-stuffing from N origins is throttled per-origin, NOT
//     collapsed into a single shared bucket that would lock out every victim.
func (rt *Router) perUserKey(r *http.Request) string {
	if !isWriteMethod(r.Method) || rt.streamControl(r) {
		return ""
	}
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil && claims.UserID != "" {
		return middleware.BucketKey(route.RatePerUser, hashRateLimitKey(claims.UserID))
	}
	// Keyed like the per-IP limiters (RateLimitAddr): an IPv6 client by its
	// /64, or rotating through the addresses of its own /64 bought a fresh
	// bucket per anonymous write.
	return middleware.BucketKey(route.RatePerUser, "ip:"+middleware.RateLimitAddr(middleware.ClientIP(r, rt.trustedProxies)))
}

// streamControl reports whether r is a stream command on the control bucket: the declared policy's bucket in
// declarative mode, else the legacy classifier's, as rateLimitBucketOf decides (without counting a disagreement
// again).
func (rt *Router) streamControl(r *http.Request) bool {
	if rt.policyMode == config.RoutePolicyDeclarative {
		p, _ := middleware.RequestPolicy(r)
		return p.RateLimit == route.RateControl
	}
	return rateLimitGroup(r.Method, r.URL) == route.RateControl
}

// isWriteMethod reports whether m is a state-changing verb. Per-user limiting
// applies only to writes; reads use the per-IP limiter.
func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// hashRateLimitKey SHA-256-hex-encodes a caller-supplied key segment so it
// cannot poison the bucket key with delimiters/control-chars or grow it
// unboundedly in Redis; deterministic per input.
func hashRateLimitKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
