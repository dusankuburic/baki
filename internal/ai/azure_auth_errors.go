package ai

import (
	"encoding/json"
	"errors"
	"strings"
)

// ErrEntraTokenRejected marks a 401/403 the resource gave an Entra ID access
// token that WAS issued: the audience the resource expects is not
// https://ai.azure.com, the resource is in another cloud or has Entra ID
// authentication disabled, or the token was revoked. Also ErrApiKeyInvalid so
// every auth-failure rule applies (no retry, no breaker count, one
// withTokenRetry replay); the chat copy and the connection test check it
// first — the principal has no API key to check.
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

// azureAuthMessage is the service's one-line explanation of a 401/403 from
// either envelope: {"error":{"code","message"}} or API Management's
// {"statusCode":401,"message":…} ("Unauthorized. Access token is missing,
// invalid, audience is incorrect (...), or have expired."), which decodes
// into openAIErrorResp with an empty message.
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

// entraUnauthorized is openaiBase's handle401 hook in Entra ID mode (set by
// the Azure provider only, so OpenAI, xAI, GLM, GitHub Models, Copilot and
// API-key Azure keep the bare invalid-key classification): PermissionDenied
// keeps its own path (nil → unauthorizedError); every other 401/403 is a
// rejected token carrying Azure's explanation.
func entraUnauthorized(apiErr openAIErrorResp, body []byte) error {
	if strings.EqualFold(apiErr.Error.Code, "PermissionDenied") {
		return nil
	}
	return &entraTokenRejectedError{msg: azureAuthMessage(apiErr, body)}
}
