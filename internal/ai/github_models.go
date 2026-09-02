package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

var githubModelsBaseURL = providerURL("GITHUB_MODELS_API_URL", "https://models.inference.ai.azure.com/chat/completions")

// ghModelsCache holds the GitHub Models /models listing process-wide: the
// list is a property of the service (same for every authenticated caller),
// and Models() sits on the audited/pricing/context-limit path — without the
// cache, every usage record re-GET the endpoint (each request constructs a
// fresh provider instance, so an instance-level cache like Copilot's
// wouldn't survive). Only successful non-empty listings are cached;
// fallbacks to the static catalog stay uncached so the next call retries.
var (
	ghModelsMu     sync.Mutex
	ghModelsCached []ModelInfo
	ghModelsExpiry time.Time
)

// ghModelsCacheTTL mirrors Copilot's model-list cache duration.
const ghModelsCacheTTL = 1 * time.Hour

type GitHubModelsProvider struct {
	openaiBase
}

func NewGitHubModelsProvider(token string) *GitHubModelsProvider {
	return &GitHubModelsProvider{
		openaiBase: openaiBase{
			apiKey:                token,
			client:                sharedHTTPClient,
			baseURL:               &githubModelsBaseURL,
			providerLabel:         "github models",
			embeddingModel:        "text-embedding-3-small",
			embeddingModelDefault: "text-embedding-3-small",
			extraHeaders: func(req *http.Request, model string) {
				req.Header.Set("x-ms-model-mesh-model-id", model)
			},
		},
	}
}

func (g *GitHubModelsProvider) SupportsTools() bool { return true }

func (g *GitHubModelsProvider) ID() string           { return "github-models" }
func (g *GitHubModelsProvider) Name() string         { return "GitHub Models" }
func (g *GitHubModelsProvider) ContextLimit() int    { return 8192 }
func (g *GitHubModelsProvider) DefaultModel() string { return "gpt-4o" }
func (g *GitHubModelsProvider) FreeModel() string    { return "" }

func (g *GitHubModelsProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 0.0, OutputCostPerM: 0.0}
}

func (g *GitHubModelsProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return g.embed(ctx, text)
}

func (g *GitHubModelsProvider) EstimateTokens(text string) int {
	return EstimateTokens(text)
}

func (g *GitHubModelsProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	ghModelsMu.Lock()
	if time.Now().Before(ghModelsExpiry) {
		res := make([]ModelInfo, len(ghModelsCached))
		copy(res, ghModelsCached)
		ghModelsMu.Unlock()
		return res, nil
	}
	ghModelsMu.Unlock()

	models, live := g.fetchModels(ctx)
	if live {
		ghModelsMu.Lock()
		ghModelsCached = models
		ghModelsExpiry = time.Now().Add(ghModelsCacheTTL)
		ghModelsMu.Unlock()
	}
	// Defensive copy: the cached slice (live path) must not be caller-mutable.
	res := make([]ModelInfo, len(models))
	copy(res, models)
	return res, nil
}

// fetchModels performs the live /models GET with the existing silent
// catalog-fallback semantics. live reports whether the listing came from the
// wire (cacheable) — fallbacks return live=false so a transient outage isn't
// pinned for the cache TTL.
func (g *GitHubModelsProvider) fetchModels(ctx context.Context) (models []ModelInfo, live bool) {
	// No token (metadata provider for an unconfigured user) → the GET would
	// 401 every time. ListProviders calls this synchronously per provider, so
	// skipping the doomed outbound call also keeps the listing fast — and the
	// 401 fallback was never cached (live=false), so it repeated per call.
	if g.apiKey == "" {
		return catalogModels("github-models"), false
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.Replace(githubModelsBaseURL, "/chat/completions", "/models", 1), nil)
	if err != nil {
		return catalogModels("github-models"), false
	}
	req.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.client.Do(req)
	if err != nil {
		return catalogModels("github-models"), false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return catalogModels("github-models"), false
	}

	var parsed []struct {
		Name         string `json:"name"`
		ContextLimit int    `json:"context_limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return catalogModels("github-models"), false
	}

	models = make([]ModelInfo, 0, len(parsed))
	for _, m := range parsed {
		limit := m.ContextLimit
		if limit <= 0 {
			limit = 8192
		}
		models = append(models, ModelInfo{
			ID:           m.Name,
			DisplayName:  m.Name,
			ContextLimit: limit,
			Pricing:      Pricing{InputCostPerM: 0, OutputCostPerM: 0},
		})
	}

	if len(models) == 0 {
		return catalogModels("github-models"), false
	}

	return models, true
}

func (g *GitHubModelsProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	return g.chat(ctx, req)
}

func (g *GitHubModelsProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	return g.stream(ctx, req, onChunk)
}
