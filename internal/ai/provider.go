package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
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
	Embed(ctx context.Context, text []string) ([][]float32, error)
}

type EmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
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
	ErrKeyNotConfigured    = errors.New("API key not configured")
	ErrRateLimited         = errors.New("rate limited")
	ErrContextLimit        = errors.New("context limit exceeded")
	ErrProviderDown        = errors.New("provider is currently unavailable")
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrCircuitOpen         = errors.New("provider circuit open: too many recent failures")
)

// RateLimitError is a rate-limit (HTTP 429) error that carries the server's
// Retry-After hint. It unwraps to ErrRateLimited so existing errors.Is checks
// (isRetryable, the circuit breaker) keep working, while the retry backoff can
// type-assert it to wait at least RetryAfter before the next attempt.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// rateLimitErr builds a *RateLimitError from a 429 response, parsing the
// Retry-After header (delta-seconds or an HTTP-date). A missing/invalid header
// yields a zero RetryAfter, so the caller falls back to its default backoff.
func rateLimitErr(resp *http.Response) error {
	return &RateLimitError{RetryAfter: parseRetryAfter(resp)}
}

func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// retryAfterFrom extracts the Retry-After hint from an error chain, if any.
func retryAfterFrom(err error) time.Duration {
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	return 0
}

// MetadataProvider exposes read-only provider information without API credentials.
// Returned by GetMetadataProvider — it intentionally omits Chat and Stream to
// prevent callers from accidentally issuing real API calls with an empty key.
type MetadataProvider interface {
	ID() string
	Name() string
	Models() []ModelInfo
	DefaultModel() string
	FreeModel() string
	ContextLimit() int
	PricePerMillionTokens() Pricing
	EstimateTokens(text string) int
}

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

// ModelContextLimit returns the context limit for a specific model, falling
// back to the provider-wide limit when the model is not in the model list.
func ModelContextLimit(p Provider, model string) int {
	for _, m := range p.Models() {
		if m.ID == model && m.ContextLimit > 0 {
			return m.ContextLimit
		}
	}
	return p.ContextLimit()
}
