package service

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/rag"
)

// The services' metrics, registered from this package.
var (
	chatStreamActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "chat_stream_active",
		Help: "Currently in-flight AI chat streams.",
	})
	analysisRunsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_analysis_runs_total",
		Help: "Number of analysis runs",
	})
	rulesSkipped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pad_rules_skipped_total",
		Help: "Rule evaluations aborted via safeCheck's panic recovery (one buggy rule or malformed block); findings may be missing for the affected (block, rule) pairs.",
	})
	// Recorded by internal/api's handlers too, which import this package.
	flowOps = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_flow_operations_total",
			Help: "Number of flow operations by type",
		},
		[]string{"op"},
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
			Help: "apply_fix/apply_fixes approval outcomes by status (applying, applied, applied-unresolved, declined, timeout, cancelled, error) — approval rate, timeouts and failures are observable.",
		},
		[]string{"status"},
	)
	chatToolRefusedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_chat_tool_refused_total",
			Help: "Tool calls the session's gate refused before the handler, by tool and reason: scope (a scoped API token without the tool's scope), approval_unavailable (no approval applier in the session) or flow_permission (the caller may not edit the flow). The model was not offered the tool and called it anyway.",
		},
		[]string{"tool", "reason"},
	)
	ciWebhookAuthTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ci_webhook_auth_total",
			Help: "CI webhook calls (POST /api/integrations/ci) that authenticated, by how: key_id (an org CI credential named by X-Baki-Key-Id), global (the deprecated PAD_CI_WEBHOOK_SECRET), flow_org (an org CI credential found through the flow, for bakicli releases that send no key id) or token (a personal API token with the ci scope). global and flow_org should fall to zero before their support is removed.",
		},
		[]string{"method"},
	)
	ciWebhookDeliveriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ci_webhook_deliveries_total",
			Help: "Authenticated CI webhook calls by their deliveryId: new (recorded; the call runs), replayed (already recorded for the same key: 409 CI_REPLAYED, nothing runs) or missing (none sent, a bakicli release before it; accepted). Once missing stays at zero the delivery id can be required.",
		},
		[]string{"delivery"},
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
			Help: "Knowledge-base lookups of the chat by source (turn: the guidelines a turn fetches before it starts; tool: the model's search_knowledge calls; preview: a context preview's lookup) and outcome — RAG health without scraping logs: hit, miss, non_member (the caller is not in the flow's organisation), trivial (a short question with nothing selected; nothing embedded), no_embedder (the caller has no embedding provider), identity_mismatch (the caller's embedding model sees none of the corpus; nothing embedded), deadline (the turn's PAD_RAG_DEADLINE_MS, or the tool's own timeout, ran out) or error.",
		},
		[]string{"source", "outcome"},
	)
	chatRAGLookupSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pad_chat_rag_lookup_seconds",
		Help:    "Time a chat turn's knowledge-base lookup spent per phase: resolve (the corpus count, the caller's embedder, the identity census), embed (the query embedding) and search (the vector and lexical statements). Read beside pad_chat_rag_lookups_total{outcome=\"deadline\"} to see where PAD_RAG_DEADLINE_MS goes.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.4, 0.8, 1.5, 3, 10},
	}, []string{"phase"})
	chatTurnsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_chat_turns_total",
			Help: "Chat turns by how they ended, once per stream: done, the lowercased failure code the client saw (provider_rate_limited, stream_timeout, server_restarting, ...), stopped (the user's stop, STREAM_STOPPED), idle_timeout (no progress for the idle limit, STREAM_IDLE_TIMEOUT: the log line names the phase), subscriber_gone (the client's connection went away, STREAM_CLIENT_GONE) or error (unclassified). Each turn also logs one \"chat turn ended\" line.",
		},
		[]string{"outcome"},
	)
	chatTurnDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "pad_chat_turn_duration_seconds",
		Help:    "Wall-clock duration of a chat turn, from its start to its terminal event (tool loops and approval waits included); the top bucket covers the stream's 10-minute cap.",
		Buckets: chatTurnBuckets,
	})
	chatToolDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pad_chat_tool_duration_seconds",
		Help:    "Duration of one tool execution in the chat's tool loop, by tool. An approval tool's includes the user's decision (pad_chat_approval_wait_seconds isolates it) and the apply.",
		Buckets: []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 120, 300},
	}, []string{"tool"})
	chatApprovalWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pad_chat_approval_wait_seconds",
		Help:    "Time a fix approval card waited for the user, by how it was resolved: approved, declined, timeout or cancelled (the stream ended).",
		Buckets: []float64{1, 2.5, 5, 10, 20, 30, 60, 120, 300},
	}, []string{"status"})
)

// chatTurnBuckets reach past maxChatStreamDuration (10 minutes): a turn cut by the cap lands in a finite bucket.
var chatTurnBuckets = []float64{0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300, 600, 900}

