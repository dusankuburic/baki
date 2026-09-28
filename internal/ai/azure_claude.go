package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Claude on Azure AI Foundry. Claude deployments (Opus/Sonnet/Haiku 4.x —
// Azure Marketplace billing, a paid pay-as-you-go subscription is required)
// are NOT reachable through the resource's /openai/v1 route (404): they
// answer on the Anthropic Messages API at
//
//	POST {endpoint}/anthropic/v1/messages
//
// with the header anthropic-version: 2023-06-01 and either x-api-key: <key>
// (API-key mode) or Authorization: Bearer <Entra ID token> (scope
// https://ai.azure.com/.default; the Mythos tier is Entra ID only), `model`
// being the DEPLOYMENT name; standard Anthropic streaming, tools, adaptive
// thinking and error envelopes; 429 with retry-after. The route is documented
// on the *.services.ai.azure.com host; whether a *.openai.azure.com endpoint
// also serves /anthropic is unverified, so the document's endpoint host is
// used as-is and the smoke test decides.
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
	// claudeOnFoundryContextLimit is the context window every Claude 4.x
	// deployment is sized at: 200k is what all of them accept; the larger
	// windows some models offer on api.anthropic.com are not verified on
	// Foundry, and a rejection teaches the learned window anyway.
	claudeOnFoundryContextLimit = 200000
	// claudeOnFoundryDefaultOutput is the output cap of a Claude deployment
	// whose model the catalogue does not know (a custom-named deployment
	// before its first answer).
	claudeOnFoundryDefaultOutput = 8192
	// anthropicModelPublisher is the project deployments listing's
	// modelPublisher for Claude deployments.
	anthropicModelPublisher = "Anthropic"
)

// newClaudeOnFoundry builds the Anthropic-wire provider for the resource's
// Claude deployments: the Messages route on the document's endpoint, the
// document's credential (tokenFn in Entra ID mode — the Azure provider's
// entraToken, which remembers the token so withTokenRetry can invalidate
// exactly that one), and the guarded Azure client. Only Chat and Stream are
// used; the Azure provider keeps its own identity, listing and pricing.
func newClaudeOnFoundry(cfg AzureFoundryConfig, client *http.Client, tokenFn func(ctx context.Context) (string, error)) *ClaudeProvider {
	p := &ClaudeProvider{
		client:      client,
		messagesURL: cfg.Endpoint + "/anthropic/v1/messages",
	}
	switch cfg.Auth.Mode {
	case AzureAuthModeAPIKey:
		key := cfg.Auth.Key
		p.authorize = func(_ context.Context, req *http.Request) error {
			req.Header.Set("x-api-key", key)
			return nil
		}
		// API-key mode keeps the bare invalid-key classification (a
		// PermissionDenied code keeps its own path), as openaiBase does.
		p.unauthorized = func(_ int, body []byte) error {
			return unauthorizedError(decodeAzureError(body))
		}
	case AzureAuthModeEntra:
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
			return unauthorizedError(apiErr)
		}
	}
	return p
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
// model's price and output cap up front; any other name starts unpriced
// (reserved and billed at AliasClaudeCeiling) with the default output cap
// until the served-model memo or the project listing names its model.
func claudeDeploymentEntry(name string) ModelInfo {
	m := ModelInfo{
		ID:              claudeModelPrefix + name,
		DisplayName:     name + " · Claude",
		ContextLimit:    claudeOnFoundryContextLimit,
		MaxOutputTokens: claudeOnFoundryDefaultOutput,
	}
	if entry, ok := catalogEntryFor("claude", name); ok {
		m.Pricing = entry.Pricing
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
		p.claude = newClaudeOnFoundry(p.cfg, p.client, p.entraToken)
	})
	return p.claude
}

// claudeChat runs a Chat against a Claude deployment under the Entra ID
// token retry.
func (p *AzureFoundryProvider) claudeChat(ctx context.Context, req Request) (*Response, error) {
	var resp *Response
	err := p.withTokenRetry(ctx, func() error {
		var err error
		resp, err = p.claudeProvider().Chat(ctx, claudeDeploymentRequest(req))
		return err
	})
	return resp, err
}

// claudeStream runs a Stream against a Claude deployment under the Entra ID
// token retry. A 401 arrives as the HTTP status, before any chunk, so a
// retry never replays a partially delivered stream.
func (p *AzureFoundryProvider) claudeStream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	return p.withTokenRetry(ctx, func() error {
		return p.claudeProvider().Stream(ctx, claudeDeploymentRequest(req), onChunk)
	})
}
