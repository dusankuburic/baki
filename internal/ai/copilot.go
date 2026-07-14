package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
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

// CopilotProvider wraps openaiBase: the wire format is OpenAI-compatible, but
// the bearer token is dynamic (a session token that must be resolved — and may
// need exchanging — per call, via tokenFn) rather than a static API key, and
// three extra Copilot-specific headers are required on every request.
type CopilotProvider struct {
	openaiBase

	mu          sync.Mutex
	cached      []ModelInfo
	cacheExpiry time.Time
}

// NewCopilotProvider creates a provider that uses a static token (manual PAT).
func NewCopilotProvider(token string) *CopilotProvider {
	return &CopilotProvider{
		openaiBase: openaiBase{
			tokenFn:       func(_ context.Context) (string, error) { return token, nil },
			client:        sharedHTTPClient,
			baseURL:       &copilotBaseURL,
			providerLabel: "copilot",
			extraHeaders:  func(req *http.Request, _ string) { setCopilotHeaders(req.Header) },
		},
	}
}

// NewCopilotProviderWithAuth creates a provider that resolves a fresh session token via CopilotAuth.
func NewCopilotProviderWithAuth(auth *CopilotAuth, githubToken string) *CopilotProvider {
	return &CopilotProvider{
		openaiBase: openaiBase{
			tokenFn: func(ctx context.Context) (string, error) {
				return auth.GetSessionToken(ctx, githubToken)
			},
			client:        sharedHTTPClient,
			baseURL:       &copilotBaseURL,
			providerLabel: "copilot",
			extraHeaders:  func(req *http.Request, _ string) { setCopilotHeaders(req.Header) },
		},
	}
}

func (p *CopilotProvider) SupportsTools() bool { return false }

func (p *CopilotProvider) ID() string           { return "copilot" }
func (p *CopilotProvider) Name() string         { return "GitHub Copilot" }
func (p *CopilotProvider) ContextLimit() int    { return 128000 }
func (p *CopilotProvider) DefaultModel() string { return "gpt-4o" }
func (p *CopilotProvider) FreeModel() string    { return "" }

func (c *CopilotProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 0.0, OutputCostPerM: 0.0}
}

func (c *CopilotProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by Copilot provider")
}

func (c *CopilotProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (p *CopilotProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	p.mu.Lock()
	if time.Now().Before(p.cacheExpiry) {
		res := make([]ModelInfo, len(p.cached))
		copy(res, p.cached)
		p.mu.Unlock()
		return res, nil
	}
	p.mu.Unlock()

	// If tokenFn returns an empty token, we can't fetch from the API.
	// Return the catalog-hardcoded models as a sensible fallback for
	// unauthenticated callers (e.g. GetMetadataProvider or initial UI load).
	token, _ := p.tokenFn(ctx)
	if token == "" {
		return catalogModels("copilot"), nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", copilotModelsURL, nil)
	if err != nil {
		return nil, err
	}
	// addHeaders will re-call p.tokenFn, which is slightly redundant but safe.
	if err := p.addHeaders(ctx, req); err != nil {
		return nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch copilot models: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot models API error: status %d, body: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Data []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			ContextLimit int    `json:"context_limit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode copilot models: %w, body: %s", err, string(body))
	}

	models := make([]ModelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		// Use a safe default for ContextLimit if not provided by the API
		limit := m.ContextLimit
		if limit <= 0 {
			limit = 128000
		}
		models = append(models, ModelInfo{
			ID:           m.ID,
			DisplayName:  m.Name,
			ContextLimit: limit,
			Pricing:      Pricing{InputCostPerM: 0, OutputCostPerM: 0},
		})
	}

	p.mu.Lock()
	p.cached = models
	p.cacheExpiry = time.Now().Add(1 * time.Hour)
	res := make([]ModelInfo, len(p.cached))
	copy(res, p.cached)
	p.mu.Unlock()

	return res, nil
}

// addHeaders is used only by Models (a plain GET, not the chat/stream wire
// format base.chat/base.stream own). Chat and Stream delegate to
// openaiBase, which builds its own headers via resolveToken + extraHeaders.
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
