package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

var githubModelsBaseURL = providerURL("GITHUB_MODELS_API_URL", "https://models.inference.ai.azure.com/chat/completions")

type GitHubModelsProvider struct {
	openaiBase
}

func NewGitHubModelsProvider(token string) *GitHubModelsProvider {
	return &GitHubModelsProvider{
		openaiBase: openaiBase{
			apiKey:         token,
			client:         sharedHTTPClient,
			baseURL:        &githubModelsBaseURL,
			providerLabel:  "github models",
			embeddingModel: "text-embedding-3-small",
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
	req, err := http.NewRequestWithContext(ctx, "GET", strings.Replace(githubModelsBaseURL, "/chat/completions", "/models", 1), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.client.Do(req)
	if err != nil {
		return catalogModels("github-models"), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return catalogModels("github-models"), nil
	}

	var parsed []struct {
		Name         string `json:"name"`
		ContextLimit int    `json:"context_limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return catalogModels("github-models"), nil
	}

	models := make([]ModelInfo, 0, len(parsed))
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
		return catalogModels("github-models"), nil
	}

	return models, nil
}

func (g *GitHubModelsProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	return g.chat(ctx, req)
}

func (g *GitHubModelsProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	return g.stream(ctx, req, onChunk)
}
