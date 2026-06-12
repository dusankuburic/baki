package ai

import (
	"context"
	"fmt"
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
			apiKey:        apiKey,
			client:        sharedHTTPClient,
			baseURL:       &glmBaseURL,
			providerLabel: "glm",
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

func (g *GLMProvider) ID() string           { return "glm" }
func (g *GLMProvider) Name() string         { return "GLM (z.ai)" }
func (g *GLMProvider) ContextLimit() int    { return 200000 }
func (g *GLMProvider) DefaultModel() string { return "glm-5.1" }
func (g *GLMProvider) FreeModel() string    { return "glm-4.7-flash" }

func (g *GLMProvider) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 0.01, OutputCostPerM: 0.01}
}

func (g *GLMProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by GLM provider")
}

func (g *GLMProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (g *GLMProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return catalogModels("glm"), nil
}

func (g *GLMProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	return g.openaiBase.chat(ctx, req)
}

func (g *GLMProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	return g.openaiBase.stream(ctx, req, onChunk)
}

func isGLMBalanceError(msg, code string) bool {
	if code == "1113" {
		return true
	}
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "balance") || strings.Contains(msg, "resource package")
}
