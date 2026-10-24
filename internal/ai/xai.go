package ai

import (
	"context"
	"fmt"
)

var xaiBaseURL = providerURL("XAI_API_URL", "https://api.x.ai/v1/chat/completions")

type XAIProvider struct {
	openaiBase
}

func NewXAIProvider(apiKey string) *XAIProvider {
	return &XAIProvider{
		openaiBase: openaiBase{
			apiKey:        apiKey,
			client:        fixedHostClient(),
			baseURL:       &xaiBaseURL,
			providerLabel: "xai",
		},
	}
}

func (x *XAIProvider) SupportsTools() bool { return true }

func (x *XAIProvider) ID() string           { return xaiSpec.ID }
func (x *XAIProvider) Name() string         { return xaiSpec.Name }
func (x *XAIProvider) ContextLimit() int    { return 131072 }
func (x *XAIProvider) DefaultModel() string { return xaiSpec.DefaultModel }
func (x *XAIProvider) FreeModel() string    { return "" }

func (x *XAIProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by xAI provider")
}

func (x *XAIProvider) EstimateTokens(text string) int {
	return EstimateTokensOpenAI(text)
}

func (x *XAIProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return catalogModels(xaiSpec.Catalog), nil
}

func (x *XAIProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	return x.chat(ctx, req)
}

func (x *XAIProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	return x.stream(ctx, req, onChunk)
}
