package ai

import (
	"context"
	"encoding/json"
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
	// SupportsTools reports whether the provider's Chat path can serialize tool
	// definitions and return tool calls. The chat service only runs the agentic
	// tool loop against providers that return true.
	SupportsTools() bool
}

type EmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

type ModelInfo struct {
	ID           string  `json:"id"`
	DisplayName  string  `json:"displayName"`
	ContextLimit int     `json:"contextLimit"`
	// MaxOutputTokens is the model's maximum completion length, which is far
	// smaller than ContextLimit (the input window). Zero means unknown — callers
	// that clamp MaxTokens against it should leave the value untouched in that
	// case. Used by the service to keep a caller's MaxTokens under the provider's
	// real output ceiling so the request doesn't 400.
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
	Pricing         Pricing `json:"pricing"`
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
	// Tools, when non-empty and the provider SupportsTools, are offered to the
	// model so it can request a tool call instead of (or before) a text answer.
	Tools []ToolDefinition `json:"tools,omitempty"`
	// OrgID is request metadata (not sent to any provider API) used by the
	// audited wrapper to attribute usage/cost to an organization, enabling
	// org-wide daily budgets. Empty for personal/non-org flows.
	OrgID string `json:"-"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is set on an assistant message that requested tool calls; it is
	// echoed back in the next turn's history so the provider can correlate the
	// following tool results.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// ToolCallID is set on a Role=="tool" message carrying a tool result; it
	// references the ToolCall.ID this result answers.
	ToolCallID string `json:"toolCallId,omitempty"`
}

type Response struct {
	Content      string `json:"content"`
	TokensIn     int    `json:"tokensIn"`
	TokensOut    int    `json:"tokensOut"`
	FinishReason string `json:"finishReason"`
	// ToolCalls is non-empty when the model requested tool execution instead of
	// returning a final answer; the chat service runs them and continues the loop.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
}

// ToolDefinition describes a tool offered to the model. InputSchema is a JSON
// Schema object describing the tool's parameters.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolCall is a model request to invoke a tool. Input is the raw JSON arguments.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type Chunk struct {
	Text      string `json:"text"`
	TokensIn  int    `json:"tokensIn,omitempty"`
	TokensOut int    `json:"tokensOut,omitempty"`
	Done      bool   `json:"done"`
	Error     error  `json:"error,omitempty"`
	// ToolCalls is populated on the terminal (Done) chunk when the model
	// requested tool execution mid-stream instead of finishing with text. Empty
	// for a normal text completion. The agentic tool loop reads it to decide
	// whether to run tools and continue, or finalize the answer.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// FinishReason mirrors the provider stop reason on the Done chunk (optional).
	FinishReason string `json:"finishReason,omitempty"`
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

// ModelMaxOutputTokens returns the maximum completion length for a specific
// model, or 0 (unknown) when the model is absent from the catalog or its
// MaxOutputTokens is unset. Callers treat 0 as "don't clamp".
func ModelMaxOutputTokens(p Provider, model string) int {
	for _, m := range p.Models() {
		if m.ID == model {
			return m.MaxOutputTokens
		}
	}
	return 0
}
