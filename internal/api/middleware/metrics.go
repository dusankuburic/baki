package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"pad-analyzer/internal/metrics"
)

// MetricsHandler exposes the Prometheus /metrics endpoint. The router
// mounts it via the dispatch table. In production this should be reachable
// only from the metrics scraper — gate at the load balancer or move to a
// private listener if needed.
func MetricsHandler() http.Handler { return metrics.Handler() }

// RecordRateLimitExceeded is wired from ratelimit.go when a request is
// refused; lives here purely for naming locality (rate-limit code is in
// this package).
func RecordRateLimitExceeded(group string) { metrics.RecordRateLimitExceeded(group) }

// Metrics wraps h to record http_requests_total and http_request_duration_seconds.
// Apply it inside the AccessLog middleware so the metric label set (status)
// reflects the final response after later layers have run.
//
// knownRoutes is the set of registered STATIC route patterns (no dynamic
// segment). It bounds the "route" label: a path that is not a known route and
// not covered by an explicit collapse below is reported as routeOther rather
// than verbatim. Pass it from the router's own route tree — never a
// hand-maintained list. A nil set is fail-closed (everything unrecognized
// becomes routeOther), which is the safe direction for cardinality.
func Metrics(h http.Handler, knownRoutes map[string]struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr := NewResponseRecorder(w)
		start := time.Now()
		h.ServeHTTP(maybeHijackable(sr), r)
		route := normalizeRoute(r.URL.Path, knownRoutes)
		elapsed := time.Since(start).Seconds()
		metrics.HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(sr.status)).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(elapsed)
	})
}

// routeOther is the bounded catch-all for any /api/ path that is neither a
// registered static route nor one of the explicit dynamic collapses.
const routeOther = "/api/other"

// normalizeRoute strips per-resource identifiers from URL paths so the
// Prometheus label set has bounded cardinality.
//
// Cardinality matters: Prometheus stores one time-series per unique label
// combination. A naive `{path = "/api/library/abc123"}` label per flow id
// blows up the series count linearly with flows.
//
// Adding a case for a new dynamic-segment route is now an OPTIMIZATION, not a
// requirement: an uncased dynamic route collapses to routeOther, which is
// bounded but coarse. It used to be a requirement, and four routes had already
// been missed — see the block below.
func normalizeRoute(path string, known map[string]struct{}) string {
	switch {
	case strings.HasPrefix(path, "/api/library/"):
		return "/api/library/:id"
	case strings.HasPrefix(path, "/api/flows/") && strings.Contains(path, "/collaborators"):
		return "/api/flows/:id/collaborators"
	case strings.HasPrefix(path, "/api/admin/users/") && strings.HasSuffix(path, "/role"):
		return "/api/admin/users/:id/role"
	// The four families below used to fall through to the verbatim /api/
	// case, so each per-request identifier minted its own time series.
	// /api/invites/ was the worst of them: the path segment is the emailed
	// single-use invite CREDENTIAL, so every accept wrote a live secret into
	// a scraped, retained Prometheus label.
	case strings.HasPrefix(path, "/api/auth/sessions/"):
		return "/api/auth/sessions/:id"
	case strings.HasPrefix(path, "/api/auth/tokens/"):
		return "/api/auth/tokens/:id"
	case strings.HasPrefix(path, "/api/invites/"):
		return "/api/invites/:token/accept"
	case strings.HasPrefix(path, "/api/system/settings/org/"):
		return "/api/system/settings/org/:id"
	case strings.HasPrefix(path, "/api/orgs/"):
		// /api/orgs/:orgId or /api/orgs/:orgId/members[/:userId]
		switch {
		case strings.Contains(path, "/members/"):
			return "/api/orgs/:id/members/:userId"
		case strings.HasSuffix(path, "/members"):
			return "/api/orgs/:id/members"
		}
		return "/api/orgs/:id"
	case strings.HasPrefix(path, "/swagger/"):
		return "/swagger/*"
	case path == "/healthz", path == "/readyz", path == "/api/health", path == "/metrics":
		return path
	case strings.HasPrefix(path, "/api/"):
		// Only REGISTERED static routes are reported verbatim. This used to
		// return every /api/ path unchanged, which handed an unauthenticated
		// caller a cardinality bomb: the metrics layer runs outside the mux
		// and before auth, so `GET /api/<random>` created a fresh time series
		// per request — 404s included — until the process ran out of memory.
		if _, ok := known[path]; ok {
			return path
		}
		return routeOther
	}
	if path == "/" || path == "" {
		return "/"
	}
	return "/static/*"
}
