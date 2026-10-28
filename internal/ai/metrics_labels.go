package ai

import (
	"context"
	"errors"
	"io"
	"slices"
	"sort"
)

// The closed label sets of the provider metrics (R-observability-4, R-observability-14): every recorder maps its value
// through closedLabel, so no series is minted outside them (TestAIMetrics_LabelValuesAreClosedSets).

// metricLabelOther is the label value of anything outside a closed set.
const metricLabelOther = "other"

// --- ai_request_errors_total{class} ---

const aiErrorOther = metricLabelOther

// aiErrorClasses is the closed class label of ai_request_errors_total.
var aiErrorClasses = []string{"rate_limited", "provider_down", "auth", "permission", "context_limit", "content_filtered",
	"output_limit", "invalid_request", "config", "transport", "truncated", aiErrorOther}

// aiErrorClass names the class of a provider error from the package's sentinels. The order matters where an error
// matches several: an access or permission refusal also matches ErrApiKeyInvalid, and a transport failure given
// ErrProviderDown semantics (asProviderDown) is still a transport failure.
func aiErrorClass(err error) string {
	switch {
	case errors.Is(err, ErrContentFiltered):
		return "content_filtered"
	case errors.Is(err, ErrContextLimit):
		return "context_limit"
	case errors.Is(err, ErrOutputLimit):
		return "output_limit"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrProviderPermissionDenied), errors.Is(err, ErrProviderAccessDenied), errors.Is(err, ErrInsufficientBalance):
		return "permission"
	case errors.Is(err, ErrApiKeyInvalid), errors.Is(err, ErrKeyNotConfigured), errors.Is(err, ErrEntraTokenUnavailable):
		return "auth"
	case errors.Is(err, ErrToolsUnsupported), errors.Is(err, ErrThoughtSignatureMissing):
		// The model refuses this request as sent (tools on a model without them, a history without Gemini's
		// thought signatures): retrying it unchanged fails again.
		return "invalid_request"
	case errors.Is(err, ErrProviderConfigInvalid), errors.Is(err, ErrEmbeddingDeploymentMissing),
		errors.Is(err, ErrAzureEndpointNotFound), errors.Is(err, ErrAzureProjectNotFound),
		errors.Is(err, ErrAzureAgentNotFound), errors.Is(err, ErrAzureAgentNotRunnable):
		// The configuration names something the provider cannot serve: the user's or the admin's to fix.
		return "config"
	case isTransportError(err):
		return "transport"
	case errors.Is(err, ErrProviderDown), errors.Is(err, ErrCircuitOpen):
		return "provider_down"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "truncated"
	}
	return aiErrorOther
}

// isProviderAnswer reports an error that is the provider's own answer (every sentinel has a class of its own), which
// counts as the provider's even when the caller's context has ended meanwhile; a transport, truncation or
// unclassified failure with the context done is the cancellation surfacing.
func isProviderAnswer(err error) bool {
	switch aiErrorClass(err) {
	case "transport", "truncated", aiErrorOther:
		return false
	}
	return true
}

// --- pad_ai_requests_cancelled_total{reason} ---

// CancelReason is why a caller ended an AI call before the provider answered: the reason label of
// pad_ai_requests_cancelled_total. A caller that cancels deliberately says why with CancelCause; an expired deadline
// is CancelDeadline and any other cancellation CancelClient.
type CancelReason string

const (
	// CancelClient: the user stopped the answer, or the request that made the call was abandoned.
	CancelClient CancelReason = "client"
	// CancelIdle: the stream watchdog's idle limit (STREAM_IDLE_TIMEOUT).
	CancelIdle CancelReason = "idle"
	// CancelSubscriberGone: the client disconnected while the stream ran (STREAM_CLIENT_GONE).
	CancelSubscriberGone CancelReason = "subscriber_gone"
	// CancelShutdown: the server stopped the stream (SERVER_RESTARTING, or a replica fenced off).
	CancelShutdown CancelReason = "shutdown"
	// CancelDeadline: a deadline expired (the stream's wall clock, the knowledge lookup's query embed).
	CancelDeadline CancelReason = "deadline"
)

