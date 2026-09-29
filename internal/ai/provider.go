package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Provider interface {
	ID() string
	Name() string
	Models(ctx context.Context) ([]ModelInfo, error)
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
	ID           string `json:"id"`
	DisplayName  string `json:"displayName"`
	ContextLimit int    `json:"contextLimit"`
	// MaxOutputTokens is the model's maximum completion length, which is far
	// smaller than ContextLimit (the input window). Zero means unknown — callers
	// that clamp MaxTokens against it should leave the value untouched in that
	// case. Used by the service to keep a caller's MaxTokens under the provider's
	// real output ceiling so the request doesn't 400.
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
	Pricing         Pricing `json:"pricing"`
	// ServedModel is the underlying model that actually answers requests sent
	// to this entry when the entry's ID is only an alias for it — an Azure AI
	// Foundry deployment name, a model-router deployment. Empty for catalogue
	// models, whose ID is the model.
	ServedModel string `json:"servedModel,omitempty"`
	// Embedding marks an embedding deployment (Azure AI Foundry): kept in
	// Models() so embedding usage is priced and sized, never offered as a
	// chat model or chosen as the chat default.
	Embedding bool `json:"embedding,omitempty"`
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
	// TemperatureSet marks Temperature as explicitly chosen by the caller, so
	// a provider sends it even when it is 0 (deterministic sampling). False
	// keeps the legacy rule: a zero Temperature means "provider default" and
	// is omitted from the wire request.
	TemperatureSet bool `json:"-"`
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
	// ProviderItems is the provider's own record of an assistant turn,
	// opaque to everything but the provider that produced it: Claude's
	// ordered content blocks (thinking and redacted_thinking with their
	// signatures, text, tool_use) or a Responses API run's ordered output
	// items (encrypted reasoning, hosted tool calls, function calls). It is
	// set only by the chat service's tool loop, on the assistant turn it
	// appends, and only for the provider (ProviderItemsFrom) that answered;
	// the provider replays it verbatim instead of rebuilding the turn from
	// Content/ToolCalls, which is how thinking and reasoning survive a tool
	// step. Never persisted, never sent to the client, and stripped from the
	// whole history as soon as the loop rewrites earlier messages
	// (truncation, tool fallback): replaying it after an edit is rejected.
	ProviderItems json.RawMessage `json:"-"`
	// ProviderItemsFrom is the provider ID that produced ProviderItems; a
	// provider ignores items another provider wrote.
	ProviderItemsFrom string `json:"-"`
}

