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
	Embed(ctx context.Context, text []string) ([][]float32, error)
	// SupportsTools reports whether the provider's Chat path can serialize tool definitions and return tool
	// calls. The chat service only runs the agentic tool loop against providers that return true.
	SupportsTools() bool
}

// TokenCounter is the optional exact-token-count capability a provider may expose next to Provider:
// CountTokens asks the provider's own tokenizer so near-window prompts can be re-checked before the
// estimate's error band lets an oversized request through (callers fall back to the estimate on error).
type TokenCounter interface {
	CountTokens(ctx context.Context, req Request) (int, error)
}

type EmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

type ModelInfo struct {
	ID           string `json:"id"`
	DisplayName  string `json:"displayName"`
	ContextLimit int    `json:"contextLimit"`
	// MaxOutputTokens is the model's maximum completion length, far smaller than ContextLimit (the input
	// window). Zero means unknown — callers clamping MaxTokens against it should leave the value untouched then.
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	// ServedModel is the underlying model that answers when this entry's ID is only an alias for it — a Foundry
	// deployment name, a model-router deployment. Empty for catalogue models, whose ID is the model.
	ServedModel string `json:"servedModel,omitempty"`
	// Embedding marks an embedding deployment (Azure AI Foundry): kept in Models() so embedding usage is
	// sized, never offered as a chat model or chosen as the chat default.
	Embedding bool `json:"embedding,omitempty"`
}

