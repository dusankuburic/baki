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

// Claude on Azure AI Foundry. Claude deployments — Fable 5.1 and 5, Mythos
// 5.1 and 5 (limited availability), Opus 5.5, 5, 4.8, 4.7, 4.6 and 4.5,
// Sonnet 5.5, 5, 4.6 and 4.5, Haiku 4.5 (Anthropic's Foundry page, "API model
// IDs and deployments"); Azure Marketplace billing, a paid pay-as-you-go
// subscription is required — are NOT reachable through the resource's
// /openai/v1 route (404): they answer on the Anthropic Messages API at
//
//	POST https://{resource}.services.ai.azure.com/anthropic/v1/messages
//
// with the header anthropic-version: 2023-06-01 and either api-key / x-api-key:
// <key> (API-key mode; both are accepted, both are sent) or
// Authorization: Bearer <Entra ID token> (scope
// https://ai.azure.com/.default), `model` being the DEPLOYMENT name; standard
// Anthropic streaming, tools, adaptive thinking and error envelopes; 429 with
// retry-after; request-id and apim-request-id on every response. That host
// is the only one the route is documented on, so a document whose endpoint
// is the same resource's *.openai.azure.com or *.cognitiveservices.azure.com
// host is sent to <resource>.services.ai.azure.com (claudeOnFoundryBase); any
// other allow-listed host (an operator's API Management front) is used as-is.
//
// The chat model picker offers each Claude deployment as "claude:<name>"
// (claudeModelPrefix): the prefix is what routes the call to the Anthropic
// wire format (ClaudeProvider with the seams below) instead of openaiBase.

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
	// claudeOnFoundryContextLimit is the context window of a Claude
	// deployment whose model the catalogue does not know yet: 200k is what
	// every Claude model on Foundry accepts. Catalogued models use their own
	// window — Anthropic's Foundry page lists 1M for Opus/Sonnet 4.6+ and the
	// 5.x models and 200k for the others (Sonnet 4.5, Haiku 4.5), matching
	// the catalogue.
	claudeOnFoundryContextLimit = 200000
	// claudeOnFoundryDefaultOutput is the output cap of a Claude deployment
	// whose model the catalogue does not know (a custom-named deployment
	// before its first answer): the smallest output cap of any Claude model
	// Foundry serves, so the reasoning floor is not clipped for an unlisted
	// deployment (audit #9 F-04). The chat service still caps at the user's
	// Max tokens and at the served model's own cap once known.
	claudeOnFoundryDefaultOutput = 64000
	// anthropicModelPublisher is the project deployments listing's
	// modelPublisher for Claude deployments.
	anthropicModelPublisher = "Anthropic"
)

// claudeOnFoundryHostSuffixes are the resource host suffixes whose Claude
// route lives on the same resource's services.ai.azure.com host.
var claudeOnFoundryHostSuffixes = []string{".openai.azure.com", ".cognitiveservices.azure.com"}

// claudeOnFoundryBase is the base URL of the resource's Anthropic route: the
// endpoint itself, except that https://<resource>.openai.azure.com and
// https://<resource>.cognitiveservices.azure.com (a single-label resource
// name — the names Azure issues) become https://<resource>.services.ai.azure.com,
// the host Anthropic documents for Claude in Microsoft Foundry. Every other
// host — services.ai.azure.com itself, an operator's API Management front
// (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS), a multi-label private DNS name — is kept.
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

// newClaudeOnFoundry builds the Anthropic-wire provider for the resource's
// Claude deployments: the Messages route on the resource's documented host
// (claudeOnFoundryBase), the document's credential (tokenFn in Entra ID mode
// — the Azure provider's entraToken, which records the token on the call's
// context so withTokenRetry can invalidate exactly that one), the guarded
// Azure client, and servedModelFor, which names the model behind a
// deployment for the sampling policy. Only Chat and Stream are used; the
// Azure provider keeps its own identity, listing and pricing — the chat
// service's tool loop therefore tags this route's replay records with the
// Azure provider's ID (itemsOwner).
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
			// Foundry documents both headers for the Anthropic route; send
			// both, as the OpenAI route does, so a gateway (API Management)
			// that inspects only one of them still sees the key.
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
		// A 401/403 in Entra ID mode is a rejected ACCESS TOKEN carrying
		// Azure's explanation — ErrEntraTokenRejected and ErrApiKeyInvalid,
		// so withTokenRetry replays once with a fresh token and the chat
		// copy names the principal, not an API key (see entraUnauthorized).
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

