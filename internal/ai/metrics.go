package ai

import (
	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// The AI providers' metrics, registered from this package.
var (
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
	usageDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_usage_dropped_total",
			Help: "AI usage metrics not recorded and dropped to bound goroutines, by reason: saturated (the wrapper's own slots are busy) or runner (the shared usage runner refused: at its cap or draining; pad_background_dropped_total{task=\"ai_usage_record\"} says which).",
		},
		[]string{"reason"},
	)
	// aiBudgetRefusalsTotal counts the calls the pre-call hooks refused: the budget checker in the audited
	// wrapper (call chat/stream/embed, reason exceeded/unavailable) and the provider policy at the factory
	// (call build, reason not_allowed). Every label is a closed set; the provider is the registry id.
	aiBudgetRefusalsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_budget_refusals_total",
			Help: "AI provider calls refused before they were made, by provider, call (chat, stream, embed; build for a provider the policy refused at the factory) and reason (exceeded, unavailable: the daily AI budget; not_allowed: the deployment or the organisation's allowed-provider list).",
		},
		[]string{"provider", "call", "reason"},
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
	// file search, MCP, …) — Azure bills these outside the token usage the
	// daily budget meters, so the gap becomes visible. The tool label is a
	// closed set (hostedToolLabels), never caller-supplied.
	aiHostedToolStepsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pad_ai_hosted_tool_steps_total",
			Help: "Provider-hosted agent tool steps completed (web search, MCP, code interpreter, …), by provider and tool type. Not priced: Azure bills these outside the token usage the daily budget meters.",
		},
		[]string{"provider", "tool"},
	)
)

func init() {
	metrics.MustRegister(circuitBreakerTransitionsTotal, aiTokensTotal, aiRequestDuration, aiRequestErrorsTotal,
		usageDroppedTotal, aiPricingFallbackTotal, aiUsageCostTotal, aiRetryAttemptsTotal, aiHostedToolStepsTotal,
		aiBudgetRefusalsTotal)
}

// recordCircuitBreakerTransition bumps the transition counter for an AI provider
// circuit breaker. state is "open", "half-open", or "closed".
func recordCircuitBreakerTransition(provider, state string) {
	circuitBreakerTransitionsTotal.WithLabelValues(provider, state).Inc()
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

// recordAIError increments the error counter for a failed AI request.
func recordAIError(provider string) {
	aiRequestErrorsTotal.WithLabelValues(provider).Inc()
}

// recordAIUsageCost adds estimated AI spend (USD) to the global cost counter;
// empty user/org (local/unauth) aggregates under the empty label. Per-tenant
// attribution is a usage_metrics query.
func recordAIUsageCost(cost float64) {
	aiUsageCostTotal.Add(cost)
}

// recordUsageDropped bumps the usage_dropped_total counter for an AI usage
// metric that could not be recorded (recorder saturated): dropping bounds
// in-flight recording goroutines so a stalled DB recorder can't exhaust
// memory; the counter surfaces the loss.
func recordUsageDropped(reason string) {
	usageDroppedTotal.WithLabelValues(reason).Inc()
}

// recordBudgetRefusal counts one call a pre-call hook refused. call and reason must be closed-set strings
// (see aiBudgetRefusalsTotal), never error text.
func recordBudgetRefusal(provider, call, reason string) {
	aiBudgetRefusalsTotal.WithLabelValues(provider, call, reason).Inc()
}

// recordPricingFallback bumps ai_pricing_fallback_total when a model isn't in
// the pricing catalog and usage was costed from the provider's default pricing
// instead; a sustained count for a provider means the catalog has drifted
// behind its model lineup.
func recordPricingFallback(provider, model string) {
	aiPricingFallbackTotal.WithLabelValues(provider, model).Inc()
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