type Request struct {
	Model        string    `json:"model"`
	Messages     []Message `json:"messages"`
	SystemPrompt string    `json:"systemPrompt,omitempty"`
	Temperature  float64   `json:"temperature,omitempty"`
	MaxTokens    int       `json:"maxTokens,omitempty"`
	// TemperatureSet marks Temperature as explicitly chosen by the caller, so a provider sends it even when
	// it is 0. False keeps the legacy rule: a zero Temperature means "provider default" and is omitted.
	TemperatureSet bool `json:"-"`
	// ReasoningEffort is the per-user choice of reasoning depth for reasoning deployments: "low", "medium" or
	// "high" (chat completions reasoning_effort, Claude output_config.effort). Empty means the provider default.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Tools, when non-empty and the provider SupportsTools, are offered to the model so it can request a tool
	// call instead of (or before) a text answer.
	Tools []ToolDefinition `json:"tools,omitempty"`
	// OrgID is request metadata (not sent to any provider API) used by the audited wrapper to attribute
	// usage/cost to an organization, enabling org-wide daily budgets. Empty for personal/non-org flows.
	OrgID string `json:"-"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is set on an assistant message that requested tool calls; it is echoed back in the next turn's
	// history so the provider can correlate the following tool results.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// ToolCallID is set on a Role=="tool" message carrying a tool result; it references the ToolCall.ID this result answers.
	ToolCallID string `json:"toolCallId,omitempty"`
	// ProviderItems is the provider's own record of an assistant turn, opaque to everything but the provider
	// that produced it (ProviderItemsFrom). Set only by the chat service's tool loop and replayed verbatim by
	// the provider that answered — how thinking survives a tool step. Never persisted or sent to the client.
	ProviderItems json.RawMessage `json:"-"`
	// ProviderItemsFrom is the provider ID that produced ProviderItems; a provider ignores items another provider wrote.
	ProviderItemsFrom string `json:"-"`
}

type Response struct {
	Content   string `json:"content"`
	TokensIn  int    `json:"tokensIn"`
	TokensOut int    `json:"tokensOut"`
	// CacheReadTokens and CacheWriteTokens are the prompt-cache slice of the input (Claude's cached-input
	// fields): a breakdown INCLUDED in TokensIn, priced at the cached rates. Zero when none is reported.
	CacheReadTokens  int    `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int    `json:"cacheWriteTokens,omitempty"`
	FinishReason     string `json:"finishReason"`
	// ToolCalls is non-empty when the model requested tool execution instead of returning a final answer; the
	// chat service runs them and continues the loop.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// ServedModel is the model name the provider reported (OpenAI-compatible APIs echo the concrete version,
	// e.g. "gpt-4o-2024-11-20"); used to price usage when the request model is an alias the catalogue cannot know.
	ServedModel string `json:"servedModel,omitempty"`
	// Reasoning is inline model reasoning split off the answer — a leading <think>…</think> block (open
	// reasoning models on OpenAI-compatible chat completions). Not part of Content: never shown or re-sent.
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

// Chunk is one streamed event. A chunk with no field set is a keepalive: the provider is still working
// (reasoning, assembling a tool call) but has nothing to deliver — consumers only use it to reset their idle timers.
type Chunk struct {
	Text string `json:"text"`
	// Reasoning is inline model reasoning split off the answer (Response.Reasoning). It is not answer text:
	// never displayed, stored or re-sent; the audited wrapper counts it toward the output-token estimate.
	Reasoning string `json:"reasoning,omitempty"`
	TokensIn  int    `json:"tokensIn,omitempty"`
	TokensOut int    `json:"tokensOut,omitempty"`
	// CacheReadTokens / CacheWriteTokens mirror Response's cached-input breakdown on the usage-carrying
	// chunks: a slice INCLUDED in TokensIn, priced at the cached rates.
	CacheReadTokens  int   `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int   `json:"cacheWriteTokens,omitempty"`
	Done             bool  `json:"done"`
	Error            error `json:"error,omitempty"`
	// ToolCalls is populated on the Done chunk when the model requested tool execution mid-stream instead of
	// finishing with text. The agentic tool loop reads it to decide whether to run tools and continue.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// FinishReason mirrors the provider stop reason on the Done chunk (optional).
	FinishReason string `json:"finishReason,omitempty"`
	// ServedModel mirrors Response.ServedModel on the Done chunk (optional).
	ServedModel string `json:"servedModel,omitempty"`
	// ToolEvent reports a tool the PROVIDER ran server-side (a hosted agent's web search, MCP or code-interpreter
	// call): one chunk at start (Done=false), one at end. Nothing for the chat service to execute.
	ToolEvent *ToolEvent `json:"toolEvent,omitempty"`
	// Notice is a provider status line for the chat trail (see Notice). It carries no content: RetryingProvider
	// does not count it as delivered.
	Notice *Notice `json:"notice,omitempty"`
	// ServerStep marks server-side work a replay of the request would repeat: RetryingProvider treats the
	// stream as delivered; every other consumer ignores it (it carries no text).
	ServerStep bool `json:"-"`
	// LocalKeepalive marks a keepalive the provider synthesised while waiting for a non-streaming answer
	// (streamViaChat), not one the upstream sent: it feeds the idle watchdog but proves nothing about the request.
	LocalKeepalive bool `json:"-"`
	// SilentStep marks the start (+1) / end (-1) of a provider phase that streams nothing while it runs but is
	// not a hosted tool step (a Responses reasoning item with summaries off); the chat service holds the
	// hosted-step idle limit while one is open.
	SilentStep int8 `json:"-"`
	// ProviderItems mirrors Response.ProviderItems on the Done chunk: the provider's ordered record of the
	// turn for verbatim replay within a tool loop (see Message.ProviderItems).
	ProviderItems json.RawMessage `json:"-"`
}

// Notice is a provider status line for the chat trail (e.g. "reasoning_off"): informational only — not a
// hosted tool step, not delivered content, so the retry wrapper and the idle watchdog's hosted-step accounting ignore it.
type Notice struct {
	Name  string
	Label string
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
	// ErrToolsUnsupported marks a 400 because the selected MODEL lacks tool support though the provider family
	// does not (SupportsTools is provider-wide); the chat tool loop degrades to a no-tools answer.
	ErrToolsUnsupported = errors.New("model does not support tools")
	// ErrContentFiltered marks a request or response the provider's content filter blocked (a 400 with code
	// "content_filter", or finish_reason "content_filter"). Deterministic: never retried, never trips the breaker.
	ErrContentFiltered = errors.New("blocked by the provider's content filter")
	// ErrProviderConfigInvalid marks a provider whose stored configuration document failed to parse or
	// validate. Surfaced instead of a network error so the user is told to re-save the provider settings.
	ErrProviderConfigInvalid = errors.New("provider configuration is invalid")
	// ErrProviderPermissionDenied marks credentials the provider ACCEPTED but whose identity lacks the role for
	// the operation (Foundry's 401 PermissionDenied for a principal without a data-plane role); the concrete
	// error also matches ErrApiKeyInvalid, so retry/breaker treat it as any auth failure.
	ErrProviderPermissionDenied = errors.New("permission denied")
)

// permissionDeniedError is a permission-denied answer carrying the provider's
// explanation (see ErrProviderPermissionDenied).
type permissionDeniedError struct{ msg string }

func (e *permissionDeniedError) Error() string { return "permission denied: " + e.msg }

func (e *permissionDeniedError) Unwrap() []error {
	return []error{ErrProviderPermissionDenied, ErrApiKeyInvalid}
}

// detectContextLimitError matches a provider 400 for a context-length rejection (providers word it
// differently; common substrings, case-insensitively). When the message states the window ("maximum context
// length is 16384 tokens", "prompt is too long: 250000 > 200000 maximum"), the error is a *ContextLimitError
// carrying it, which still satisfies errors.Is(err, ErrContextLimit).
func detectContextLimitError(statusCode int, message string) error {
	if statusCode != 400 || message == "" {
		return nil
	}
	lower := strings.ToLower(message)
	// Gemini's wording ("The input token count (N) exceeds the maximum number of tokens allowed (M).") matches none of the OpenAI/Claude phrasings.
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

// contextWindowRes extract the window a context-length rejection states: OpenAI-compatible "maximum context
// length is N", Anthropic's "prompt is too long: N tokens > L maximum" and "exceed context limit: N + M > L".
var contextWindowRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)maximum context length is (\d+)`),
	regexp.MustCompile(`(?i)prompt is too long:\s*\d+\s*tokens?\s*>\s*(\d+)\s*maximum`),
	regexp.MustCompile(`(?i)context limit:\s*\d+\s*\+\s*\d+\s*>\s*(\d+)`),
}

