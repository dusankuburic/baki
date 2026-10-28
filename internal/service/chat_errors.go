package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/platform/drain"
	"pad-analyzer/internal/storage"
	"pad-core/logger"
)

// Machine-readable failure codes carried on chat stream "error" events (and ResumeResult.ErrorCode) so the
// client can branch on the failure class instead of pattern-matching the rewordable message; "" = unclassified.
const (
	ChatErrProviderUnavailable   = "PROVIDER_UNAVAILABLE"
	ChatErrProviderRateLimited   = "PROVIDER_RATE_LIMITED"
	ChatErrProviderAuth          = "PROVIDER_AUTH"
	ChatErrProviderBalance       = "PROVIDER_BALANCE"
	ChatErrProviderNotConfigured = "PROVIDER_NOT_CONFIGURED"
	ChatErrContextLimit          = "CONTEXT_LIMIT"
	// ChatErrContextUnavailable: the flow's redacting clone could not be made, so nothing was sent (errContextScrub).
	ChatErrContextUnavailable = "CONTEXT_UNAVAILABLE"
	ChatErrStopped            = "STREAM_STOPPED"
	// ChatErrIdleTimeout: the watchdog stopped a stream that made no progress for the idle limit; the message names
	// the phase it stalled in (setup, the provider, a tool, an approval, an apply) — R-streaming-10.
	ChatErrIdleTimeout = "STREAM_IDLE_TIMEOUT"
	// ChatErrClientGone: the watchdog stopped a begun stream whose owner had no live event connection for its grace
	// period (the tab was closed or lost its connection).
	ChatErrClientGone    = "STREAM_CLIENT_GONE"
	ChatErrTimeout       = "STREAM_TIMEOUT"
	ChatErrInterrupted   = "STREAM_INTERRUPTED"
	ChatErrToolLoopLimit = "TOOL_LOOP_LIMIT"
	// ChatErrHistoryUnavailable: the thread's stored history could not be read at the start of the turn, so the
	// turn was not sent (an answer without its context would be silently wrong). Retryable.
	ChatErrHistoryUnavailable = "HISTORY_UNAVAILABLE"
	ChatErrContentFiltered    = "PROVIDER_CONTENT_FILTERED"
	// ChatErrProviderPermission: credentials accepted, but the identity lacks the role for the model or agent.
	ChatErrProviderPermission = "PROVIDER_PERMISSION"
	// ChatErrOutputLimit: the model spent its whole completion budget (Max tokens) without writing any answer —
	// reasoning models count their hidden thinking against that budget.
	ChatErrOutputLimit = "PROVIDER_OUTPUT_LIMIT"
	// ChatErrModelNotAvailable: the turn's model is not one the caller can use any more (ErrModelNotAvailable);
	// the client refreshes its model list and offers another model.
	ChatErrModelNotAvailable = "MODEL_NOT_AVAILABLE"
	// ChatErrServerRestarting: the server's stop ended the stream (Shutdown); the client may send the turn again
	// once, on the replica its reconnect reaches.
	ChatErrServerRestarting = drain.Code
	// ChatErrProviderUnknown: the turn names a provider no spec describes (ErrProviderUnknown) — a stale client
	// or a typo, refused before the stream in the common case; here for a provider gone mid-flight.
	ChatErrProviderUnknown = ProviderCodeUnknown
	// ChatErrProviderNotAllowed: the provider policy refused the turn's provider at the factory
	// (ai.ErrProviderNotAllowed) — disabled in the deployment, or off the organisation's allowed list.
	ChatErrProviderNotAllowed = ProviderCodeNotAllowed
)

// serverRestartingMsg is the ChatErrServerRestarting copy: a client that does not know the code shows it with
// its Retry, which is the right advice.
const serverRestartingMsg = "response stopped: the server is restarting — try again in a moment"

// modelNotAvailableMsg is the ChatErrModelNotAvailable copy.
const modelNotAvailableMsg = "this model is no longer available — it was removed, or it is no longer shared with you; choose another model"

// errAzureNoCredentials marks a turn that resolved onto a resource with no stored credentials (never entered,
// or the admin who lent them was demoted); it wraps ai.ErrKeyNotConfigured so the code stays NOT_CONFIGURED.
var errAzureNoCredentials = errors.New("the Azure AI Foundry resource of this model has no stored credentials")

// errAzureUnreadableCredentials marks a turn that resolved onto a resource whose stored credentials the keystore
// cannot decrypt (ErrSecretDecryptFailed — the server's encryption key changed). Only entering them again
// repairs it, so it is PROVIDER_NOT_CONFIGURED with copy that says so — never the generic setup error.
var errAzureUnreadableCredentials = errors.New("the stored credentials of this model's Azure AI Foundry resource could not be read")

