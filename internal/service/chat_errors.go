package service

import (
	"context"
	"errors"

	"pad-analyzer/internal/ai"
	"pad-core/logger"
)

// Machine-readable failure codes carried on chat stream "error" events (and on
// ResumeResult.ErrorCode) so the client can branch on the failure class
// instead of pattern-matching the human-readable message, which is free to be
// reworded. An empty code means "unclassified" — the message is all there is.
const (
	ChatErrBudgetExceeded        = "AI_BUDGET_EXCEEDED"
	ChatErrBudgetUnavailable     = "AI_BUDGET_UNAVAILABLE"
	ChatErrProviderUnavailable   = "PROVIDER_UNAVAILABLE"
	ChatErrProviderRateLimited   = "PROVIDER_RATE_LIMITED"
	ChatErrProviderAuth          = "PROVIDER_AUTH"
	ChatErrProviderBalance       = "PROVIDER_BALANCE"
	ChatErrProviderNotConfigured = "PROVIDER_NOT_CONFIGURED"
	ChatErrContextLimit          = "CONTEXT_LIMIT"
	ChatErrStopped               = "STREAM_STOPPED"
	ChatErrTimeout               = "STREAM_TIMEOUT"
	ChatErrInterrupted           = "STREAM_INTERRUPTED"
	ChatErrToolLoopLimit         = "TOOL_LOOP_LIMIT"
)

// ErrAIBudgetExceeded / ErrAIBudgetUnavailable classify enforceBudget
// failures. Match with errors.Is; the concrete error is a *budgetError whose
// message is client-safe.
var (
	ErrAIBudgetExceeded    = errors.New("daily AI budget exceeded")
	ErrAIBudgetUnavailable = errors.New("daily AI budget check unavailable")
)

// budgetError is a budget failure whose Error() is safe to show the user. The
// underlying cause (a storage/driver error when the spend could not be read)
// stays reachable through Unwrap for logging, but never reaches the client:
// stream errors bypass the HTTP layer's 5xx masking, so a raw "postgres: get
// daily usage: …" used to be rendered verbatim in the chat.
type budgetError struct {
	kind  error
	msg   string
	cause error
}

func (e *budgetError) Error() string        { return e.msg }
func (e *budgetError) Is(target error) bool { return target == e.kind }
func (e *budgetError) Unwrap() error        { return e.cause }

// budgetUnavailable reports a spend read that failed. The storage error is
// logged here — it is an operator's problem, not something the user can act
// on — and the returned error carries only client-safe copy. The caller still
// fails closed (the turn is refused). A read that failed only because the
// stream itself ended (user stop, response-time cap) is not worth a warning.
func budgetUnavailable(scope string, cause error) error {
	if !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		logger.Warn("chat: daily AI budget check failed", "scope", scope, "error", cause)
	}
	return &budgetError{
		kind:  ErrAIBudgetUnavailable,
		msg:   "couldn't verify today's AI budget — try again in a moment",
		cause: cause,
	}
}

// errProviderSetup stands in for a provider that could not be resolved for a
// reason the user cannot act on — a key-store read or decryption failure.
// The cause is logged by providerSetupError and never shown: stream errors
// bypass the HTTP layer's 5xx masking, so "get claude key: keystore: …" used
// to reach the chat bubble verbatim.
var errProviderSetup = errors.New("couldn't load the AI provider configuration — try again in a moment")

// providerSetupError maps a ProviderFactory.For failure to what the client may
// see. A missing key is the user's to fix and keeps its sentinel
// (classifyFailure codes it PROVIDER_NOT_CONFIGURED); anything else is logged
// and replaced by errProviderSetup.
func providerSetupError(scope, providerID string, cause error) error {
	if errors.Is(cause, ai.ErrKeyNotConfigured) {
		return cause
	}
	logger.Warn("chat: provider setup failed", "scope", scope, "provider", providerID, "error", cause)
	return errProviderSetup
}

// loopBudgetFailure classifies a failed per-iteration budget check inside a
// tool loop. A read that failed only because the stream's context ended (user
// stop, the response-time cap) is reported as that — classifyFailure already
// words it, and the timeout copy carries its own "response stopped" — with no
// operator report. A real mid-loop budget stop gets the prefix and is worth
// aggregating: the turn already burned budget before it was cut off.
func loopBudgetFailure(ctx context.Context, ctl *streamCtl, err error) (code, msg string, report bool) {
	code, msg = ctl.classifyFailure(ctx, err)
	if ctx.Err() != nil {
		return code, msg, false
	}
	return code, "response stopped: " + msg, true
}

// errorEventData builds an SSE "error" payload: the message plus, when known,
// its machine-readable code.
func errorEventData(code, msg string) map[string]interface{} {
	data := map[string]interface{}{"message": msg}
	if code != "" {
		data["code"] = code
	}
	return data
}

// classifyFailure maps a stream error to its client-facing (code, message).
// A deliberate cancellation surfaces its stored reason, the stream-duration
// timeout gets a readable message, and the common provider sentinels map to
// actionable copy — a raw "rate limited" or "provider circuit open: too many
// recent failures" told the user nothing they could act on.
func (c *streamCtl) classifyFailure(ctx context.Context, err error) (code, msg string) {
	c.mu.Lock()
	reason := c.cancelReason
	c.mu.Unlock()
	if ctx.Err() != nil {
		if reason != "" {
			return ChatErrStopped, reason
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ChatErrTimeout, "response stopped: maximum response time reached"
		}
	}
	switch {
	case errors.Is(err, ErrAIBudgetExceeded):
		return ChatErrBudgetExceeded, err.Error()
	case errors.Is(err, ErrAIBudgetUnavailable):
		return ChatErrBudgetUnavailable, err.Error()
	case errors.Is(err, ai.ErrContextLimit):
		return ChatErrContextLimit, "conversation is too long for this model's context window — start a new conversation or remove some history"
	case errors.Is(err, ai.ErrRateLimited):
		return ChatErrProviderRateLimited, "the AI provider is rate-limiting requests — wait a moment and try again"
	case errors.Is(err, ai.ErrCircuitOpen):
		return ChatErrProviderUnavailable, "the AI provider is temporarily unavailable after repeated failures — wait a moment and try again"
	case errors.Is(err, ai.ErrProviderDown):
		return ChatErrProviderUnavailable, "the AI provider is temporarily unavailable — try again shortly"
	case errors.Is(err, ai.ErrInsufficientBalance):
		return ChatErrProviderBalance, "the AI provider account has insufficient balance — add credits or switch providers"
	case errors.Is(err, ai.ErrApiKeyInvalid):
		return ChatErrProviderAuth, "the API key for this provider is invalid — check it in Settings → AI Providers"
	case errors.Is(err, ai.ErrKeyNotConfigured):
		return ChatErrProviderNotConfigured, "no API key is configured for this provider — add one in Settings → AI Providers"
	case errors.Is(err, errProviderSetup):
		return ChatErrProviderUnavailable, err.Error()
	case isStreamTruncation(err):
		return ChatErrInterrupted, "the response was interrupted before it finished — try again"
	}
	return "", err.Error()
}