// ContextLimitError is a context-length rejection whose message stated the real window; it unwraps to
// ErrContextLimit, and the audited wrapper remembers Limit so the next request is sized to it.
type ContextLimitError struct {
	Limit int
}

func (e *ContextLimitError) Error() string { return ErrContextLimit.Error() }
func (e *ContextLimitError) Unwrap() error { return ErrContextLimit }

// detectToolsUnsupportedError matches a 400 saying the MODEL can't use tools/function calling, returning
// ErrToolsUnsupported. Matching is two-sided — tools AND unavailable — so an ordinary error containing "tool"
// never matches; vLLM's "--enable-auto-tool-choice" wording counts as unavailable too.
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

// RateLimitError is a rate-limit (HTTP 429) error that carries the server's Retry-After hint. It unwraps to
// ErrRateLimited so existing errors.Is checks keep working, while the retry backoff can wait at least RetryAfter.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// rateLimitErr builds a *RateLimitError from a 429 response, parsing the Retry-After header (delta-seconds or
// an HTTP-date); a missing/invalid header yields a zero RetryAfter, so the caller falls back to its default backoff.
func rateLimitErr(resp *http.Response) error {
	return &RateLimitError{RetryAfter: parseRetryAfter(resp)}
}

func parseRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	// Azure OpenAI / Foundry send the precise hint in milliseconds (retry-after-ms) next to a coarse whole-second Retry-After; prefer it.
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

// MetadataProvider exposes read-only provider information without API credentials; it intentionally omits
// Chat and Stream so callers cannot accidentally issue real API calls with an empty key.
type MetadataProvider interface {
	ID() string
	Name() string
	Models(ctx context.Context) ([]ModelInfo, error)
	DefaultModel() string
	FreeModel() string
	ContextLimit() int
	EstimateTokens(text string) int
	// SupportsTools reports whether the provider family can run the agentic tool loop. Used by the provider
	// listing so the UI can gate the Tools toggle.
	SupportsTools() bool
}

func orDefault(val, def int) int {
	if val <= 0 {
		return def
	}
	return val
}

// ModelMaxOutputTokens returns the maximum completion length for a specific model, or 0 (unknown) when the
// model is absent from the catalog or its MaxOutputTokens is unset. Callers treat 0 as "don't clamp".
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

// ContextLimitFor returns the model's own context window when it advertises one (per-model windows vary —
// Claude Opus/Sonnet 4.6 are 1M against a 200k provider default), else the provider-wide limit.
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
