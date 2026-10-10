package middleware

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"pad-analyzer/internal/metrics"
)

// The HTTP layer's metrics, registered from this package.
var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests processed, labeled by method, normalized route, and status.",
		},
		[]string{"method", "route", "status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by method and normalized route.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "route"},
	)
	rateLimitExceededTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rate_limit_exceeded_total",
			Help: "Requests refused by the per-IP rate limiter, by endpoint group.",
		},
		[]string{"group"},
	)
	// Route policy (ADR 0006). Both route labels are policy labels: the
	// matched pattern or one of a few fixed catch-alls, never a request path.
	routePolicyMismatchTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_route_policy_mismatch_total",
			Help: "Shadow mode (PAD_ROUTE_POLICY=shadow): requests for which the policy declared on the route decides differently from the legacy path tables, by route and decision (auth, scope, ratelimit, tx, deadline). Must stay at zero over a soak before the flip to declarative.",
		},
		[]string{"route", "decision"},
	)
	routePolicyGuardMismatchTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_route_policy_guard_mismatch_total",
			Help: "Requests chi dispatched to a different route (or 404/405 fallback) than the one the outer layer resolved and the middleware applied the policy of; answered 500 in declarative mode. Any increase is a resolver bug.",
		},
		[]string{"route"},
	)
)

func init() {
	metrics.MustRegister(httpRequestsTotal, httpRequestDuration, rateLimitExceededTotal, routePolicyMismatchTotal,
		routePolicyGuardMismatchTotal)
}

// recordRoutePolicyMismatch counts one shadow-mode disagreement between the
// declared route policy and the legacy path tables; route is the policy's label.
func recordRoutePolicyMismatch(route, decision string) {
	routePolicyMismatchTotal.WithLabelValues(route, decision).Inc()
}

// RoutePolicyMismatchCount returns pad_route_policy_mismatch_total for route
// and decision. Exposed for tests.
func RoutePolicyMismatchCount(route, decision string) float64 {
	return counterValue(routePolicyMismatchTotal.WithLabelValues(route, decision))
}

// recordRouteGuardMismatch counts one request chi dispatched elsewhere than
// the route its policy was resolved for; route is a policy label.
func recordRouteGuardMismatch(route string) {
	routePolicyGuardMismatchTotal.WithLabelValues(route).Inc()
}

// RouteGuardMismatchCount returns pad_route_policy_guard_mismatch_total for
// route. Exposed for tests.
func RouteGuardMismatchCount(route string) float64 {
	return counterValue(routePolicyGuardMismatchTotal.WithLabelValues(route))
}

func counterValue(c prometheus.Counter) float64 {
	m := &dto.Metric{}
	if err := c.Write(m); err != nil || m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}