// Copy for errAzureUnreadableCredentials: the caller's own resource, and an organisation's.
const (
	azureUnreadableMsg       = "the stored credentials of this model's Azure AI Foundry resource could not be read (the server's encryption key may have changed) — enter them again under Settings → AI Accounts → Azure AI Foundry"
	azureUnreadableSharedMsg = "the stored credentials of your organisation's shared Azure AI Foundry resource could not be read (the server's encryption key may have changed) — ask your organisation admin to enter them again under Settings → Organizations (or choose one of your own models)"
)

// errKeyUnreadable marks a turn whose bare key the keystore cannot decrypt (ErrSecretDecryptFailed — the server's
// encryption key changed): the Azure resource case's twin for every other provider. errOrgKeyUnreadable is the
// organisation's shared key. Both are PROVIDER_NOT_CONFIGURED with copy that asks for the key again.
var (
	errKeyUnreadable    = errors.New("the stored API key for this provider could not be read")
	errOrgKeyUnreadable = errors.New("the organisation's shared API key for this provider could not be read")
)

// Copy for errKeyUnreadable and errOrgKeyUnreadable.
const (
	keyUnreadableMsg       = "the stored credential for this provider could not be read (the server's encryption key may have changed) — enter the key again, or sign in again, in Settings → AI Accounts"
	keyUnreadableSharedMsg = "your organisation's shared API key for this provider could not be read (the server's encryption key may have changed) — ask your organisation admin to enter it again under Settings → Organizations (or add a key of your own)"
)

// unreadableKeyError marks err when a bare key the turn needed could not be decrypted: the organisation's when the
// failing read was its shared credential's, else the caller's own. Any other err is returned unchanged.
func unreadableKeyError(err error) error {
	if !errors.Is(err, storage.ErrSecretDecryptFailed) {
		return err
	}
	if orgErr := (*ai.OrgKeyError)(nil); errors.As(err, &orgErr) {
		return fmt.Errorf("%w: %w", errOrgKeyUnreadable, err)
	}
	return fmt.Errorf("%w: %w", errKeyUnreadable, err)
}

