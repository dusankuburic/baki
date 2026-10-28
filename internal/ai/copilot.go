package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var copilotBaseURL = providerURL("COPILOT_API_URL", "https://api.githubcopilot.com/chat/completions")
var copilotModelsURL = providerURL("COPILOT_MODELS_API_URL", "https://api.githubcopilot.com/models")

const (
	copilotIntegrationID = "vscode-chat"
	copilotEditorVersion = "vscode/1.95.0"
	copilotPluginVersion = "copilot-chat/0.22.0"
)

func setCopilotHeaders(h http.Header) {
	h.Set("Copilot-Integration-Id", copilotIntegrationID)
	h.Set("Editor-Version", copilotEditorVersion)
	h.Set("Editor-Plugin-Version", copilotPluginVersion)
}

// CopilotProvider wraps openaiBase: OpenAI-compatible wire format, but the
// bearer token is dynamic (resolved, possibly exchanged, per call via
// tokenFn) and three Copilot-specific headers ride every request.
type CopilotProvider struct {
	openaiBase

	// modelsKey identifies the credential for the process-wide listing cache
	// (a hash, never the token itself); "" when the provider holds no
	// credential (the metadata provider).
	modelsKey string
}

// copilotModels caches Copilot model listings process-wide (modelListCache),
// keyed by the models URL and a credential hash — the listing is
// subscription-scoped, never shared between tokens. factory.forCopilot builds
// a fresh provider per request, so the instance-level cache this replaced was
// always cold while chat consults Models() before every turn (modelOutputInfo).
var copilotModels = newModelListCache(copilotModelsCacheSize)

// Listing cache tuning: success kept an hour (as the instance cache was), a
// failed fetch a few seconds only — an outage is retried promptly, not once
// per turn. Vars so tests can shorten them.
var (
	copilotModelsCacheTTL    = 1 * time.Hour
	copilotModelsErrCacheTTL = 15 * time.Second
)

// copilotModelsCacheSize bounds the listing cache (one entry per credential).
const copilotModelsCacheSize = 512

// copilotErrorBodyMax bounds the read of a failed listing's body: only its first line (at most 500 bytes) is kept.
const copilotErrorBodyMax = 4 << 10

// copilotRequestIDSuffix names GitHub's request id (else the generic upstream ones), so a failure can be correlated
// with GitHub's side; "" when the response carries none.
func copilotRequestIDSuffix(resp *http.Response) string {
	rid := resp.Header.Get("X-GitHub-Request-Id")
	if rid == "" {
		rid = upstreamRequestID(resp)
	}
	if rid = oneLine(rid, 128); rid == "" {
		return ""
	}
	return ", request id " + rid
}

// NewCopilotProvider creates a provider that uses a static token (manual PAT).
func NewCopilotProvider(token string) *CopilotProvider {
	key := ""
	if token != "" {
		key = "pat:" + tokenHash(token)
	}
	return &CopilotProvider{
		modelsKey: key,
		openaiBase: openaiBase{
			tokenFn:        func(_ context.Context) (string, error) { return token, nil },
			client:         fixedHostClient(),
			longWaitClient: fixedHostClientFor(false),
			handle429:      quotaExhausted,
			baseURL:        &copilotBaseURL,
			providerLabel:  "copilot",
			providerID:     copilotProviderID,
			extraHeaders:   func(req *http.Request, _ string) { setCopilotHeaders(req.Header) },
		},
	}
}

// NewCopilotProviderWithAuth creates a provider that resolves a fresh session token via CopilotAuth.
func NewCopilotProviderWithAuth(auth *CopilotAuth, githubToken string) *CopilotProvider {
	key := ""
	if githubToken != "" {
		key = "oauth:" + tokenHash(githubToken)
	}
	return &CopilotProvider{
		modelsKey: key,
		openaiBase: openaiBase{
			tokenFn: func(ctx context.Context) (string, error) {
				return auth.GetSessionToken(ctx, githubToken)
			},
			client:         fixedHostClient(),
			longWaitClient: fixedHostClientFor(false),
			handle429:      quotaExhausted,
			baseURL:        &copilotBaseURL,
			providerLabel:  "copilot",
			providerID:     copilotProviderID,
			extraHeaders:   func(req *http.Request, _ string) { setCopilotHeaders(req.Header) },
		},
	}
}

func (p *CopilotProvider) SupportsTools() bool { return false }

func (p *CopilotProvider) ID() string           { return copilotSpec.ID }
func (p *CopilotProvider) Name() string         { return copilotSpec.Name }
func (p *CopilotProvider) ContextLimit() int    { return 128000 }
func (p *CopilotProvider) DefaultModel() string { return copilotSpec.DefaultModel }
func (p *CopilotProvider) FreeModel() string    { return "" }

func (c *CopilotProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by Copilot provider")
}

func (c *CopilotProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

// Models returns the caller's Copilot model listing from the process-wide
// cache (copilotModels), fetching it on a miss.
func (p *CopilotProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	if p.modelsKey == "" {
		// No credential: the catalog fallback, never cached (fetchModels).
		models, _, err := p.fetchModels(ctx)
		return models, err
	}
	return copilotModels.load(ctx, copilotModelsURL+"\x00"+p.modelsKey, copilotModelsCacheTTL, copilotModelsErrCacheTTL, p.fetchModels)
}

// fetchModels is the live listing GET (uncached). live is false for the
// catalog fallback, which is never cached: the next call retries.
func (p *CopilotProvider) fetchModels(ctx context.Context) (models []ModelInfo, live bool, err error) {
	// Empty token: return the catalog fallback for unauthenticated callers
	// (GetMetadataProvider, initial UI load) instead of a guaranteed 401.
	token, _ := p.tokenFn(ctx)
	if token == "" {
		return catalogModels(copilotSpec.Catalog), false, nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", copilotModelsURL, nil)
	if err != nil {
		return nil, false, err
	}
	// addHeaders will re-call p.tokenFn, which is slightly redundant but safe.
	if err := p.addHeaders(ctx, req); err != nil {
		return nil, false, err
	}

	resp, err := doProviderRequest(p.client, req)
	if err != nil {
		return nil, false, fmt.Errorf("fetch copilot models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The error is cached for copilotModelsErrCacheTTL and logged by every listing in that window: the status, one
		// line of the body and the request id, never the upstream's page (R-observability-16).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, copilotErrorBodyMax))
		return nil, false, fmt.Errorf("copilot models API error: status %d%s, body: %s", resp.StatusCode, copilotRequestIDSuffix(resp), oneLine(string(body), 500))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	var parsed struct {
		Data []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			ContextLimit int    `json:"context_limit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false, fmt.Errorf("decode copilot models%s: %w, body: %s", copilotRequestIDSuffix(resp), err, oneLine(string(body), 500))
	}

	models = make([]ModelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		limit := m.ContextLimit
		if limit <= 0 {
			limit = 128000
		}
		models = append(models, ModelInfo{
			ID:           m.ID,
			DisplayName:  m.Name,
			ContextLimit: limit,
		})
	}

	return models, true, nil
}

// addHeaders is used only by Models (a plain GET) — chat and stream delegate
// to openaiBase, which builds its own headers via resolveToken + extraHeaders.
func (p *CopilotProvider) addHeaders(ctx context.Context, req *http.Request) error {
	token, err := p.resolveToken(ctx)
	if err != nil {
		return fmt.Errorf("get copilot token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	setCopilotHeaders(req.Header)
	return nil
}

func (p *CopilotProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	return p.chat(ctx, req)
}

func (p *CopilotProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	return p.stream(ctx, req, onChunk)
}
