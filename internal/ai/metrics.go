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
	metrics.MustRegister(circuitBreakerTransitionsTotal, aiTokensTotal, aiRequestDuration, aiRequestErrorsTotal,
		usageDroppedTotal, aiRetryAttemptsTotal, aiHostedToolStepsTotal, aiPolicyRefusalsTotal)
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