// claudeUnauthorized classifies a 401/403 of the Anthropic route that is not
// a rejected Entra ID token: an Anthropic permission_error (the credential
// was accepted, the identity may not use the deployment) or Azure's
// PermissionDenied code keeps the service's explanation on the
// permission-denied path (ErrProviderPermissionDenied — no token replay, the
// chat copy names a missing role); anything else, an Anthropic
// authentication_error included, is an invalid credential.
func claudeUnauthorized(apiErr openAIErrorResp, body []byte) error {
	if strings.EqualFold(apiErr.Error.Type, "permission_error") {
		return &permissionDeniedError{msg: permissionDeniedMessage(azureAuthMessage(apiErr, body))}
	}
	return unauthorizedError(apiErr)
}

// claudeEntraOnlyRe matches a Mythos model or deployment name ("claude-mythos-5-1",
// a custom deployment serving "claude-mythos-preview"). Mythos ONLY: every
// other Claude deployment on Foundry accepts an API key too, and Fable —
// which claudeAlwaysThinkingFamilyRe also matches — is not Entra-gated.
var claudeEntraOnlyRe = regexp.MustCompile(`(?:^|[^a-z])mythos(?:[^a-z]|$)`)

// ErrClaudeEntraOnly marks a 401/403 an API-key credential got against a
// Claude Mythos deployment: Foundry serves Mythos to Microsoft Entra ID
// identities only, so the key is valid for the resource yet can never use
// this deployment. The chat service renders its own copy for it (the
// connection-test and settings-hint halves of audit #9 F-26); it also
// unwraps to ErrApiKeyInvalid, keeping the no-retry/no-breaker rules of any
// auth failure.
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

// claudeMythosEntraOnly remaps an API-key 401/403 that hit a Mythos
// deployment (by name, or by the model the served-model memo says it serves)
// onto claudeEntraOnlyError, so the user is told "this model requires Entra
// ID" instead of "invalid API key" — the key is valid for every other
// deployment on the resource. Only in API-key mode, only when the refusal was
// NOT already a permission denial (which carries Azure's own explanation),
// and only after the retry wrapper is done: nothing about the call is
// retried differently. Foundry's exact status/body for a key against Mythos
// is undocumented, so ANY 401/403 on the invalid-key path matches.
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

// decodeAzureError reads the error envelope of an Anthropic-format response
// ({"type":"error","error":{"type","message"}}) into the shape the shared
// Azure auth helpers take; API Management's {"statusCode","message"} shape
// decodes to an empty message, which azureAuthMessage handles.
func decodeAzureError(body []byte) openAIErrorResp {
	var apiErr openAIErrorResp
	_ = json.Unmarshal(body, &apiErr)
	return apiErr
}

// claudeDeploymentEntry is the listing entry for one Claude deployment. A
// deployment named after its model (the portal default, optionally with a
// trailing version date such as "claude-sonnet-4-6-20260301") carries that
// model's window and output cap up front; any other name starts with the
// default window and output cap until the served-model memo or the project listing names its model.
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

// claudeProvider builds the Anthropic-wire provider on first use. The
// factory builds a fresh Azure provider per request, so this is one
// allocation per request that chats with a Claude deployment; tokens and
// client are read through the Azure provider (entraToken, the openaiBase
// client), so a provider configured after construction (tests) is honoured.
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

// CountTokens counts a Claude deployment's prompt exactly through the
// Anthropic count_tokens route (ai.TokenCounter): same host as the Messages
// route, same credential (api-key/x-api-key or a Bearer Entra token), the
// deployment name as `model`. Non-Claude deployments are refused — the
// OpenAI route has no count endpoint — and callers fall back to the
// estimate. An invalid stored document is refused like every other network
// method: its credential must never reach a host Validate rejected.
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
