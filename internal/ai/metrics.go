package ai

import (
	"slices"

	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// aiLatencyBuckets spans a fast embed to the stream's 10-minute wall clock (maxChatStreamDuration).
var aiLatencyBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600}

// The kinds of AI call pad_ai_call_duration_seconds tells apart.
const (
	aiCallChat   = "chat"
	aiCallStream = "stream"
	aiCallEmbed  = "embed"
)

// The AI providers' metrics, registered from this package.
var (
	circuitBreakerTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ai_circuit_breaker_transitions_total",
			Help: "AI provider circuit-breaker state transitions, by provider, model (a catalogued id, deployment for an alias provider, uncatalogued, or none for the provider-wide circuit), resource (an 8-character hash of a per-user endpoint, empty for shared upstreams) and new state (open/half-open/closed).",
		},
		[]string{"provider", "model", "resource", "state"},
	)
	// circuitBreakerOpen is the number of circuits refusing callers right now, by provider: "open for more than
	// N minutes" is an alert on it. Counted at scrape time (openCircuitCounts), so a circuit nobody calls again
	// stops counting when its cooldown ends instead of staying "open" for good.
	circuitBreakerOpen = &openCircuitsCollector{desc: prometheus.NewDesc("pad_ai_circuit_breaker_open",
		"AI provider circuits currently refusing callers (open within their cooldown, or half-open), by provider.",
		[]string{"provider"}, nil)}
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
	// aiCallDuration is the AI call latency in buckets that reach the stream's 10-minute wall clock (the old
	// ai_request_duration_seconds stops at 10 s, where every reasoning stream lands in +Inf). A new series rather
	// than a re-bucketed one: changing an existing histogram's buckets breaks range quantiles across the change.
	aiCallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pad_ai_call_duration_seconds",
			Help:    "AI provider call latency — a full Chat or Embed call, or the lifetime of a Stream with its retries — by provider and kind (chat, stream, embed).",
			Buckets: aiLatencyBuckets,
		},
		[]string{"provider", "kind"},
	)
	// aiFirstToken is a stream's time to its first content (text, reasoning or a tool call): what a user waits
	// before anything appears.
	aiFirstToken = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pad_ai_stream_first_token_seconds",
			Help:    "Time from a Stream call to its first content chunk (text, reasoning or tool calls), by provider.",
			Buckets: aiLatencyBuckets,
		},
		[]string{"provider"},
	)
	// aiRequestErrorsTotal counts the provider's failures only: a call its caller cancelled (the user's stop, a
	// watchdog, shutdown, a deadline) is aiRequestsCancelledTotal. class is a closed set (aiErrorClasses).
	aiRequestErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ai_request_errors_total",
			Help: "AI provider requests that ended in a provider error, by provider and class: rate_limited, provider_down, auth, permission, context_limit, content_filtered, output_limit, invalid_request (the model refuses the request as sent), config (the configuration names something the provider cannot serve), transport, truncated or other. A call its caller cancelled is pad_ai_requests_cancelled_total instead.",
		},
		[]string{"provider", "class"},
	)
	// aiRequestsCancelledTotal counts the calls their caller ended: reason is the closed CancelReason set, read from the
	// context's cancel cause (CancelCause), deadline for an expired deadline, client otherwise.
	aiRequestsCancelledTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_requests_cancelled_total",
			Help: "AI provider requests their caller ended before the provider answered, by provider and reason: client (the user stopped, or the request was abandoned), idle (the stream watchdog's idle limit), subscriber_gone (the client disconnected), shutdown (the server stopped the stream) or deadline (a deadline expired: the stream's wall clock, the knowledge lookup's query embed).",
		},
		[]string{"provider", "reason"},
	)
	// aiParamRepairsTotal counts the request parameters repaired after a rejection (R-observability-14): a fresh repair
	// only, never a remembered one pre-applied. provider is a registry id (metricProvider), param a closed set
	// (aiRepairParams).
	aiParamRepairsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_param_repairs_total",
			Help: "Request parameters an AI provider rejected and the request was resent without (or with the other max-tokens name), by provider and parameter; reasoning_effort=none is the tools-with-reasoning repair. Counted once per learned repair: a deployment that keeps counting is paying a rejection round trip every turn (see pad_cache_evictions_total{cache=\"ai.param-repairs\"}).",
		},
		[]string{"provider", "param"},
	)
	// aiRouteFallbacksTotal counts the requests the provider layer answered another way after a rejection.
	aiRouteFallbacksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_route_fallbacks_total",
			Help: "AI requests answered another way after an upstream rejection, by provider and kind: stream_refused (the model refuses streaming; answered non-streaming), responses_reroute (a tools turn moved to the Responses API), responses_refused (the Responses route refused the deployment; answered on chat completions), replay_rebuild (the replayed history rebuilt without provider items), embed_split (an embedding batch split in half) or dimensions_dropped (embeddings retried without dimensions).",
		},
		[]string{"provider", "kind"},
	)
	// entraTokenEventsTotal counts the Azure AI Foundry Entra ID token sources' events.
	entraTokenEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_entra_token_events_total",
			Help: "Microsoft Entra ID token events of the Azure AI Foundry token sources, by kind: refreshed (a token fetched), refresh_failed (a token request failed), rejected (the service refused a cached token that had not expired) or invalidated (such a token dropped, so the next call fetches a fresh one). A steady rejected rate is a resource refusing its tokens.",
		},
		[]string{"kind"},
	)
	usageDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_usage_dropped_total",
			Help: "AI usage metrics not recorded and dropped to bound goroutines, by reason: saturated (the wrapper's own slots are busy) or runner (the shared usage runner refused: at its cap or draining; pad_background_dropped_total{task=\"ai_usage_record\"} says which).",
		},
		[]string{"reason"},
	)
	// usageSaveFailedTotal counts usage rows whose save failed: retried (saved on the second try) or lost (both
	// tries failed — the row is gone and the usage views under-count).
	usageSaveFailedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_usage_save_failed_total",
			Help: "AI usage rows whose save failed, by outcome: retried (saved on the one retry) or lost (the retry failed too; the usage views under-count).",
		},
		[]string{"outcome"},
	)
	// aiPolicyRefusalsTotal counts the providers the provider policy refused to build: the deployment's or the
	// organisation's allowed-provider list. The provider label is the registry id, a closed set.
	aiPolicyRefusalsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_policy_refusals_total",
			Help: "AI providers the provider policy refused to build (the deployment's or the organisation's allowed-provider list), by provider.",
		},
		[]string{"provider"},
	)
	// aiRetryAttemptsTotal counts RETRIED provider attempts (second and later
	// tries), by provider and trigger condition — otherwise a retry storm and
	// backoff-inflated latency were invisible while PAD_RETRY_MAX_ATTEMPTS has
	// no upper bound. Both labels are closed sets (the provider spec registry; the
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
	// file search, MCP, …) — Azure bills these outside the token usage, so
	// they are counted here. The tool label is a closed set
	// (hostedToolLabels), never caller-supplied.
	aiHostedToolStepsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_hosted_tool_steps_total",
			Help: "Provider-hosted agent tool steps completed (web search, MCP, code interpreter, …), by provider and tool type. Azure bills these outside the token usage.",
		},
		[]string{"provider", "tool"},
	)
)

