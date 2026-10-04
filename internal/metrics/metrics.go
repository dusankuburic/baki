// Package metrics owns the Prometheus registry and the shared metric symbols
// (HTTP middleware counters/histograms, chat-stream and SSE gauges, DB pool
// stats). Leaf package: service/api/middleware depend on it, it imports none
// of them, so a service-level counter lives beside HTTP counters in one
// registry without service importing api.
package metrics

import (
	"database/sql"
	"net/http"
	"strconv"
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
	// The three below mirror database/sql's cumulative DBStats counters as
	// gauges set on every snapshot (the pool owns the counters; a Prometheus
	// Counter could only be fed deltas). Monotonic for the life of the
	// process, so rate()/increase() apply. A rising wait count is pool
	// exhaustion; a rising closed{reason="max_idle"} is idle-cap churn.
	postgresPoolWaitCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_wait_count",
		Help: "Cumulative number of Postgres connection requests that had to wait for a free pooled connection (database/sql DBStats.WaitCount).",
	})
	postgresPoolWaitSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "postgres_pool_wait_duration_seconds",
		Help: "Cumulative time spent waiting for a free pooled Postgres connection (database/sql DBStats.WaitDuration).",
	})
	postgresPoolClosed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgres_pool_closed_connections",
		Help: "Cumulative Postgres connections closed by the pool, by reason: max_idle (idle cap exceeded on return), max_idle_time (ConnMaxIdleTime), max_lifetime (ConnMaxLifetime).",
	}, []string{"reason"})
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
			Help: "Iterations of each long-running periodic background loop (scanner, retention_purge). Stops increasing ⇒ the loop has hung. Recommended alert: time() - pad_background_loop_last_tick_timestamp_seconds > 2 * expected_interval. Worker-pool loops (blob_cleaner, audit_pool) are not periodic — they're driven by enqueue — so are not labelled here.",
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
	auditSpilledTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_audit_spilled_total",
		Help: "Audit events diverted to the on-disk spill queue because the in-memory pool was full. A reaper drains these back into the DB pool when capacity returns.",
	})
	auditSpillReplayedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_audit_spill_replayed_total",
		Help: "Audit events drained from the on-disk spill queue and re-enqueued to the DB pool.",
	})
	auditSpillDroppedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_audit_spill_dropped_total",
		Help: "Audit events dropped because the on-disk spill queue was at its size cap — only happens under sustained overload beyond the spill capacity.",
	})
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
	backgroundDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_background_dropped_total",
			Help: "Best-effort background tasks (analysis webhooks, finding emails) not started because their concurrency cap was reached or shutdown had begun, by task and reason.",
		},
		[]string{"task", "reason"},
	)
	usageDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_usage_dropped_total",
			Help: "AI usage metrics not recorded (recorder saturated / queue full) and dropped to bound goroutines, by reason.",
		},
		[]string{"reason"},
	)
	ragFallbackCappedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pad_rag_fallback_capped_total",
			Help: "Knowledge searches served by the Go-side ranking fallback at its 500-chunk sample cap — chunks beyond the deterministic sample are unsearchable; install pgvector or reduce the KB.",
		},
	)
	ragDimMismatchTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pad_rag_dim_mismatch_total",
			Help: "Knowledge searches that returned nothing while the org HAS chunks — corpus is stranded at a different embedding dimension than the configured one; re-index.",
		},
	)
	chatToolIterationsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pad_chat_tool_iterations_total",
			Help: "Tool-loop iterations executed (both native and marker-based loops). Sustained growth toward the iteration cap signals models looping without progress.",
		},
	)
	chatToolResultsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_chat_tool_results_total",
			Help: "Tool executions by tool and outcome (ok / error) — per-tool reliability and usage at a glance.",
		},
		[]string{"tool", "ok"},
	)
	chatFixDecisionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_chat_fix_decisions_total",
			Help: "apply_fix/apply_fixes approval outcomes by status — approval rate, timeouts and failures are observable.",
		},
		[]string{"status"},
	)
	librarySearchModeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_library_search_mode_total",
			Help: "Cross-library searches by strategy: pushdown (storage-indexed content match) vs scan (in-process, capped at the first 50 flows).",
		},
		[]string{"mode"},
	)
	chatRAGLookupsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_chat_rag_lookups_total",
			Help: "Per-turn knowledge-base guideline lookups by outcome (hit/miss/error/skipped) — RAG health without scraping logs.",
		},
		[]string{"outcome"},
	)
	aiPricingFallbackTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_pricing_fallback_total",
			Help: "AI usage priced from provider-default pricing because the model wasn't in the pricing catalog, by provider and model. The model label is a closed set: alias deployments collapse to \"deployment\" and unknown names to \"uncatalogued\" (see pricingMetricModel) — raw request models are client-chosen and would mint a series per name.",
		},
		[]string{"provider", "model"},
	)
	// Total AI spend. GLOBAL — deliberately unlabelled.
	//
	// A {user_id, org_id} label would mint one permanent series per (user,
	// org) pair that ever made an AI call (counters are never deleted): at a
	// few thousand users the scrape crosses its timeout and EVERY metric from
	// the process disappears, and it publishes the customer roster to anyone
	// with scrape access. Per-tenant attribution lives in usage_metrics
	// (audited.go writes UserID, OrgID and EstimatedCost per call).
	aiUsageCostTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pad_ai_usage_estimated_cost_total",
			Help: "Estimated USD cost of AI usage across the deployment. Per-tenant attribution lives in the usage_metrics table (see UsageMetric), which is queryable and does not cost a time series per user.",
		},
	)
	// aiRetryAttemptsTotal counts RETRIED provider attempts (second and later
	// tries), by provider and trigger condition — otherwise a retry storm and
	// backoff-inflated latency were invisible while PAD_RETRY_MAX_ATTEMPTS has
	// no upper bound. Both labels are closed sets (providerCtors registry; the
	// two retryable sentinels), so this cannot become a cardinality bomb.
	aiRetryAttemptsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_retry_attempts_total",
			Help: "Retried AI provider attempts (second and later tries), by provider and trigger.",
		},
		[]string{"provider", "reason"},
	)
	// aiHostedToolStepsTotal counts the provider-hosted agent tool steps an
	// Azure AI Foundry agent run completed (Bing grounding, code interpreter,
	// file search, MCP, …) — Azure bills these outside the token usage the
	// daily budget meters, so the gap becomes visible. The tool label is a
	// closed set (hostedToolLabels in the ai package), never caller-supplied.
	aiHostedToolStepsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_hosted_tool_steps_total",
			Help: "Provider-hosted agent tool steps completed (web search, MCP, code interpreter, …), by provider and tool type. Not priced: Azure bills these outside the token usage the daily budget meters.",
		},
		[]string{"provider", "tool"},
	)
	// Queue depths: surface backlog growth (bulk import, audit overflow) so it's
	// visible before it becomes a problem. Gauges (Set on each change/tick).
	blobCleanerPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_blob_cleaner_pending",
		Help: "Deferred blob-cleanup jobs waiting in the cleaner's pending heap. Sustained growth under bulk import/migration signals the worker pool can't keep up.",
	})
	auditSpillDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pad_audit_spill_depth",
		Help: "Audit events currently on the on-disk spill queue (overflow past the in-memory pool). Non-zero means the DB sink fell behind; pair with pad_audit_spill_replayed_total to confirm recovery.",
	})
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
		postgresPoolWaitCount,
		postgresPoolWaitSeconds,
		postgresPoolClosed,
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
		auditSpilledTotal,
		auditSpillReplayedTotal,
		auditSpillDroppedTotal,
		panicRecoveredTotal,
		errorsReportedTotal,
		usageDroppedTotal,
		backgroundDroppedTotal,
		aiPricingFallbackTotal,
		ragFallbackCappedTotal,
		ragDimMismatchTotal,
		chatToolIterationsTotal,
		librarySearchModeTotal,
		chatToolResultsTotal,
		chatFixDecisionsTotal,
		chatRAGLookupsTotal,
		aiUsageCostTotal,
		aiRetryAttemptsTotal,
		aiHostedToolStepsTotal,
		blobCleanerPending,
		auditSpillDepth,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return r
}()

