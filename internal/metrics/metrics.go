// Package metrics owns the Prometheus registry and the metric symbols
// shared between HTTP middleware (request counters, latency histograms)
// and lower layers (chat-stream gauges, SSE client gauges, DB pool stats).
//
// Putting these in a leaf package keeps the dependency graph one-way:
// `service`, `api`, and `api/middleware` may all depend on `metrics`,
// but nothing in `metrics` imports from elsewhere — which lets a service-
// level counter (e.g. chat_stream_active) live alongside HTTP counters
// in the same registry without forcing `service` to import `api`.
package metrics

import (
	"database/sql"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests processed, labeled by method, normalized route, and status.",
		},
		[]string{"method", "route", "status"},
	)
	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by method and normalized route.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "route"},
	)
	postgresPoolInUse = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_in_use",
		Help: "Current Postgres connections checked out of the pool.",
	})
	postgresPoolIdle = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_idle",
		Help: "Current Postgres connections idle in the pool.",
	})
	postgresPoolOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_open",
		Help: "Current total Postgres connections (in use + idle).",
	})
	rateLimitExceededTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rate_limit_exceeded_total",
			Help: "Requests refused by the per-IP rate limiter, by endpoint group.",
		},
		[]string{"group"},
	)
	chatStreamActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "chat_stream_active",
		Help: "Currently in-flight AI chat streams.",
	})
	sseClientsConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sse_clients_connected",
		Help: "Currently connected Server-Sent Events subscribers.",
	})
	circuitBreakerTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ai_circuit_breaker_transitions_total",
			Help: "AI provider circuit-breaker state transitions, by provider and new state (open/half-open/closed).",
		},
		[]string{"provider", "state"},
	)
)

// registry is process-local. Tests are hermetic (no leftover series between
// runs); production deployments still get a clean snapshot at /metrics.
var registry = func() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(
		HTTPRequestsTotal,
		HTTPRequestDuration,
		postgresPoolInUse,
		postgresPoolIdle,
		postgresPoolOpen,
		rateLimitExceededTotal,
		chatStreamActive,
		sseClientsConnected,
		circuitBreakerTransitionsTotal,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return r
}()

// Handler returns an http.Handler that serves /metrics in the Prometheus
// exposition format. In production this should be reachable only from
// the metrics scraper — gate at the load balancer or move to a private
// listener if needed.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		Registry:          registry,
	})
}

// ObservePostgresPool snapshots db.Stats() into the gauge metrics. Call
// periodically from a background goroutine in main; cheap (atomic reads
// inside database/sql).
func ObservePostgresPool(db *sql.DB) {
	if db == nil {
		return
	}
	s := db.Stats()
	postgresPoolInUse.Set(float64(s.InUse))
	postgresPoolIdle.Set(float64(s.Idle))
	postgresPoolOpen.Set(float64(s.OpenConnections))
}

// RecordRateLimitExceeded bumps the rate_limit_exceeded_total counter
// for the given endpoint group ("general" or "auth").
func RecordRateLimitExceeded(group string) {
	rateLimitExceededTotal.WithLabelValues(group).Inc()
}

// Chat-stream gauges (called from service.ChatService).
func ChatStreamStart() { chatStreamActive.Inc() }
func ChatStreamEnd()   { chatStreamActive.Dec() }

// SSE-subscriber gauges (called from api.Router SSE handler).
func SSEClientStart() { sseClientsConnected.Inc() }
func SSEClientEnd()   { sseClientsConnected.Dec() }

// RecordCircuitBreakerTransition bumps the transition counter for an AI provider
// circuit breaker. state is "open", "half-open", or "closed".
func RecordCircuitBreakerTransition(provider, state string) {
	circuitBreakerTransitionsTotal.WithLabelValues(provider, state).Inc()
}
