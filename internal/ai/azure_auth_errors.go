package ai

import (
	"encoding/json"
	"errors"
	"strings"
)

// ErrEntraTokenRejected marks a 401/403 the resource gave an Entra ID token that WAS issued: wrong audience
// (not https://ai.azure.com), another cloud, Entra ID disabled on the resource, or a revoked token. Also
// ErrApiKeyInvalid, so every auth-failure rule applies (no retry, no breaker count, one withTokenRetry replay).
var ErrEntraTokenRejected = errors.New("azure ai foundry rejected the entra id access token")

// entraTokenRejectedError carries the service's own one-line explanation.
type entraTokenRejectedError struct{ msg string }

func (e *entraTokenRejectedError) Error() string {
	if e.msg == "" {
		return ErrEntraTokenRejected.Error()
	}
	return ErrEntraTokenRejected.Error() + ": " + e.msg
}

func (e *entraTokenRejectedError) Unwrap() []error {
	return []error{ErrEntraTokenRejected, ErrApiKeyInvalid}
}

// azureAuthMessageMax bounds the service text kept on the error.
const azureAuthMessageMax = 300

// azureAuthMessage is the service's one-line explanation of a 401/403 from either envelope,
// {"error":{"code","message"}} or API Management's {"statusCode":401,"message":…}, which decodes into
// openAIErrorResp with an empty message.
func azureAuthMessage(apiErr openAIErrorResp, body []byte) string {
	msg := apiErr.Error.Message
	if msg == "" {
		var apim struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &apim) == nil {
			msg = apim.Message
		}
	}
	return oneLine(strings.Join(strings.Fields(msg), " "), azureAuthMessageMax)
}

// entraUnauthorized is openaiBase's handle401 hook in Entra ID mode (Azure only — API-key Azure and the other
// providers keep the bare invalid-key classification): a permission denial (Azure's PermissionDenied code, the
// Anthropic route's permission_error) keeps its own path — the token was accepted and a fresh one cannot grant
// the missing role. Every other 401/403 is a rejected token carrying Azure's explanation.
func entraUnauthorized(apiErr openAIErrorResp, body []byte) error {
	if strings.EqualFold(apiErr.Error.Code, "PermissionDenied") || strings.EqualFold(apiErr.Error.Type, "permission_error") {
		return nil
	}
	return &entraTokenRejectedError{msg: azureAuthMessage(apiErr, body)}
}

// azureForbiddenFallback explains a 403 whose body says nothing.
const azureForbiddenFallback = "the Azure resource refused the request (HTTP 403): check its networking rules (firewall, virtual network, private endpoint) and the role the credential holds on it"

// azureForbidden is openaiBase's handle403 hook for Azure, in both auth modes: a 403 means the credential WAS
// accepted but the request is not allowed (network rules, a missing data-plane role) — never an invalid key or
// rejected token. Kept as a permission denial (ErrProviderPermissionDenied, also ErrApiKeyInvalid: no retry,
// no breaker count, no withTokenRetry replay — a fresh token could not change the answer).
func azureForbidden(apiErr openAIErrorResp, body []byte) error {
	msg := azureAuthMessage(apiErr, body)
	if msg == "" {
		msg = azureForbiddenFallback
	}
	return &permissionDeniedError{msg: msg}
}
