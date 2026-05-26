package ai

import (
	"context"
	"errors"
	"fmt"
)

type Provider interface {
	ID() string
	Name() string
	Models() []ModelInfo
	DefaultModel() string
	FreeModel() string
	Chat(ctx context.Context, req Request) (*Response, error)
	Stream(ctx context.Context, req Request, onChunk func(Chunk)) error
	EstimateTokens(text string) int
	ContextLimit() int
	PricePerMillionTokens() Pricing
}

type ModelInfo struct {
	ID           string  `json:"id"`
	DisplayName  string  `json:"displayName"`
	ContextLimit int     `json:"contextLimit"`
	Pricing      Pricing `json:"pricing"`
}

type Pricing struct {
	InputCostPerM  float64 `json:"inputCostPerM"`
	OutputCostPerM float64 `json:"outputCostPerM"`
}

type Request struct {
	Model        string    `json:"model"`
	Messages     []Message `json:"messages"`
	SystemPrompt string    `json:"systemPrompt,omitempty"`
	Temperature  float64   `json:"temperature,omitempty"`
	MaxTokens    int       `json:"maxTokens,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Response struct {
	Content      string `json:"content"`
	TokensIn     int    `json:"tokensIn"`
	TokensOut    int    `json:"tokensOut"`
	FinishReason string `json:"finishReason"`
}

type Chunk struct {
	Text      string `json:"text"`
	TokensIn  int    `json:"tokensIn,omitempty"`
	TokensOut int    `json:"tokensOut,omitempty"`
	Done      bool   `json:"done"`
	Error     error  `json:"error,omitempty"`
}

var (
	ErrApiKeyInvalid       = errors.New("invalid API key")
	ErrRateLimited         = errors.New("rate limited")
	ErrContextLimit        = errors.New("context limit exceeded")
	ErrProviderDown        = errors.New("provider is currently unavailable")
	ErrInsufficientBalance = errors.New("insufficient balance")
)

func orDefault(val, def int) int {
	if val <= 0 {
		return def
	}
	return val
}

func convertMessages(msgs []Message, toRoleFn func(string) string) []messageRole {
	out := make([]messageRole, len(msgs))
	for i, m := range msgs {
		out[i] = messageRole{Role: toRoleFn(m.Role), Content: m.Content}
	}
	return out
}

type messageRole struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func ProviderError(provider string, code int, msg string) error {
	return fmt.Errorf("%s API status %d: %s", provider, code, msg)
}