type Response struct {
	Content      string `json:"content"`
	TokensIn     int    `json:"tokensIn"`
	TokensOut    int    `json:"tokensOut"`
	FinishReason string `json:"finishReason"`
	// ToolCalls is non-empty when the model requested tool execution instead of
	// returning a final answer; the chat service runs them and continues the loop.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// ServedModel is the model name the provider reported in its response
	// (OpenAI-compatible APIs echo the concrete version, e.g.
	// "gpt-4o-2024-11-20"; an Azure model-router deployment reports the model
	// it routed to). Used to price usage when the request model is a
	// deployment alias the catalogue cannot know. Empty when not reported.
	ServedModel string `json:"servedModel,omitempty"`
	// Reasoning is inline model reasoning split off the answer — a leading
	// <think>…</think> block (DeepSeek-R1 and other open reasoning models on
	// OpenAI-compatible chat completions). Not part of Content: never shown,
	// stored or re-sent as history.
	Reasoning string `json:"reasoning,omitempty"`
	// ProviderItems is the provider's ordered record of this assistant turn
	// for verbatim replay within a tool loop (see Message.ProviderItems).
	ProviderItems json.RawMessage `json:"-"`
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

// Chunk is one streamed event. A chunk with no field set is a keepalive: the
// provider is still working (reasoning, assembling a tool call) but has
// nothing to deliver — consumers only use it to reset their idle timers.
type Chunk struct {
	Text string `json:"text"`
	// Reasoning is inline model reasoning split off the answer (see
	// Response.Reasoning). It is not answer text: the chat service never
	// displays, stores or re-sends it; the audited wrapper counts it toward
	// the output-token estimate of a stream that ends without usage.
	Reasoning string `json:"reasoning,omitempty"`
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
	// ServedModel mirrors Response.ServedModel on the Done chunk (optional).
	ServedModel string `json:"servedModel,omitempty"`
	// ToolEvent reports a tool the PROVIDER ran server-side (an Azure AI
	// Foundry agent's web search, file search, MCP or code-interpreter call):
	// one chunk when the step starts (Done=false) and one when it ends
	// (Done=true). Nothing for the chat service to execute — it forwards the
	// step to the client's tool trail so hosted work is visible.
	ToolEvent *ToolEvent `json:"toolEvent,omitempty"`
	// ServerStep marks work the provider started server-side (a hosted
	// agent step without a trail label) that a replay of the request would
	// repeat: RetryingProvider treats the stream as delivered; every other
	// consumer ignores it (it carries no text).
	ServerStep bool `json:"-"`
	// ProviderItems mirrors Response.ProviderItems on the Done chunk: the
	// provider's ordered record of the turn for verbatim replay within a
	// tool loop (see Message.ProviderItems).
	ProviderItems json.RawMessage `json:"-"`
}

// ToolEvent describes one server-side tool step of a hosted agent run.
type ToolEvent struct {
	// Name is the provider's item type (web_search_call, mcp_call, …).
	Name string `json:"name"`
	// Label is the human-readable trail entry ("Agent · web search").
	Label string `json:"label"`
	// Done is false for the start notification, true for the completion.
	Done bool `json:"done"`
	// OK, DurationMs and Summary are only meaningful when Done is true.
	OK         bool   `json:"ok"`
	DurationMs int64  `json:"durationMs"`
	Summary    string `json:"summary,omitempty"`
}

var (
	ErrApiKeyInvalid       = errors.New("invalid API key")
	ErrKeyNotConfigured    = errors.New("API key not configured")
	ErrRateLimited         = errors.New("rate limited")
	ErrContextLimit        = errors.New("context limit exceeded")
	ErrProviderDown        = errors.New("provider is currently unavailable")
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrCircuitOpen         = errors.New("provider circuit open: too many recent failures")
	// ErrToolsUnsupported marks a 400 rejection because the selected MODEL
	// lacks function/tool support even though the provider family does
	// (SupportsTools is provider-wide — e.g. GitHub Models serves many models
	// without tool support behind one OpenAI-compatible endpoint). The chat
	// tool loop catches it and degrades gracefully to a no-tools answer
	// instead of failing the turn.
	ErrToolsUnsupported = errors.New("model does not support tools")
	// ErrContentFiltered marks a request or response the provider's content
	// filter blocked (Azure OpenAI / Foundry: an HTTP 400 with code
	// "content_filter" for a blocked prompt, finish_reason "content_filter" for
	// a blocked completion). Deterministic on the input, so it is never retried
	// and never trips the circuit breaker; the chat service explains it.
	ErrContentFiltered = errors.New("blocked by the provider's content filter")
	// ErrProviderConfigInvalid marks a provider whose stored configuration
	// document (Azure AI Foundry's endpoint + credential JSON) failed to parse
	// or validate. Surfaced instead of a network error so the user is told to
	// re-save the provider settings rather than to check connectivity.
	ErrProviderConfigInvalid = errors.New("provider configuration is invalid")
	// ErrProviderPermissionDenied marks credentials the provider ACCEPTED but
	// whose identity lacks the role for the operation — Azure AI Foundry
	// answers HTTP 401 with error.code "PermissionDenied" when a service
	// principal has no data-plane role (Foundry User) on the resource. The
	// concrete error (permissionDeniedError) also matches ErrApiKeyInvalid, so
	// retry and circuit-breaker behaviour is that of any auth failure, and it
	// keeps the provider's message, which names the principal and the missing
	// data action.
	ErrProviderPermissionDenied = errors.New("permission denied")
)

// permissionDeniedError is a permission-denied answer carrying the provider's
// explanation (see ErrProviderPermissionDenied).
type permissionDeniedError struct{ msg string }

func (e *permissionDeniedError) Error() string { return "permission denied: " + e.msg }

func (e *permissionDeniedError) Unwrap() []error {
	return []error{ErrProviderPermissionDenied, ErrApiKeyInvalid}
}

// detectContextLimitError checks whether a provider 400 response is a
// context-length-exceeded error and returns ErrContextLimit if so. Providers
// use different message wording, so we match on common substrings (case-
// insensitive). When the message states the window ("This model's maximum
// context length is 16384 tokens" — OpenAI, Azure OpenAI / Foundry, vLLM; Anthropic's
// "prompt is too long: 250000 tokens > 200000 maximum" and "input length and
// `max_tokens` exceed context limit: 180000 + 32000 > 200000"), the error is
// a *ContextLimitError carrying it, which still satisfies
// errors.Is(err, ErrContextLimit). Returns nil when the response is not a
// context-limit error.
func detectContextLimitError(statusCode int, message string) error {
	if statusCode != 400 || message == "" {
		return nil
	}
	lower := strings.ToLower(message)
	// Gemini's wording ("The input token count (N) exceeds the maximum number
	// of tokens allowed (M).") matches none of the OpenAI/Claude phrasings.
	for _, hint := range []string{"context length", "context window", "context limit", "too long", "maximum context", "token limit", "prompt is too long", "exceeds the maximum number of tokens"} {
		if strings.Contains(lower, hint) {
			for _, re := range contextWindowRes {
				if m := re.FindStringSubmatch(message); m != nil {
					if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
						return &ContextLimitError{Limit: n}
					}
				}
			}
			return ErrContextLimit
		}
	}
	return nil
}

