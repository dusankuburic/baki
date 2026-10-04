package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Claude deployments on Azure AI Foundry are NOT reachable on the resource's /openai/v1 route (404): they
// answer on the Anthropic Messages API at POST {resource}.services.ai.azure.com/anthropic/v1/messages, with
// anthropic-version 2023-06-01 and either api-key AND x-api-key (API-key mode) or Authorization: Bearer
// <Entra ID token>; `model` is the DEPLOYMENT name. That host is the only documented one, so an endpoint on
// *.openai.azure.com / *.cognitiveservices.azure.com is re-hosted to <resource>.services.ai.azure.com;
// any other allow-listed host (an APIM front) is used as-is. The picker offers each deployment as
// "claude:<name>" — the prefix routes the call to the Anthropic wire format.

// claudeModelPrefix marks a model id that names a Claude deployment on the
// resource (the Anthropic Messages route) rather than a chat-completions
// deployment.
const claudeModelPrefix = "claude:"

// IsClaudeModel reports whether a model id names a Claude deployment on an
// Azure AI Foundry resource.
func IsClaudeModel(id string) bool { return strings.HasPrefix(id, claudeModelPrefix) }

// claudeDeploymentName is the deployment behind a "claude:<name>" id.
func claudeDeploymentName(id string) string { return strings.TrimPrefix(id, claudeModelPrefix) }

const (
	// claudeOnFoundryContextLimit is the window of a Claude deployment the catalogue does not know yet — 200k
	// is what every Claude model on Foundry accepts (catalogued models use their own, larger, window).
	claudeOnFoundryContextLimit = 200000
	// claudeOnFoundryDefaultOutput is the output cap of an uncatalogued deployment: the smallest cap of any
	// Claude model Foundry serves, so the reasoning floor is not clipped (the model's own cap applies once known).
	claudeOnFoundryDefaultOutput = 64000
	// anthropicModelPublisher is the project deployments listing's
	// modelPublisher for Claude deployments.
	anthropicModelPublisher = "Anthropic"
)

// claudeOnFoundryHostSuffixes are the resource host suffixes whose Claude
// route lives on the same resource's services.ai.azure.com host.
var claudeOnFoundryHostSuffixes = []string{".openai.azure.com", ".cognitiveservices.azure.com"}

