package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/reqinfo"
	"pad-core/logger"
)

// MetricsHandler exposes the Prometheus /metrics endpoint; in production it should be reachable
// only from the metrics scraper — gate at the load balancer or use a private listener if needed.
func MetricsHandler() http.Handler { return metrics.Handler() }

// RecordRateLimitExceeded is wired from ratelimit.go when a request is refused; lives here for
// naming locality.
func RecordRateLimitExceeded(group string) { rateLimitExceededTotal.WithLabelValues(group).Inc() }

// Metrics wraps h to record http_requests_total and http_request_duration_seconds. Apply it inside AccessLog so the status label reflects the final response.
// The route label is the label of the policy resolved for the request at the outermost layer (RouteLabel), taken before the handler runs: the matched pattern of a route, one of a few fixed labels otherwise. It is bounded whatever the path, and it does not depend on whether the router got to rewrite the URL (a 429 is labelled like the request it refused).
func Metrics(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := RouteLabel(r)
		sr := NewResponseRecorder(w)
		start := time.Now()
		h.ServeHTTP(maybeHijackable(sr), r)
		method := normalizeMethod(r.Method)
		elapsed := time.Since(start).Seconds()
		httpRequestsTotal.WithLabelValues(method, route, strconv.Itoa(sr.status)).Inc()
		httpRequestDuration.WithLabelValues(method, route).Observe(elapsed)
	})
}

// RouteLabel is the route label Metrics records for r: the label of the policy resolved for it (the pattern, e.g.
// /api/library/{id}; /api/other, / or /static/* when no route matches), "unresolved" outside the router's chain.
func RouteLabel(r *http.Request) string {
	p, _ := RequestPolicy(r)
	return p.Label
}

// RequestPolicy returns the route policy resolved for r at the outermost layer, and false when none was (a layer
// running outside the router's chain), with the fail-closed route.Unresolved in its place.
func RequestPolicy(r *http.Request) (*route.Policy, bool) {
	if info := reqinfo.From(r.Context()); info != nil && info.Policy != nil {
		return info.Policy, true
	}
	return route.Unresolved, false
}

// methodOther is the bounded catch-all for any request method outside the
// standard set.
const methodOther = "OTHER"

// normalizeMethod bounds the "method" label: net/http accepts any token, and this layer runs before auth and the rate limiter, so verbatim r.Method let an
// unauthenticated FOO1 /, FOO2 /, … mint a fresh counter and histogram series per request, retained for the process's life.
func normalizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return m
	}
	return methodOther
}

// The decisions shadow mode compares between the declared route policy and the legacy path tables: the decision
// label of pad_route_policy_mismatch_total.
const (
	DecisionAuth      = "auth"      // jwtAuth authenticates the request
	DecisionScope     = "scope"     // a scoped API token may call the route
	DecisionRateLimit = "ratelimit" // the rate-limit bucket
	DecisionTx        = "tx"        // the per-request RLS transaction
	DecisionDeadline  = "deadline"  // the deadline class
)

// mismatchLogged holds the (route, decision) pairs already logged: one WARN line each per process, however often
// the disagreement recurs; the counter has the rate.
var mismatchLogged sync.Map

// ReportPolicyMismatch records that the policy declared on r's route (p) decides differently from the legacy path
// tables: it counts pad_route_policy_mismatch_total{route,decision} and logs the first occurrence of each pair.
func ReportPolicyMismatch(r *http.Request, p *route.Policy, decision, legacy, declared string) {
	recordRoutePolicyMismatch(p.Label, decision)
	if _, seen := mismatchLogged.LoadOrStore(p.Label+"\x00"+decision, struct{}{}); seen {
		return
	}
	logger.Warn("route policy: the declared policy disagrees with the legacy path tables (logged once per route and decision)",
		"route", p.Label, "decision", decision, "legacy", legacy, "declared", declared,
		"method", r.Method, "path", redactPath(canonicalPath(r.URL.Path)))
}

// ReportGuardMismatch records a request chi dispatched to a route (or 404/405 fallback) other than the one whose
// policy the outer layer resolved and the middleware applied: a resolver bug, logged at ERROR every time and
// counted in pad_route_policy_guard_mismatch_total{route}, where route is what chi dispatched to.
func ReportGuardMismatch(r *http.Request, resolved *route.Policy, dispatched string) {
	recordRouteGuardMismatch(dispatched)
	logger.Error("route policy: chi dispatched the request elsewhere than the route its policy was resolved for",
		"method", r.Method, "path", redactPath(canonicalPath(r.URL.Path)),
		"resolved", resolved.Label, "resolved_kind", resolved.Kind, "dispatched", dispatched)
}
