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
func Metrics(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr := NewResponseRecorder(w)
		start := time.Now()
		h.ServeHTTP(maybeHijackable(sr), r)
		route := normalizeRoute(r.URL.Path)
		elapsed := time.Since(start).Seconds()
		metrics.HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(sr.status)).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(r.Method, route).Observe(elapsed)
	})
}

// normalizeRoute strips per-resource identifiers from URL paths so the
// Prometheus label set has bounded cardinality. Add a case here whenever
// a new dynamic-segment route is introduced.
//
// Cardinality matters: Prometheus stores one time-series per unique label
// combination. A naive `{path = "/api/library/abc123"}` label per flow id
// blows up the series count linearly with flows.
func normalizeRoute(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/library/"):
		return "/api/library/:id"
	case strings.HasPrefix(path, "/api/flows/") && strings.Contains(path, "/collaborators"):
		return "/api/flows/:id/collaborators"
	case strings.HasPrefix(path, "/api/admin/users/") && strings.HasSuffix(path, "/role"):
		return "/api/admin/users/:id/role"
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
		return path
	}
	if path == "/" || path == "" {
		return "/"
	}
	return "/static/*"
}
