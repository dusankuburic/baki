package ai

import (
	"context"
	"net/http"
	"strings"
)

var glmBaseURL = providerURL("GLM_API_URL", "https://api.z.ai/api/paas/v4/chat/completions")

type GLMProvider struct {
	openaiBase
}

func NewGLMProvider(apiKey string) *GLMProvider {
	return &GLMProvider{
		openaiBase: openaiBase{
			apiKey:                apiKey,
			client:                sharedHTTPClient,
			baseURL:               &glmBaseURL,
			providerLabel:         "glm",
			embeddingModel:        "embedding-3",
			embeddingModelDefault: "embedding-3",
			handle429: func(_ *http.Response, apiErr openAIErrorResp) error {
				if isGLMBalanceError(apiErr.Error.Message, apiErr.Error.Code) {
					return ErrInsufficientBalance
				}
				return nil
			},
		},
	}
}

func (g *GLMProvider) SupportsTools() bool { return true }

func (g *GLMProvider) ID() string           { return glmSpec.ID }
func (g *GLMProvider) Name() string         { return glmSpec.Name }
func (g *GLMProvider) ContextLimit() int    { return 200000 }
func (g *GLMProvider) DefaultModel() string { return glmSpec.DefaultModel }

// No free GLM model remains in the catalog since the 4.7 family was dropped;
// empty matches the other pay-only providers (the connection test then uses
// the default model alone).
func (g *GLMProvider) FreeModel() string { return "" }

// PricePerMillionTokens is the fallback price used to meter out-of-catalog models (audited.record): the
// default model's catalogue row, so it cannot drift (a stale/lowball value once silently under-billed usage
// and kept the daily budget from ever tripping for models the catalogue had not caught up to).
func (g *GLMProvider) PricePerMillionTokens() Pricing { return specDefaultPricing(glmSpec) }

func (g *GLMProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return g.embed(ctx, text)
}

func (g *GLMProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (g *GLMProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return catalogModels(glmSpec.Catalog), nil
}

func (g *GLMProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	return g.chat(ctx, req)
}

func (g *GLMProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	return g.stream(ctx, req, onChunk)
}

func isGLMBalanceError(msg, code string) bool {
	if code == "1113" {
		return true
	}
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "balance") || strings.Contains(msg, "resource package")
}
