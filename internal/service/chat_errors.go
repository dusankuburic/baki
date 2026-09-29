package service

import (
	"bytes"
	"context"
	"encoding/json"
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
	ChatErrContentFiltered       = "PROVIDER_CONTENT_FILTERED"
	// ChatErrProviderPermission: the provider accepted the credentials but
	// the identity lacks the role for the model or agent (Azure AI Foundry
	// "PermissionDenied" — no Foundry User role for the service principal).
	ChatErrProviderPermission = "PROVIDER_PERMISSION"
	// ChatErrOutputLimit: the model spent its whole completion budget (Max
	// tokens) without writing any answer — reasoning models (o-series,
	// GPT-5, DeepSeek-R1) count their hidden thinking against that budget.
	ChatErrOutputLimit = "PROVIDER_OUTPUT_LIMIT"
)

// outputLimitMsg is the ChatErrOutputLimit copy.
const outputLimitMsg = "the model used its whole Max tokens budget before writing an answer (reasoning models count their thinking against it) — raise Max tokens in Settings → AI Behavior and try again"

// isOutputLimitFinish reports whether a provider finish reason means the
// completion was cut off at an output limit: its token cap — OpenAI-compatible
// chat completions and the Responses mapping ("length"), Claude
// ("max_tokens"), Gemini ("MAX_TOKENS") — or, on Claude 4.5+ models, the
// context window filling up during generation
// ("model_context_window_exceeded"; the API accepts input + max_tokens beyond
// the window and stops there instead of refusing the request).
func isOutputLimitFinish(reason string) bool {
	switch reason {
	case "length", "max_tokens", "MAX_TOKENS", "model_context_window_exceeded":
		return true
	}
	return false
}

// truncatedToolTurn reports a turn whose tool calls must not run: the turn
// was cut off at an output limit (a call may be missing arguments or be only
// the first of those the model meant to make), or a call's arguments are not
// valid JSON — a call cut mid-argument, whatever finish reason the provider
// reported. Empty arguments are the providers' "{}" (no arguments).
func truncatedToolTurn(calls []ai.ToolCall, finishReason string) bool {
	if len(calls) == 0 {
		return false
	}
	if isOutputLimitFinish(finishReason) {
		return true
	}
	for _, tc := range calls {
		if in := bytes.TrimSpace(tc.Input); len(in) > 0 && !json.Valid(in) {
			return true
		}
	}
	return false
}

// emptyOutputLimitTurn reports a turn that ended at the token cap with no
// answer text and no tool calls: a "successful" completion that is empty
// (and billed). A turn cut at the cap AFTER some text keeps that text — a
// truncated answer is still shown, as before.
func emptyOutputLimitTurn(sawText bool, toolCalls int, finishReason string) bool {
	return !sawText && toolCalls == 0 && isOutputLimitFinish(finishReason)
}

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
//
// A turn on the organisation's shared credential (sharedCredential) that the
// provider refused — rejected credentials, a missing role, an unreadable
// document — gets copy pointing at the org admin: the member cannot fix the
// org's configuration in their own Settings.
func (c *streamCtl) classifyFailure(ctx context.Context, err error) (code, msg string) {
	code, msg = c.classifyFailureBase(ctx, err)
	if c.sharedCredential.Load() {
		if shared, ok := sharedCredentialFailureMsg(code, err); ok {
			msg = shared
		}
	}
	return code, msg
}

// sharedCredentialFailureMsg is the copy for a credential failure on the
// organisation's shared Azure AI Foundry configuration, keyed by the code
// classifyFailureBase chose; ok is false for every other failure.
func sharedCredentialFailureMsg(code string, err error) (string, bool) {
	const ask = "ask your organisation admin to update the shared Azure AI Foundry configuration"
	switch {
	case code == ChatErrProviderPermission:
		return "the provider accepted your organisation's shared credentials, but its service principal lacks permission for this model or agent — " + ask + " (or add your own in Settings → AI Accounts)", true
	case code == ChatErrProviderAuth:
		return "the provider rejected your organisation's shared credentials — " + ask + " (or add your own in Settings → AI Accounts)", true
	case code == ChatErrProviderNotConfigured && errors.Is(err, ai.ErrProviderConfigInvalid):
		return "your organisation's shared provider configuration is invalid — " + ask, true
	}
	return "", false
}

// classifyFailureBase is classifyFailure without the shared-credential copy.
func (c *streamCtl) classifyFailureBase(ctx context.Context, err error) (code, msg string) {
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
	case errors.Is(err, ai.ErrEntraTokenUnavailable):
		// The identity service (not the resource) did not answer: deliberately
		// not ErrProviderDown upstream, so no retries and no open breaker.
		return ChatErrProviderUnavailable, "could not obtain a Microsoft Entra ID token for this provider (the identity service did not answer) — try again shortly"
	case errors.Is(err, ai.ErrInsufficientBalance):
		return ChatErrProviderBalance, "the AI provider account has insufficient balance — add credits or switch providers"
	case errors.Is(err, ai.ErrContentFiltered):
		return ChatErrContentFiltered, "the provider's content filter blocked this request or response — rephrase and try again"
	case errors.Is(err, ai.ErrOutputLimit):
		return ChatErrOutputLimit, outputLimitMsg
	case errors.Is(err, ai.ErrProviderConfigInvalid):
		return ChatErrProviderNotConfigured, "this provider's configuration is invalid — open Settings → AI Accounts and save it again"
	case errors.Is(err, ai.ErrProviderPermissionDenied):
		// Before ErrApiKeyInvalid, which it also matches: the credentials
		// work, a role is missing.
		return ChatErrProviderPermission, "the provider accepted these credentials, but the service principal lacks permission for this model or agent — assign it the Foundry User role on the Azure AI Foundry resource, then use Test in Settings → AI Accounts for details"
	case errors.Is(err, ai.ErrEntraCredentialsRejected):
		// Also ErrApiKeyInvalid; an Entra ID service principal has no API key.
		// Name the specific cause when Entra gave one (expired secret, secret
		// ID pasted for its value, unknown tenant, Conditional Access, …).
		if reason, ok := ai.EntraRejectionReason(err); ok {
			return ChatErrProviderAuth, reason + " — update the credentials in Settings → AI Accounts"
		}
		return ChatErrProviderAuth, "Microsoft Entra ID rejected this provider's credentials (tenant ID, client ID or client secret — an expired client secret is the usual cause) — update them in Settings → AI Accounts"
	case errors.Is(err, ai.ErrEntraTokenRejected):
		// Also ErrApiKeyInvalid; the token WAS issued, the resource refused it
		// (wrong audience for the resource kind, Entra ID auth disabled, a
		// resource in another cloud). Azure's own explanation is what the
		// connection test shows; the bubble must not send the user to edit a
		// key the principal does not have.
		return ChatErrProviderAuth, "Azure rejected this provider's Microsoft Entra ID access token — check that the endpoint is the resource's own and that Entra ID authentication is enabled on it, then use Test in Settings → AI Accounts for Azure's explanation"
	case errors.Is(err, ai.ErrApiKeyInvalid):
		return ChatErrProviderAuth, "the API key for this provider is invalid — check it in Settings → AI Accounts"
	case errors.Is(err, ai.ErrKeyNotConfigured):
		return ChatErrProviderNotConfigured, "no API key is configured for this provider — add one in Settings → AI Accounts"
	case errors.Is(err, errProviderSetup):
		return ChatErrProviderUnavailable, err.Error()
	case isStreamTruncation(err):
		return ChatErrInterrupted, "the response was interrupted before it finished — try again"
	}
	return "", err.Error()
}