// circuitOpenMsg is the ErrCircuitOpen copy naming the model and when to try again (R-observability-7).
func circuitOpenMsg(model string, wait time.Duration) string {
	subject := "this model"
	switch model {
	case "", "none", "uncatalogued":
	case "deployment":
		subject = "this deployment"
	default:
		subject = model
	}
	secs := int(wait.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("the AI provider is temporarily unavailable for %s after repeated failures — try again in %d s", subject, secs)
}

// accessDeniedMsg is the ErrProviderAccessDenied copy: the vendor's own reason, and where to look.
func accessDeniedMsg(err error) string {
	return "the provider refused this request for your account (" + ai.AccessDeniedMessage(err) +
		") — check the account's region, project permissions or billing with the provider"
}

// outputLimitMsg is the ChatErrOutputLimit copy.
const outputLimitMsg = "the model used its whole Max tokens budget before writing an answer (reasoning models count their thinking against it) — raise Max tokens in Settings → AI Behavior and try again"

// isOutputLimitFinish reports whether a finish reason means the completion was cut at an output limit: the
// token cap ("length"/"max_tokens"/"MAX_TOKENS") or, on Claude 4.5+, the context window filling up during
// generation ("model_context_window_exceeded" — the API accepts input + max_tokens beyond the window and stops).
func isOutputLimitFinish(reason string) bool {
	switch reason {
	case "length", "max_tokens", "MAX_TOKENS", "model_context_window_exceeded":
		return true
	}
	return false
}

// truncatedToolTurn reports a turn whose tool calls must not run: cut at an output limit, a call may miss
// arguments or be only the first of those meant. Malformed JSON with a NON-limit finish is different —
// ExecuteTool answers those with a retryable "not valid JSON" result.
func truncatedToolTurn(calls []ai.ToolCall, finishReason string) bool {
	return len(calls) > 0 && isOutputLimitFinish(finishReason)
}

// emptyOutputLimitTurn reports a turn that ended at the token cap with no text and no tool calls: a
// "successful" completion that is empty (and billed). Cut AFTER some text keeps that text.
func emptyOutputLimitTurn(sawText bool, toolCalls int, finishReason string) bool {
	return !sawText && toolCalls == 0 && isOutputLimitFinish(finishReason)
}

// errProviderSetup stands in for a provider that could not be resolved for a reason the user cannot act on
// (keystore read/decryption failure, unreadable registry). The cause is logged and never shown — stream errors
// bypass the HTTP 5xx masking.
var errProviderSetup = errors.New("couldn't load the AI provider configuration — try again in a moment")

// providerSetupError maps a provider-setup failure to what the client may see: a missing key is the user's to
// fix and keeps its sentinel, as does an unknown provider id (the client's, not an incident) and a resource
// whose credentials cannot be decrypted (logged too — the operator's concern); anything else is logged and
// replaced by errProviderSetup. No kept error's text reaches the client: classifyFailure words them all.
func providerSetupError(ctx context.Context, scope, providerID string, cause error) error {
	if errors.Is(cause, ai.ErrKeyNotConfigured) || errors.Is(cause, ErrProviderUnknown) || errors.Is(cause, ai.ErrProviderNotAllowed) {
		return cause
	}
	logger.FromContext(ctx).Warn("chat: provider setup failed", "scope", scope, "provider", providerID, "error", cause)
	if errors.Is(cause, errAzureUnreadableCredentials) || errors.Is(cause, errKeyUnreadable) || errors.Is(cause, errOrgKeyUnreadable) {
		return cause
	}
	return errProviderSetup
}

// errorEventData builds an SSE "error" payload: message, code when known, and the shared flag for the
// org-shared credential classes (the client's error bubble offers Organizations). Both are omitted when unset.
func errorEventData(code, msg string, shared bool) map[string]interface{} {
	data := map[string]interface{}{"message": msg}
	if code != "" {
		data["code"] = code
	}
	if shared {
		data["shared"] = true
	}
	return data
}

// classifyFailure maps a stream error to its client-facing (code, message): a deliberate cancellation surfaces
// its stored reason, the duration timeout gets a readable message, and the common provider sentinels map to
// actionable copy. A turn on one of the org's shared Azure resources that failed on its credential gets copy
// pointing at the org admin — the member cannot fix the org's resource in their own Settings.
func (c *streamCtl) classifyFailure(ctx context.Context, err error) (code, msg string) {
	code, msg, _ = c.classifyFailureShared(ctx, err)
	return code, msg
}

// classifyFailureShared is classifyFailure with the shared-credential flag: shared is true only when the turn
// ran on the org's credential AND the failure is one of the credential errors whose copy points at the org
// admin — a budget/context/rate limit on the same shared provider keeps shared=false. The flag rides the error
// event and the resume/begin snapshots, so a reloaded client's bubble offers Organizations too.
func (c *streamCtl) classifyFailureShared(ctx context.Context, err error) (code, msg string, shared bool) {
	code, msg = c.classifyFailureBase(ctx, err)
	if c.sharedCredential.Load() {
		if sharedMsg, ok := sharedCredentialFailureMsg(code, err); ok {
			return code, sharedMsg, true
		}
	}
	return code, msg, false
}

// sharedCredentialFailureMsg is the org-credential copy for a failure, keyed by classifyFailureBase's code; ok
// is false for every other failure. None of them is the member's to fix — a shared resource and its credentials
// are the org admins' (Settings → Organizations); the member's way out is a model of their own.
func sharedCredentialFailureMsg(code string, err error) (string, bool) {
	const ask = "ask your organisation admin to check the shared Azure AI Foundry resource under Settings → Organizations"
	const own = " (or choose one of your own models)"
	switch {
	case code == ChatErrProviderPermission && errors.Is(err, ai.ErrProviderAccessDenied):
		return "the provider refused your organisation's shared key for this request (" + ai.AccessDeniedMessage(err) +
			") — ask your organisation admin to check the account under Settings → Organizations (or add a key of your own)", true
	case code == ChatErrProviderPermission:
		return "Azure accepted your organisation's shared credentials, but their identity lacks permission for this model or agent — " + ask + own, true
	case code == ChatErrProviderAuth:
		return "Azure rejected the credentials of your organisation's shared resource — " + ask + own, true
	case code == ChatErrProviderNotConfigured && errors.Is(err, errAzureNoCredentials):
		return "your organisation's shared Azure AI Foundry resource has no credentials (for example, the admin who shared them is no longer an admin) — " + ask + own, true
	case code == ChatErrProviderNotConfigured && errors.Is(err, errAzureUnreadableCredentials):
		return azureUnreadableSharedMsg, true
	case code == ChatErrProviderNotConfigured && errors.Is(err, errOrgKeyUnreadable):
		return keyUnreadableSharedMsg, true
	case code == ChatErrProviderNotConfigured && errors.Is(err, ai.ErrProviderConfigInvalid):
		return "the settings of your organisation's shared Azure AI Foundry resource are no longer valid — " + ask, true
	}
	return "", false
}

// classifyFailureBase is classifyFailure without the shared-credential copy.
func (c *streamCtl) classifyFailureBase(ctx context.Context, err error) (code, msg string) {
	c.mu.Lock()
	reason, cancelCode := c.cancelReason, c.cancelCode
	c.mu.Unlock()
	if ctx.Err() != nil {
		if reason != "" {
			return cmp.Or(cancelCode, ChatErrStopped), reason
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ChatErrTimeout, "response stopped: maximum response time reached"
		}
	}
	switch {
	case errors.Is(err, ai.ErrContextLimit):
		return ChatErrContextLimit, "conversation is too long for this model's context window — start a new conversation or remove some history"
	case errors.Is(err, ai.ErrRateLimited):
		return ChatErrProviderRateLimited, "the AI provider is rate-limiting requests — wait a moment and try again"
	case errors.Is(err, ai.ErrCircuitOpen):
		if model, wait, ok := ai.CircuitOpenDetail(err); ok {
			return ChatErrProviderUnavailable, circuitOpenMsg(model, wait)
		}
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
	case errors.Is(err, ai.ErrClaudeEntraOnly):
		// Before ErrProviderPermissionDenied and ErrApiKeyInvalid, which it also matches: a valid key against a
		// Claude Mythos deployment is not a key or role problem — the family accepts Entra ID only.
		return ChatErrProviderAuth, "Azure AI Foundry refused the API key for this Claude deployment — Claude Mythos deployments accept Microsoft Entra ID only — switch the Azure AI Foundry credential to Microsoft Entra ID"
	case errors.Is(err, ai.ErrProviderAccessDenied):
		// Before ErrProviderPermissionDenied, whose copy is Azure's: the key was read, and the provider refused
		// this account (a region block, a project permission, a billing hard limit).
		return ChatErrProviderPermission, accessDeniedMsg(err)
	case errors.Is(err, ai.ErrProviderPermissionDenied):
		// Before ErrApiKeyInvalid, which it also matches: the credentials work, a role is missing.
		return ChatErrProviderPermission, "the provider accepted these credentials, but the service principal lacks permission for this model or agent — assign it the Foundry User role on the Azure AI Foundry resource, then use Test in Settings → AI Accounts for details"
	case errors.Is(err, ai.ErrEntraCredentialsRejected):
		// Also ErrApiKeyInvalid; an Entra ID service principal has no API key. Name the specific cause when
		// Entra gave one (expired secret, secret ID pasted for its value, unknown tenant, …).
		if reason, ok := ai.EntraRejectionReason(err); ok {
			return ChatErrProviderAuth, reason + " — update the credentials in Settings → AI Accounts"
		}
		return ChatErrProviderAuth, "Microsoft Entra ID rejected this provider's credentials (tenant ID, client ID or client secret — an expired client secret is the usual cause) — update them in Settings → AI Accounts"
	case errors.Is(err, ai.ErrEntraTokenRejected):
		// Also ErrApiKeyInvalid; the token WAS issued, the resource refused it (wrong audience, Entra ID auth
		// disabled, another cloud). The bubble must not send the user to edit a key the principal does not have.
		return ChatErrProviderAuth, "Azure rejected this provider's Microsoft Entra ID access token — check that the endpoint is the resource's own and that Entra ID authentication is enabled on it, then use Test in Settings → AI Accounts for Azure's explanation"
	case errors.Is(err, ai.ErrApiKeyInvalid):
		return ChatErrProviderAuth, "the API key for this provider is invalid — check it in Settings → AI Accounts"
	case errors.Is(err, ErrModelNotAvailable):
		return ChatErrModelNotAvailable, modelNotAvailableMsg
	case errors.Is(err, errAzureNoCredentials):
		// Before ErrKeyNotConfigured, which it wraps: the model's resource,
		// not the provider, is what lacks credentials.
		return ChatErrProviderNotConfigured, "this model's Azure AI Foundry resource has no stored credentials — enter them again under Settings → AI Accounts → Azure AI Foundry"
	case errors.Is(err, errAzureUnreadableCredentials):
		return ChatErrProviderNotConfigured, azureUnreadableMsg
	case errors.Is(err, errKeyUnreadable):
		return ChatErrProviderNotConfigured, keyUnreadableMsg
	case errors.Is(err, errOrgKeyUnreadable):
		// The shared-credential flag is set when the turn was set up (prepareTurn), so classifyFailureShared
		// words it for the org's admin; this is the base copy.
		return ChatErrProviderNotConfigured, keyUnreadableSharedMsg
	case errors.Is(err, ai.ErrKeyNotConfigured):
		return ChatErrProviderNotConfigured, "no API key is configured for this provider — add one in Settings → AI Accounts"
	case errors.Is(err, ErrProviderUnknown):
		return ChatErrProviderUnknown, "this AI provider is unknown to the server — reload the app and choose a provider from Settings → AI Accounts"
	case errors.Is(err, ai.ErrProviderNotAllowed):
		// The policy's own copy: the provider id and what to do, no credential or store detail.
		return ChatErrProviderNotAllowed, err.Error()
	case errors.Is(err, errProviderSetup):
		return ChatErrProviderUnavailable, err.Error()
	case isStreamTruncation(err):
		return ChatErrInterrupted, "the response was interrupted before it finished — try again"
	}
	return "", err.Error()
}