func init() {
	metrics.MustRegister(circuitBreakerTransitionsTotal, circuitBreakerOpen, aiTokensTotal, aiRequestDuration, aiCallDuration, aiFirstToken, aiRequestErrorsTotal,
		aiRequestsCancelledTotal, aiParamRepairsTotal, aiRouteFallbacksTotal, entraTokenEventsTotal,
		usageDroppedTotal, usageSaveFailedTotal, aiRetryAttemptsTotal, aiHostedToolStepsTotal, aiPolicyRefusalsTotal)
}

// recordCircuitBreakerTransition bumps the transition counter for an AI provider
// circuit breaker. state is "open", "half-open", or "closed".
func recordCircuitBreakerTransition(provider, model, resource, state string) {
	circuitBreakerTransitionsTotal.WithLabelValues(provider, model, resource, state).Inc()
}

// AI request metrics (called from the audited AI provider wrapper).

// recordAITokens adds input/output token counts for a completed AI request.
// Zero-valued directions are skipped so empty results don't create noise.
func recordAITokens(provider string, input, output int) {
	if input > 0 {
		aiTokensTotal.WithLabelValues(provider, "input").Add(float64(input))
	}
	if output > 0 {
		aiTokensTotal.WithLabelValues(provider, "output").Add(float64(output))
	}
}