// Handler returns an http.Handler that serves /metrics in the Prometheus
// exposition format — reachable in production only from the metrics scraper
// (gate at the load balancer or use a private listener if needed).
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		Registry:          registry,
	})
}

// ObservePostgresPool snapshots db.Stats() into the gauge metrics; call
// periodically from main's background goroutine (cheap: atomic reads).
func ObservePostgresPool(db *sql.DB) {
	if db == nil {
		return
	}
	observePoolStats(db.Stats())
}

// Pool-closure reasons: the complete, fixed label set of
// postgres_pool_closed_connections. Every snapshot sets all three.
const (
	poolClosedMaxIdle     = "max_idle"
	poolClosedMaxIdleTime = "max_idle_time"
	poolClosedMaxLifetime = "max_lifetime"
)

// observePoolStats records one DBStats snapshot. Split from
// ObservePostgresPool so it is testable without a live pool.
func observePoolStats(s sql.DBStats) {
	postgresPoolInUse.Set(float64(s.InUse))
	postgresPoolIdle.Set(float64(s.Idle))
	postgresPoolOpen.Set(float64(s.OpenConnections))
	postgresPoolWaitCount.Set(float64(s.WaitCount))
	postgresPoolWaitSeconds.Set(s.WaitDuration.Seconds())
	postgresPoolClosed.WithLabelValues(poolClosedMaxIdle).Set(float64(s.MaxIdleClosed))
	postgresPoolClosed.WithLabelValues(poolClosedMaxIdleTime).Set(float64(s.MaxIdleTimeClosed))
	postgresPoolClosed.WithLabelValues(poolClosedMaxLifetime).Set(float64(s.MaxLifetimeClosed))
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

// ObserveAIRequest records the latency of an AI request (full Chat call or a
// Stream's lifetime).
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

// RecordRulesSkipped adds n (straight off AnalysisStats.RulesSkipped, set after
// safeCheck recovers a panic) to the rules-skipped counter, so ops can alert on
// "a rule is silently producing no findings for this flow".
func RecordRulesSkipped(n int) {
	if n > 0 {
		rulesSkipped.Add(float64(n))
	}
}

// RecordBackgroundLoopTick records a completed iteration of a long-running
// background loop (scanner, retention_purge, …). Recommended alert:
// time() - pad_background_loop_last_tick_timestamp_seconds{loop="X"} >
// 2 * expected_interval — catches hangs that /healthz (always 200) cannot see.
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
// operation (op "upload"/"download"/"delete"/"list"; status "ok"/"not_found"/
// "throttled"/"error") — surfaces blob latency, 404s (potential data loss)
// and 429 throttling.
func RecordBlobOp(op, status string, dur time.Duration) {
	blobOps.WithLabelValues(op, status).Inc()
	blobOpDuration.WithLabelValues(op).Observe(dur.Seconds())
}

// RecordBlobContentMissing bumps the counter for a flow whose content blob was
// absent though its metadata recorded content — a data-loss signal. See
// pad_blob_content_missing_total.
func RecordBlobContentMissing() {
	blobContentMissing.Inc()
}

// RecordAuditDropped bumps the audit_dropped_total counter for an event that
// could not be persisted to the DB sink — "full"/"closed" when the enqueue
// buffer can't accept it, "write_failed" when a (retried) DB write fails. The
// event is diverted to the structured-log fallback sink; the counter surfaces
// when the DB sink can't keep up.
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

// RecordAuditSpilled bumps pad_audit_spilled_total — an event went to the
// on-disk spill queue because the in-memory pool was full.
func RecordAuditSpilled() {
	auditSpilledTotal.Inc()
}

// RecordAuditSpillReplayed bumps pad_audit_spill_replayed_total — a spilled
// event was drained back into the DB pool.
func RecordAuditSpillReplayed() {
	auditSpillReplayedTotal.Inc()
}

// RecordAuditSpillDropped bumps pad_audit_spill_dropped_total — the spill
// queue was at capacity and could not absorb the event.
func RecordAuditSpillDropped() {
	auditSpillDroppedTotal.Inc()
}

// AuditSpillDroppedCount returns the current pad_audit_spill_dropped_total
// value. Exposed for tests; safe for production use.
func AuditSpillDroppedCount() float64 {
	m := &dto.Metric{}
	if err := auditSpillDroppedTotal.Write(m); err != nil {
		return 0
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// AuditSpilledCount returns the current pad_audit_spilled_total value.
// Exposed for tests; safe for production use.
func AuditSpilledCount() float64 {
	m := &dto.Metric{}
	if err := auditSpilledTotal.Write(m); err != nil {
		return 0
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// RecordAIUsageCost adds estimated AI spend (USD) to the global cost counter;
// empty user/org (local/unauth) aggregates under the empty label. Per-tenant
// attribution is a usage_metrics query.
func RecordAIUsageCost(cost float64) {
	aiUsageCostTotal.Add(cost)
}

// SetBlobCleanerPending sets the blob-cleaner pending-heap depth gauge.
func SetBlobCleanerPending(n int) {
	blobCleanerPending.Set(float64(n))
}

// SetAuditSpillDepth sets the on-disk audit spill queue depth gauge.
func SetAuditSpillDepth(n int) {
	auditSpillDepth.Set(float64(n))
}

// AuditSpillDepthCount returns the current pad_audit_spill_depth value.
// Exposed for tests; safe for production use.
func AuditSpillDepthCount() float64 {
	m := &dto.Metric{}
	if err := auditSpillDepth.Write(m); err != nil {
		return 0
	}
	if m.Gauge == nil {
		return 0
	}
	return m.Gauge.GetValue()
}

// RecordPanic bumps the panics_total counter for a recovered panic at the given
// location (e.g. "http", "scanner", "audit"). Always recorded, even without an
// external error-aggregation sink, so recovered panics surface in the
// Prometheus/alert pipeline regardless.
func RecordPanic(location string) {
	panicRecoveredTotal.WithLabelValues(location).Inc()
}

// RecordError bumps the errors_reported_total counter for a notable error
// forwarded to the error-reporting sink from the given location.
func RecordError(location string) {
	errorsReportedTotal.WithLabelValues(location).Inc()
}

// RecordRAGFallbackCapped bumps rag_fallback_capped_total: a knowledge search
// ran on the Go-side fallback at its sample cap, so results are lossy here.
func RecordRAGFallbackCapped() {
	ragFallbackCappedTotal.Inc()
}

// RecordRAGDimMismatch bumps rag_dim_mismatch_total: a knowledge search found
// nothing while the org has chunks — the corpus is stranded at a different
// embedding dimension than the query's.
func RecordRAGDimMismatch() {
	ragDimMismatchTotal.Inc()
}

// RecordLibrarySearch bumps pad_library_search_mode_total with the strategy
// a cross-library search used (pushdown | scan).
func RecordLibrarySearch(mode string) {
	librarySearchModeTotal.WithLabelValues(mode).Inc()
}

// RecordChatToolIteration bumps pad_chat_tool_iterations_total: one tool-loop
// iteration ran (model turn + tool executions).
func RecordChatToolIteration() {
	chatToolIterationsTotal.Inc()
}

// RecordChatToolResult bumps pad_chat_tool_results_total for one tool
// execution outcome.
func RecordChatToolResult(tool string, ok bool) {
	chatToolResultsTotal.WithLabelValues(tool, strconv.FormatBool(ok)).Inc()
}

// RecordChatFixDecision bumps pad_chat_fix_decisions_total for one approval
// outcome (applied / applied-unresolved / declined / timeout / error).
func RecordChatFixDecision(status string) {
	chatFixDecisionsTotal.WithLabelValues(status).Inc()
}

// RecordRAGLookup bumps pad_chat_rag_lookups_total for one per-turn
// guidelines lookup outcome.
func RecordRAGLookup(outcome string) {
	chatRAGLookupsTotal.WithLabelValues(outcome).Inc()
}

// RecordUsageDropped bumps the usage_dropped_total counter for an AI usage
// metric that could not be recorded (recorder saturated): dropping bounds
// in-flight recording goroutines so a stalled DB recorder can't exhaust
// memory; the counter surfaces the loss.
func RecordUsageDropped(reason string) {
	usageDroppedTotal.WithLabelValues(reason).Inc()
}

// RecordBackgroundDropped counts a best-effort background task that was not
// started; task is the runner's code-literal name, reason "saturated" or
// "closed" — both closed sets, never request data.
func RecordBackgroundDropped(task, reason string) {
	backgroundDroppedTotal.WithLabelValues(task, reason).Inc()
}

// BackgroundDroppedCount returns the current pad_background_dropped_total
// value for a task and reason. Exposed for tests; safe for production use.
func BackgroundDroppedCount(task, reason string) float64 {
	m := &dto.Metric{}
	if err := backgroundDroppedTotal.WithLabelValues(task, reason).Write(m); err != nil {
		return 0
	}
	if m.Counter == nil {
		return 0
	}
	return m.Counter.GetValue()
}

// RecordPricingFallback bumps ai_pricing_fallback_total when a model isn't in
// the pricing catalog and usage was costed from the provider's default pricing
// instead; a sustained count for a provider means the catalog has drifted
// behind its model lineup.
func RecordPricingFallback(provider, model string) {
	aiPricingFallbackTotal.WithLabelValues(provider, model).Inc()
}

// RecordAIRetryAttempt counts one retried provider attempt. reason must be a
// closed-set string ("rate_limited" or "provider_down"), never raw error text —
// error strings carry provider detail and would make this unbounded.
func RecordAIRetryAttempt(provider, reason string) {
	aiRetryAttemptsTotal.WithLabelValues(provider, reason).Inc()
}

// RecordAIHostedToolStep counts one completed provider-hosted agent tool step.
// tool must be a closed-set item type (the hosted tool kinds the ai package
// labels), never raw provider text.
func RecordAIHostedToolStep(provider, tool string) {
	aiHostedToolStepsTotal.WithLabelValues(provider, tool).Inc()
}

// Gather returns the current metric families from the package registry —
// exported so packages can assert their own instrumentation end to end
// through the real registry (catches a metric defined but never registered,
// or registered but never incremented).
func Gather() ([]*dto.MetricFamily, error) {
	return registry.Gather()
}