// claudeOnFoundryBase is the base URL of the resource's Anthropic route: the endpoint itself, except that
// *.openai.azure.com and *.cognitiveservices.azure.com hosts become <resource>.services.ai.azure.com, the
// host Anthropic documents; every other host (an APIM front, a private DNS name) is kept.
func claudeOnFoundryBase(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Port() != "" {
		return endpoint
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range claudeOnFoundryHostSuffixes {
		resource, ok := strings.CutSuffix(host, suffix)
		if ok && resource != "" && !strings.Contains(resource, ".") {
			u.Host = resource + ".services.ai.azure.com"
			return strings.TrimRight(u.String(), "/")
		}
	}
	return endpoint
}

// newClaudeOnFoundry builds the Anthropic-wire provider for the resource's Claude deployments: the Messages
// route on the documented host, the document's credential (tokenFn records the token so withTokenRetry can
// invalidate exactly that one), the guarded Azure client and servedModelFor. Only Chat and Stream are used;
// the Azure provider keeps its identity, listing and pricing — so replay records are tagged with its ID.
func newClaudeOnFoundry(cfg AzureFoundryConfig, client *http.Client, tokenFn func(ctx context.Context) (string, error), servedModelFor func(string) string) *ClaudeProvider {
	p := &ClaudeProvider{
		client:         client,
		messagesURL:    claudeOnFoundryBase(cfg.Endpoint) + "/anthropic/v1/messages",
		servedModelFor: servedModelFor,
		label:          "azure ai foundry (claude)",
		itemsOwner:     azureFoundryProviderID,
	}
	switch cfg.Auth.Mode {
	case AzureAuthModeAPIKey:
		key := cfg.Auth.Key
		p.authorize = func(_ context.Context, req *http.Request) error {
			// Foundry documents both headers for the Anthropic route; send both, as the OpenAI route does, so
			// a gateway that inspects only one still sees the key.
			req.Header.Set("api-key", key)
			req.Header.Set("x-api-key", key)
			return nil
		}
		// API-key mode keeps the bare invalid-key classification (a
		// permission denial keeps its own path), as openaiBase does.
		p.unauthorized = func(_ int, body []byte) error {
			return claudeUnauthorized(decodeAzureError(body), body)
		}
	case AzureAuthModeEntra, AzureAuthModeEntraCLI:
		p.authorize = func(ctx context.Context, req *http.Request) error {
			tok, err := tokenFn(ctx)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			return nil
		}
		// A 401/403 in Entra ID mode is a rejected ACCESS TOKEN — ErrApiKeyInvalid, so withTokenRetry replays
		// once with a fresh token (see entraUnauthorized).
		p.unauthorized = func(_ int, body []byte) error {
			apiErr := decodeAzureError(body)
			if err := entraUnauthorized(apiErr, body); err != nil {
				return err
			}
			return claudeUnauthorized(apiErr, body)
		}
	}
	return p
}

// claudeUnauthorized classifies a 401/403 of the Anthropic route that is not a rejected Entra ID token: an
// Anthropic permission_error keeps the service's explanation on the permission-denied path (no token
// replay, the copy names a missing role); anything else is an invalid credential.
func claudeUnauthorized(apiErr openAIErrorResp, body []byte) error {
	if strings.EqualFold(apiErr.Error.Type, "permission_error") {
		return &permissionDeniedError{msg: permissionDeniedMessage(azureAuthMessage(apiErr, body))}
	}
	return unauthorizedError(apiErr)
}

// claudeEntraOnlyRe matches a Mythos model or deployment name. Mythos ONLY: every other Claude deployment on
// Foundry accepts an API key too, and Fable — which claudeAlwaysThinkingFamilyRe also matches — is not gated.
var claudeEntraOnlyRe = regexp.MustCompile(`(?:^|[^a-z])mythos(?:[^a-z]|$)`)

// ErrClaudeEntraOnly marks a 401/403 an API-key credential got against a Claude Mythos deployment: the key is
// valid for the resource yet can never use this deployment (Entra ID identities only). Unwraps to
// ErrApiKeyInvalid, keeping the no-retry/no-breaker rules of any auth failure.
var ErrClaudeEntraOnly = errors.New("this Claude deployment accepts Microsoft Entra ID only")

// claudeEntraOnlyError is the concrete remap of an API-key refusal on a
// Mythos deployment: names the deployment and tells the user to switch the
// Azure AI Foundry credential to Entra ID.
type claudeEntraOnlyError struct{ deployment string }

func (e *claudeEntraOnlyError) Error() string {
	return "azure ai foundry refused the API key for Claude deployment " + e.deployment + ": Claude Mythos deployments accept Microsoft Entra ID only — switch the Azure AI Foundry credential to Entra ID"
}

func (e *claudeEntraOnlyError) Unwrap() []error {
	return []error{ErrClaudeEntraOnly, ErrApiKeyInvalid}
}

// claudeMythosEntraOnly remaps an API-key 401/403 that hit a Mythos deployment (by name, or by the model the
// served-model memo says it serves) onto claudeEntraOnlyError — "this model requires Entra ID", not "invalid
// API key". Only in API-key mode, only when not already a permission denial, and only after the retry wrapper
// is done: Foundry's exact status/body for a key against Mythos is undocumented, so ANY 401/403 matches.
func (p *AzureFoundryProvider) claudeMythosEntraOnly(deployment string, err error) error {
	if err == nil || p.cfg.Auth.Mode != AzureAuthModeAPIKey {
		return err
	}
	if errors.Is(err, ErrProviderPermissionDenied) || !errors.Is(err, ErrApiKeyInvalid) {
		return err
	}
	if claudeEntraOnlyRe.MatchString(strings.ToLower(deployment)) ||
		claudeEntraOnlyRe.MatchString(strings.ToLower(p.claudeServedModel(deployment))) {
		return &claudeEntraOnlyError{deployment: deployment}
	}
	return err
}

// decodeAzureError reads the error envelope of an Anthropic-format response into the shape the shared Azure
// auth helpers take; API Management's {"statusCode","message"} decodes to an empty message, which
// azureAuthMessage handles.
func decodeAzureError(body []byte) openAIErrorResp {
	var apiErr openAIErrorResp
	_ = json.Unmarshal(body, &apiErr)
	return apiErr
}

// claudeDeploymentEntry is the listing entry for one Claude deployment: a deployment named after its model
// (the portal default) carries that model's window and output cap up front; any other name starts at the
// defaults until the served-model memo or the project listing names its model.
func claudeDeploymentEntry(name string) ModelInfo {
	m := ModelInfo{
		ID:              claudeModelPrefix + name,
		DisplayName:     name + " · Claude",
		ContextLimit:    claudeOnFoundryContextLimit,
		MaxOutputTokens: claudeOnFoundryDefaultOutput,
	}
	if entry, ok := catalogEntryFor("claude", name); ok {
		// No price: Foundry deployments are unpriced (the Azure portal shows the cost).
		if entry.ContextLimit > 0 {
			m.ContextLimit = entry.ContextLimit
		}
		if entry.MaxOutputTokens > 0 {
			m.MaxOutputTokens = entry.MaxOutputTokens
		}
	}
	return m
}

// claudeDeploymentRequest is req addressed to the deployment behind a "claude:<name>"
// id: the Messages route wants the deployment name as `model`.
func claudeDeploymentRequest(req Request) Request {
	req.Model = claudeDeploymentName(req.Model)
	return req
}

// claudeProvider builds the Anthropic-wire provider on first use (one allocation per request that chats with
// a Claude deployment). Tokens and client are read through the Azure provider, so a provider configured
// after construction (tests) is honoured.
func (p *AzureFoundryProvider) claudeProvider() *ClaudeProvider {
	p.claudeOnce.Do(func() {
		p.claude = newClaudeOnFoundry(p.cfg, p.client, p.entraToken, p.claudeServedModel)
	})
	return p.claude
}

// claudeChat runs a Chat against a Claude deployment under the Entra ID
// token retry.
func (p *AzureFoundryProvider) claudeChat(ctx context.Context, req Request) (*Response, error) {
	var resp *Response
	err := p.withTokenRetry(ctx, func(ctx context.Context) error {
		var err error
		resp, err = p.claudeProvider().Chat(ctx, claudeDeploymentRequest(req))
		return err
	})
	return resp, p.claudeMythosEntraOnly(claudeDeploymentName(req.Model), err)
}

// claudeStream runs a Stream against a Claude deployment under the Entra ID
// token retry. A 401 arrives as the HTTP status, before any chunk, so a
// retry never replays a partially delivered stream.
func (p *AzureFoundryProvider) claudeStream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	return p.claudeMythosEntraOnly(claudeDeploymentName(req.Model),
		p.withTokenRetry(ctx, func(ctx context.Context) error {
			return p.claudeProvider().Stream(ctx, claudeDeploymentRequest(req), onChunk)
		}))
}

// CountTokens counts a Claude deployment's prompt exactly through the Anthropic count_tokens route (same
// host and credential, the deployment name as `model`). Non-Claude deployments are refused (the OpenAI route
// has no count endpoint); callers fall back to the estimate.
func (p *AzureFoundryProvider) CountTokens(ctx context.Context, req Request) (int, error) {
	if p.configErr != nil {
		return 0, p.configErr
	}
	id, ok := p.claudeRoute(req.Model)
	if !ok {
		return 0, fmt.Errorf("count_tokens is only available for Claude deployments, not %q", req.Model)
	}
	req.Model = id
	var n int
	err := p.withTokenRetry(ctx, func(ctx context.Context) error {
		var err error
		n, err = p.claudeProvider().CountTokens(ctx, claudeDeploymentRequest(req))
		return err
	})
	return n, err
}