// aiCancelReasons is the closed reason label of pad_ai_requests_cancelled_total.
func aiCancelReasons() []string {
	return []string{string(CancelClient), string(CancelIdle), string(CancelSubscriberGone), string(CancelShutdown), string(CancelDeadline)}
}

// CancelCause is the cause to cancel an AI call's context with (context.WithCancelCause) so the cancellation is
// counted under reason. It matches context.Canceled.
func CancelCause(reason CancelReason) error { return &cancelCause{reason: reason} }

type cancelCause struct{ reason CancelReason }

func (c *cancelCause) Error() string { return "cancelled: " + string(c.reason) }

func (c *cancelCause) Is(target error) bool { return target == context.Canceled }

// CancelReasonOf is the reason a CancelCause error carries (context.Cause of a context cancelled with one); false for
// any other error.
func CancelReasonOf(err error) (CancelReason, bool) {
	var c *cancelCause
	if errors.As(err, &c) {
		return c.reason, true
	}
	return "", false
}

// callCancellation reports whether a call that ended in err (or, for a stream, ended without its terminal chunk)
// was ended by its caller, and why: err matches context.Canceled, or ctx is done and err is not the provider's own
// answer. The reason is the context's cancel cause (CancelCause), deadline for an expired deadline, client otherwise.
func callCancellation(ctx context.Context, err error) (CancelReason, bool) {
	if !errors.Is(err, context.Canceled) && (ctx.Err() == nil || (err != nil && isProviderAnswer(err))) {
		return "", false
	}
	cause := context.Cause(ctx)
	if reason, ok := CancelReasonOf(cause); ok {
		return reason, true
	}
	if errors.Is(cause, context.DeadlineExceeded) || (cause == nil && errors.Is(err, context.DeadlineExceeded)) {
		return CancelDeadline, true
	}
	return CancelClient, true
}

// --- pad_ai_param_repairs_total{param}, pad_ai_route_fallbacks_total{kind}, pad_ai_entra_token_events_total{kind} ---

// aiRepairParams is the closed param label of pad_ai_param_repairs_total: the repairable parameters and the
// tools-with-reasoning repair, sorted.
func aiRepairParams() []string {
	out := make([]string, 0, len(repairableParams)+1)
	for p := range repairableParams {
		out = append(out, p)
	}
	out = append(out, reasoningEffortNoneRepair)
	sort.Strings(out)
	return slices.Compact(out)
}

// The kinds of pad_ai_route_fallbacks_total.
const (
	fallbackStreamRefused     = "stream_refused"
	fallbackResponsesReroute  = "responses_reroute"
	fallbackResponsesRefused  = "responses_refused"
	fallbackReplayRebuild     = "replay_rebuild"
	fallbackEmbedSplit        = "embed_split"
	fallbackDimensionsDropped = "dimensions_dropped"
)

var aiRouteFallbackKinds = []string{fallbackStreamRefused, fallbackResponsesReroute, fallbackResponsesRefused,
	fallbackReplayRebuild, fallbackEmbedSplit, fallbackDimensionsDropped, metricLabelOther}

// The kinds of pad_ai_entra_token_events_total.
const (
	entraTokenRefreshed     = "refreshed"
	entraTokenRefreshFailed = "refresh_failed"
	entraTokenInvalidated   = "invalidated"
	entraTokenRejected      = "rejected"
)

var entraTokenEventKinds = []string{entraTokenRefreshed, entraTokenRefreshFailed, entraTokenInvalidated, entraTokenRejected, metricLabelOther}

// --- the provider label of the repair and fallback counters ---

const metricProviderOther = metricLabelOther

// metricProvider is the provider label of the repair and fallback counters: a registered provider's id, other for
// anything else (an unset id, a test's override).
func metricProvider(id string) string {
	providerMu.RLock()
	_, ok := providerSpecs[id]
	providerMu.RUnlock()
	if !ok {
		return metricProviderOther
	}
	return id
}