func init() {
	metrics.MustRegister(chatStreamActive, analysisRunsTotal, rulesSkipped, flowOps, chatToolIterationsTotal,
		chatToolResultsTotal, chatFixDecisionsTotal, chatToolRefusedTotal, ciWebhookAuthTotal, ciWebhookDeliveriesTotal,
		librarySearchModeTotal, chatRAGLookupsTotal, chatRAGLookupSeconds, chatTurnsTotal, chatTurnDuration, chatToolDuration, chatApprovalWait)
	for _, source := range []string{ragSourceTurn, ragSourceTool, ragSourcePreview} {
		for _, outcome := range ragLookupOutcomes() {
			chatRAGLookupsTotal.WithLabelValues(source, outcome)
		}
	}
	for _, phase := range []string{rag.PhaseResolve, rag.PhaseEmbed, rag.PhaseSearch} {
		chatRAGLookupSeconds.WithLabelValues(phase)
	}
	for _, outcome := range chatTurnOutcomes() {
		chatTurnsTotal.WithLabelValues(outcome)
	}
	for _, status := range []string{"approved", "declined", "timeout", "cancelled"} {
		chatApprovalWait.WithLabelValues(status)
	}
}

// Chat-stream gauges (called from ChatService).
func chatStreamStart() { chatStreamActive.Inc() }
func chatStreamEnd()   { chatStreamActive.Dec() }

// recordAnalysisRun increments the analysis-runs counter. Called at the start
// of every AnalyzeFlow run.
func recordAnalysisRun() {
	analysisRunsTotal.Inc()
}

// recordRulesSkipped adds n (straight off AnalysisStats.RulesSkipped, set after
// safeCheck recovers a panic) to the rules-skipped counter, so ops can alert on
// "a rule is silently producing no findings for this flow".
func recordRulesSkipped(n int) {
	if n > 0 {
		rulesSkipped.Add(float64(n))
	}
}

// RecordFlowOp increments the flow-operations counter for the given op type
// (e.g. "upload", "load_path", "load_folder").
func RecordFlowOp(op string) {
	flowOps.WithLabelValues(op).Inc()
}

// recordLibrarySearch bumps pad_library_search_mode_total with the strategy
// a cross-library search used (pushdown | scan).
func recordLibrarySearch(mode string) {
	librarySearchModeTotal.WithLabelValues(mode).Inc()
}

// recordChatToolIteration bumps pad_chat_tool_iterations_total: one tool-loop
// iteration ran (model turn + tool executions).
func recordChatToolIteration() {
	chatToolIterationsTotal.Inc()
}

// recordChatToolResult bumps pad_chat_tool_results_total for one tool
// execution outcome.
func recordChatToolResult(tool string, ok bool) {
	chatToolResultsTotal.WithLabelValues(tool, strconv.FormatBool(ok)).Inc()
}

// recordChatFixDecision bumps pad_chat_fix_decisions_total for one approval
// outcome (applied / applied-unresolved / declined / timeout / cancelled / error).
func recordChatFixDecision(status string) {
	chatFixDecisionsTotal.WithLabelValues(status).Inc()
}

// recordChatToolRefused bumps pad_chat_tool_refused_total for one tool call the
// session's gate refused (tool is a registered name; reason an ai.ToolRefused* value).
func recordChatToolRefused(tool, reason string) {
	chatToolRefusedTotal.WithLabelValues(tool, reason).Inc()
}

// The ways a CI webhook call authenticates, the label values of pad_ci_webhook_auth_total.
const (
	CIAuthKeyID   = "key_id"
	CIAuthGlobal  = "global"
	CIAuthFlowOrg = "flow_org"
	CIAuthToken   = "token"
)

// recordCIWebhookAuth counts one authenticated CI webhook call by method (CIAuthKeyID, CIAuthGlobal, CIAuthFlowOrg,
// CIAuthToken).
func recordCIWebhookAuth(method string) {
	ciWebhookAuthTotal.WithLabelValues(method).Inc()
}

// What an authenticated CI webhook call's delivery id was, the label values of pad_ci_webhook_deliveries_total.
const (
	CIDeliveryNew      = "new"
	CIDeliveryReplayed = "replayed"
	CIDeliveryMissing  = "missing"
)

// recordCIWebhookDelivery counts one authenticated CI webhook call by its delivery id (CIDeliveryNew,
// CIDeliveryReplayed, CIDeliveryMissing).
func recordCIWebhookDelivery(outcome string) {
	ciWebhookDeliveriesTotal.WithLabelValues(outcome).Inc()
}

// recordRAGLookup bumps pad_chat_rag_lookups_total for one knowledge-base lookup of source (ragSourceTurn,
// ragSourceTool) with its outcome (ragLookupOutcomes).
func recordRAGLookup(source, outcome string) {
	chatRAGLookupsTotal.WithLabelValues(source, outcome).Inc()
}

// recordRAGPhases observes the phases a knowledge search ran in pad_chat_rag_lookup_seconds.
func recordRAGPhases(phases []rag.SearchPhase) {
	for _, p := range phases {
		chatRAGLookupSeconds.WithLabelValues(p.Name).Observe(p.Took.Seconds())
	}
}
