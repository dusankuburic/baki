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
	dto "github.com/prometheus/client_model/go"
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
	rulesSkipped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_rules_skipped_total",
		Help: "Rule evaluations aborted via safeCheck's panic recovery (one buggy rule or malformed block); findings may be missing for the affected (block, rule) pairs.",
	})
	backgroundLoopTick = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_background_loop_tick_total",
			Help: "Iterations of each long-running periodic background loop (scanner, padcloud_ingest, retention_purge). Stops increasing ⇒ the loop has hung. Recommended alert: time() - pad_background_loop_last_tick_timestamp_seconds > 2 * expected_interval. Worker-pool loops (blob_cleaner, audit_pool) are not periodic — they're driven by enqueue — so are not labelled here.",
		},
		[]string{"loop"},
	)
	backgroundLoopLastTick = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pad_background_loop_last_tick_timestamp_seconds",
			Help: "Unix timestamp of the last completed iteration of each background loop. Stops updating ⇒ the loop has hung. Pair with pad_background_loop_tick_total to detect deadlocks /healthz cannot see.",
		},
		[]string{"loop"},
	)
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
	blobContentMissing = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pad_blob_content_missing_total",
			Help: "Flows whose content blob was absent though metadata recorded content — data loss, surfaced so ops can alert rather than only find a log line.",
		},
	)
	auditDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_audit_dropped_total",
			Help: "Audit events not persisted to the DB (pool full/closed) and diverted to the structured-log fallback sink, by reason.",
		},
		[]string{"reason"},
	)
	panicRecoveredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_panics_total",
			Help: "Panics recovered (HTTP handlers, background goroutines) forwarded to the error-reporting sink, by location.",
		},
		[]string{"location"},
	)
	errorsReportedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_errors_reported_total",
			Help: "Notable errors forwarded to the error-reporting sink for aggregation/triage, by location.",
		},
		[]string{"location"},
	)
	usageDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_usage_dropped_total",
			Help: "AI usage metrics not recorded (recorder saturated / queue full) and dropped to bound goroutines, by reason.",
		},
		[]string{"reason"},
	)
	aiPricingFallbackTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_pricing_fallback_total",
			Help: "AI usage priced from provider-default pricing because the model wasn't in the pricing catalog, by provider and model.",
		},
		[]string{"provider", "model"},
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
		rulesSkipped,
		backgroundLoopTick,
		backgroundLoopLastTick,
		wsConnectionsActive,
		flowOps,
		authOps,
		blobOps,
		blobOpDuration,
		blobContentMissing,
		auditDroppedTotal,
		panicRecoveredTotal,
		errorsReportedTotal,
		usageDroppedTotal,
		aiPricingFallbackTotal,
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

// RecordRulesSkipped adds n to the rules-skipped counter. n comes straight off
// AnalysisStats.RulesSkipped (set by runAnalysisCore after safeCheck recovers a
// panic). With no consumer the metric stayed invisible; this lets ops alert on
// "a rule is silently producing no findings for this flow".
func RecordRulesSkipped(n int) {
	if n > 0 {
		rulesSkipped.Add(float64(n))
	}
}

// RecordBackgroundLoopTick records a completed iteration of one of the long-
// running background loops (scanner, padcloud_ingest, retention_purge,
// blob_cleaner, audit_pool). The recommended alert is
//
//	time() - pad_background_loop_last_tick_timestamp_seconds{loop="X"} > 2 * expected_interval
//
// which catches deadlocks/hangs that /healthz (always 200) cannot see.
func RecordBackgroundLoopTick(name string) {
	backgroundLoopTick.WithLabelValues(name).Inc()
	backgroundLoopLastTick.WithLabelValues(name).SetToCurrentTime()
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

// RecordBlobContentMissing bumps the counter for a flow whose content blob was
// absent though its metadata recorded content — a data-loss signal ops can
// alert on. See pad_blob_content_missing_total.
func RecordBlobContentMissing() {
	blobContentMissing.Inc()
}

// RecordAuditDropped bumps the audit_dropped_total counter for an event that
// could not be persisted to the DB sink — reason "full"/"closed" when the
// enqueue buffer can't accept it, or "write_failed" when a (retried) DB write
// fails. Such events are diverted to the structured-log fallback sink so the
// data isn't silently lost, and this counter lets ops alert when the DB sink
// can't keep up.
func RecordAuditDropped(reason string) {
	auditDroppedTotal.WithLabelValues(reason).Inc()
}

// AuditDroppedCount returns the current audit_dropped_total value for a reason.
// Exposed for tests; safe for production use.
func AuditDroppedCount(reason string) float64 {
	m := &dto.Metric{}
	if err := auditDroppedTotal.WithLabelValues(reason).Write(m); err != nil {
		return 0
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// RecordPanic bumps the panics_total counter for a recovered panic at the given
// location (e.g. "http", "scanner", "audit"). Always recorded, even when no
// external error-aggregation sink (Sentry/App Insights) is configured, so
// recovered panics surface in the Prometheus/alert pipeline regardless.
func RecordPanic(location string) {
	panicRecoveredTotal.WithLabelValues(location).Inc()
}

// RecordError bumps the errors_reported_total counter for a notable error
// forwarded to the error-reporting sink from the given location.
func RecordError(location string) {
	errorsReportedTotal.WithLabelValues(location).Inc()
}

// RecordUsageDropped bumps the usage_dropped_total counter for an AI usage
// metric that could not be recorded (recorder saturated). Dropping bounds the
// number of in-flight recording goroutines so a stalled DB recorder can't
// exhaust memory under sustained AI traffic; the counter surfaces the loss so
// ops can alert on it.
func RecordUsageDropped(reason string) {
	usageDroppedTotal.WithLabelValues(reason).Inc()
}

// RecordPricingFallback bumps the ai_pricing_fallback_total counter when a
// model isn't in the pricing catalog and usage was costed from the provider's
// default pricing instead. A sustained/growing count for a given provider
// means the catalog has drifted behind that provider's model lineup and
// should be updated.
func RecordPricingFallback(provider, model string) {
	aiPricingFallbackTotal.WithLabelValues(provider, model).Inc()
}