// observeAIRequest records the latency of an AI request (full Chat call or a
// Stream's lifetime).
func observeAIRequest(provider string, seconds float64) {
	aiRequestDuration.WithLabelValues(provider).Observe(seconds)
}

// observeAICall records one call's latency in both histograms: the legacy one and the kind-labelled one.
func observeAICall(provider, kind string, seconds float64) {
	observeAIRequest(provider, seconds)
	aiCallDuration.WithLabelValues(provider, kind).Observe(seconds)
}

// recordAIError increments the error counter for a request the provider failed; class is one of aiErrorClasses
// (aiErrorClass), anything else counts as other.
func recordAIError(provider, class string) {
	aiRequestErrorsTotal.WithLabelValues(provider, closedLabel(class, aiErrorClasses, aiErrorOther)).Inc()
}

// recordAICancelled counts a request its caller ended; an unknown reason counts as client.
func recordAICancelled(provider string, reason CancelReason) {
	aiRequestsCancelledTotal.WithLabelValues(provider, closedLabel(string(reason), aiCancelReasons(), string(CancelClient))).Inc()
}

// recordParamRepair counts one fresh repair of param on a request to providerID (metricProvider).
func recordParamRepair(providerID, param string) {
	aiParamRepairsTotal.WithLabelValues(metricProvider(providerID), closedLabel(param, aiRepairParams(), metricLabelOther)).Inc()
}

// recordRouteFallback counts one request answered another way (aiRouteFallbackKinds).
func recordRouteFallback(providerID, kind string) {
	aiRouteFallbacksTotal.WithLabelValues(metricProvider(providerID), closedLabel(kind, aiRouteFallbackKinds, metricLabelOther)).Inc()
}

// recordEntraTokenEvent counts one Entra ID token event (entraTokenEventKinds).
func recordEntraTokenEvent(kind string) {
	entraTokenEventsTotal.WithLabelValues(closedLabel(kind, entraTokenEventKinds, metricLabelOther)).Inc()
}

// closedLabel is v when it is one of set, else fallback: a recorder never mints a value outside its closed set.
func closedLabel(v string, set []string, fallback string) string {
	if slices.Contains(set, v) {
		return v
	}
	return fallback
}

// recordUsageDropped bumps the usage_dropped_total counter for an AI usage
// metric that could not be recorded (recorder saturated): dropping bounds
// in-flight recording goroutines so a stalled DB recorder can't exhaust
// memory; the counter surfaces the loss.
func recordUsageDropped(reason string) {
	usageDroppedTotal.WithLabelValues(reason).Inc()
}

// recordPolicyRefusal counts one provider the provider policy refused to build.
func recordPolicyRefusal(provider string) {
	aiPolicyRefusalsTotal.WithLabelValues(provider).Inc()
}

// recordAIRetryAttempt counts one retried provider attempt. reason must be a
// closed-set string ("rate_limited" or "provider_down"), never raw error text —
// error strings carry provider detail and would make this unbounded.
func recordAIRetryAttempt(provider, reason string) {
	aiRetryAttemptsTotal.WithLabelValues(provider, reason).Inc()
}

// recordAIHostedToolStep counts one completed provider-hosted agent tool step.
// tool must be a closed-set item type (the hosted tool kinds this package
// labels), never raw provider text.
func recordAIHostedToolStep(provider, tool string) {
	aiHostedToolStepsTotal.WithLabelValues(provider, tool).Inc()
}

// openCircuitsCollector reports pad_ai_circuit_breaker_open for every registered provider (0 when none is open),
// from the breaker registry as it is at the scrape.
type openCircuitsCollector struct{ desc *prometheus.Desc }

func (c *openCircuitsCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *openCircuitsCollector) Collect(ch chan<- prometheus.Metric) {
	counts := openCircuitCounts()
	for _, spec := range Specs() {
		counts[spec.ID] += 0
	}
	for provider, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), provider)
	}
}
