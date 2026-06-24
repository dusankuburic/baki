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
	"time"

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
	aiTokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ai_tokens_total",
			Help: "Total AI tokens processed, by provider and direction (input/output).",
		},
		[]string{"provider", "direction"},
	)
	aiRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ai_request_duration_seconds",
			Help:    "AI provider request latency (a full Chat call or the lifetime of a Stream), by provider.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"provider"},
	)
	aiRequestErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ai_request_errors_total",
			Help: "AI provider requests that ended in an error, by provider.",
		},
		[]string{"provider"},
	)
	analysisRuns = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_analysis_runs_total",
		Help: "Number of analysis runs",
	})
	wsConnectionsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_ws_connections_active",
		Help: "Number of active WebSocket connections",
	})
	flowOps = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_flow_operations_total",
			Help: "Number of flow operations by type",
		},
		[]string{"op"},
	)
	authOps = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_auth_operations_total",
			Help: "Number of auth operations by type",
		},
		[]string{"op"},
	)
	blobOps = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_blob_operations_total",
			Help: "Number of Azure Blob Storage operations by type and outcome",
		},
		[]string{"op", "status"},
	)
	blobOpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pad_blob_operation_duration_seconds",
			Help:    "Duration of Azure Blob Storage operations by type",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"op"},
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
		aiTokensTotal,
		aiRequestDuration,
		aiRequestErrorsTotal,
		analysisRuns,
		wsConnectionsActive,
		flowOps,
		authOps,
		blobOps,
		blobOpDuration,
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

// AI request metrics (called from the audited AI provider wrapper).

// RecordAITokens adds input/output token counts for a completed AI request.
// Zero-valued directions are skipped so empty results don't create noise.
func RecordAITokens(provider string, input, output int) {
	if input > 0 {
		aiTokensTotal.WithLabelValues(provider, "input").Add(float64(input))
	}
	if output > 0 {
		aiTokensTotal.WithLabelValues(provider, "output").Add(float64(output))
	}
}

// ObserveAIRequest records the latency of an AI request (full Chat call or the
// lifetime of a Stream).
func ObserveAIRequest(provider string, seconds float64) {
	aiRequestDuration.WithLabelValues(provider).Observe(seconds)
}

// RecordAIError increments the error counter for a failed AI request.
func RecordAIError(provider string) {
	aiRequestErrorsTotal.WithLabelValues(provider).Inc()
}

// RecordAnalysisRun increments the analysis-runs counter. Called at the start
// of every AnalyzeFlow run.
func RecordAnalysisRun() {
	analysisRuns.Inc()
}

// RecordWSConnectionChange adjusts the active-WebSocket-connections gauge by
// delta (+1 on join, -1 on leave).
func RecordWSConnectionChange(delta int64) {
	wsConnectionsActive.Add(float64(delta))
}

// RecordFlowOp increments the flow-operations counter for the given op type
// (e.g. "upload", "load_path", "load_folder").
func RecordFlowOp(op string) {
	flowOps.WithLabelValues(op).Inc()
}

// RecordAuthOp increments the auth-operations counter for the given op type
// (e.g. "login", "register", "logout", "refresh").
func RecordAuthOp(op string) {
	authOps.WithLabelValues(op).Inc()
}

// RecordBlobOp records the outcome and duration of an Azure Blob Storage
// operation. op is one of "upload", "download", "delete", "list"; status is one
// of "ok", "not_found", "throttled", "error". Surfaces blob latency, 404s
// (potential data loss), and 429 throttling to the Prometheus/Azure Monitor
// pipeline.
func RecordBlobOp(op, status string, dur time.Duration) {
	blobOps.WithLabelValues(op, status).Inc()
	blobOpDuration.WithLabelValues(op).Observe(dur.Seconds())
}
