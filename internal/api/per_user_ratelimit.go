package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
)

// perUserRateLimit is the chi inner-chain middleware (runs after jwtAuth, so
// claims are on the context) that caps one user's total write throughput via
// rt.perUserLimiter. When the limiter is nil (local mode, or cloud with the
// limiter not yet wired) it passes through untouched — so local single-user mode
// never self-DoSes. Reads (GET/HEAD/OPTIONS) bypass it; the per-IP limiter still
// covers them. Run-order is documented in middleware_chain.go.
func (rt *Router) perUserRateLimit(next http.Handler) http.Handler {
	if rt.perUserLimiter == nil {
		return next
	}
	return rt.perUserLimiter.LimitByKey(next, rt.perUserKey)
}

// perUserKey derives the per-user bucket key for a request:
//   - Reads (GET/HEAD/OPTIONS) → "" (skip; the per-IP limiter covers reads).
//   - Authenticated write → "ratelimit:peruser:" + hash(userID). The userID is
//     SHA-256-hex'd before keying to avoid delimiter collisions, unbounded Redis
//     key length, and control characters in keys/metrics/logs.
//   - Unauthenticated write (public POST like /api/auth/login) → per-IP fallback
//     so credential-stuffing from N origins is throttled per-origin, NOT
//     collapsed into a single shared bucket that would lock out every victim.
func (rt *Router) perUserKey(r *http.Request) string {
	if !isWriteMethod(r.Method) {
		return ""
	}
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil && claims.UserID != "" {
		return "ratelimit:peruser:" + hashRateLimitKey(claims.UserID)
	}
	return "ratelimit:peruser:ip:" + middleware.ClientIP(r, rt.trustedProxies)
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

// hashRateLimitKey SHA-256-hex-encodes a caller-supplied key segment (e.g. a
// userID) so it cannot poison the bucket key with delimiters/control-chars or
// grow it unboundedly in Redis. Deterministic: the same userID always maps to
// the same bucket.
func hashRateLimitKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