// contextWindowRes extract the window a context-length rejection states:
// OpenAI-compatible "maximum context length is N", Anthropic's "prompt is
// too long: N tokens > L maximum" and "exceed context limit: N + M > L".
var contextWindowRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)maximum context length is (\d+)`),
	regexp.MustCompile(`(?i)prompt is too long:\s*\d+\s*tokens?\s*>\s*(\d+)\s*maximum`),
	regexp.MustCompile(`(?i)context limit:\s*\d+\s*\+\s*\d+\s*>\s*(\d+)`),
}

// ContextLimitError is a context-length rejection whose message stated the
// model's real window. It unwraps to ErrContextLimit so every errors.Is
// check keeps working; the audited wrapper remembers Limit for alias
// providers' deployments (recordContextWindow) so the next request is sized
// to it instead of the listing's default window.
type ContextLimitError struct {
	Limit int
}

func (e *ContextLimitError) Error() string { return ErrContextLimit.Error() }
func (e *ContextLimitError) Unwrap() error { return ErrContextLimit }

// detectToolsUnsupportedError checks whether a provider 400 response is a
// "this model can't use tools/function calling" rejection and returns
// ErrToolsUnsupported if so. Matching is deliberately two-sided — the message
// must BOTH mention tools/function-calling AND say they're unavailable — so an
// ordinary error that merely contains the word "tool" (e.g. "tool result too
// long") never matches. Returns nil for anything else.
//
// vLLM-served models (open models on Azure AI Foundry and GitHub Models whose
// server runs without a tool-call parser — Phi-4-multimodal, some Llama
// variants) reject a tools request with `"auto" tool choice requires
// --enable-auto-tool-choice and --tool-call-parser to be set`; Azure passes
// vLLM's error object through as the text of error.message. That wording is
// only emitted when the server cannot parse tool calls at all, so it counts as
// "unavailable" too.
func detectToolsUnsupportedError(statusCode int, message string) error {
	if statusCode != 400 || message == "" {
		return nil
	}
	lower := strings.ToLower(message)
	mentionsTools := strings.Contains(lower, "tool") || strings.Contains(lower, "function calling")
	if !mentionsTools {
		return nil
	}
	for _, hint := range []string{"not supported", "unsupported", "does not support", "doesn't support", "not available", "not enabled", "is disabled", "are disabled", "enable-auto-tool-choice", "tool-call-parser"} {
		if strings.Contains(lower, hint) {
			return ErrToolsUnsupported
		}
	}
	return nil
}

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
	// Azure OpenAI / Foundry send the precise hint in milliseconds
	// (retry-after-ms) next to a coarse whole-second Retry-After; prefer it.
	if ms := resp.Header.Get("retry-after-ms"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
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
	Models(ctx context.Context) ([]ModelInfo, error)
	DefaultModel() string
	FreeModel() string
	ContextLimit() int
	PricePerMillionTokens() Pricing
	EstimateTokens(text string) int
	// SupportsTools reports whether the provider family can run the agentic
	// tool loop (native function calling or the marker-based fallback). Used
	// by the provider listing so the UI can gate the Tools toggle.
	SupportsTools() bool
}

func orDefault(val, def int) int {
	if val <= 0 {
		return def
	}
	return val
}

// ModelMaxOutputTokens returns the maximum completion length for a specific
// model, or 0 (unknown) when the model is absent from the catalog or its
// MaxOutputTokens is unset. Callers treat 0 as "don't clamp".
func ModelMaxOutputTokens(ctx context.Context, p Provider, model string) int {
	models, err := p.Models(ctx)
	if err != nil {
		return 0
	}
	for _, m := range models {
		if m.ID == model {
			return m.MaxOutputTokens
		}
	}
	return 0
}

// ContextLimitFor returns the effective context window for a specific model:
// the model's catalog entry when it advertises one (per-model windows vary —
// e.g. Claude Opus/Sonnet 4.6 are 1M while the provider-wide default is 200k),
// otherwise the provider-wide limit. Callers that pass ContextLimit()
// directly under-use large-window models — history is truncated against the
// smaller default and the paid-for window is never filled.
func ContextLimitFor(ctx context.Context, p Provider, modelID string) int {
	if modelID == "" {
		return p.ContextLimit()
	}
	models, err := p.Models(ctx)
	if err == nil {
		for _, m := range models {
			if m.ID == modelID && m.ContextLimit > 0 {
				return m.ContextLimit
			}
		}
	}
	return p.ContextLimit()
}
