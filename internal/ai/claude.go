package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var claudeAPIURL = providerURL("CLAUDE_API_URL", "https://api.anthropic.com/v1/messages")
var claudeAPIVersion = "2023-06-01"

type ClaudeProvider struct {
	apiKey string
	client *http.Client
}

func NewClaudeProvider(apiKey string) *ClaudeProvider {
	return &ClaudeProvider{
		apiKey: apiKey,
		client: sharedHTTPClient,
	}
}

func (c *ClaudeProvider) SupportsTools() bool { return true }

func (c *ClaudeProvider) ID() string          { return "claude" }
func (c *ClaudeProvider) Name() string        { return "Claude" }
func (c *ClaudeProvider) ContextLimit() int    { return 200000 }
func (c *ClaudeProvider) DefaultModel() string { return "claude-sonnet-4-6" }
func (c *ClaudeProvider) FreeModel() string    { return "" }

func (c *ClaudeProvider) PricePerMillionTokens() Pricing {
	// Sonnet 4.6 list pricing (the default model).
	return Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}
}

func (c *ClaudeProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by Claude provider")
}

func (c *ClaudeProvider) EstimateTokens(text string) int {
	return EstimateTokensClaude(text)
}

func (c *ClaudeProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return catalogModels("claude"), nil
}

type claudeRequest struct {
	Model       string              `json:"model"`
	MaxTokens   int                 `json:"max_tokens"`
	Temperature float64             `json:"temperature,omitempty"`
	System      []claudeSystemBlock `json:"system,omitempty"`
	Messages    []claudeMessage     `json:"messages"`
	Tools       []claudeTool        `json:"tools,omitempty"`
	Thinking    *claudeThinking     `json:"thinking,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
}

// claudeCacheControl marks a content block as a prompt-cache breakpoint. The
// stable prefix up to and including the marked block is cached (~0.1x read cost
// vs full price), which pays off when the same prefix is re-sent — every
// iteration of the agentic tool loop and every turn of a conversation.
type claudeCacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

var ephemeralCache = &claudeCacheControl{Type: "ephemeral"}

// claudeThinking enables adaptive extended thinking. Required form on Opus 4.7+
// (manual budget_tokens is rejected there); a quality lift for multi-step flow
// reasoning. Thinking text is omitted from the response by default, so no
// response-parsing change is needed.
type claudeThinking struct {
	Type string `json:"type"` // "adaptive"
}

// claudeSystemBlock is one system-prompt content block. Sending system as an
// array (rather than a bare string) is what lets us attach cache_control to it.
type claudeSystemBlock struct {
	Type         string              `json:"type"` // "text"
	Text         string              `json:"text"`
	CacheControl *claudeCacheControl `json:"cache_control,omitempty"`
}

type claudeTool struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	InputSchema json.RawMessage     `json:"input_schema"`
	CacheControl *claudeCacheControl `json:"cache_control,omitempty"`
}

// claudeMessage.Content is either a plain string (text-only turn) or an array of
// content blocks (when tool_use / tool_result blocks are present), hence any.
type claudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// claudeBlock is one content block: text, tool_use (assistant), or tool_result
// (user). Only the fields relevant to a block's Type are populated.
type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"` // tool_result payload
}

type claudeResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Content    []claudeContent  `json:"content"`
	Model      string           `json:"model"`
	StopReason string           `json:"stop_reason"`
	Usage      claudeUsage      `json:"usage"`
}

type claudeContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type claudeErrorResp struct {
	Error claudeErrorDetail `json:"error"`
}

type claudeErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// toClaudeMessages converts the provider-neutral messages into Claude's wire
// format. Text turns use a plain string content; assistant turns that issued
// tool calls become a content-block array (optional text + tool_use blocks);
// and a run of consecutive Role=="tool" results is coalesced into a single user
// message carrying one tool_result block per result — Anthropic requires the
// tool results for an assistant turn to arrive together in the next user turn.
func toClaudeMessages(msgs []Message) []claudeMessage {
	var out []claudeMessage
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		switch {
		case m.Role == "tool":
			// Gather this and any immediately-following tool results.
			var blocks []claudeBlock
			for i < len(msgs) && msgs[i].Role == "tool" {
				blocks = append(blocks, claudeBlock{
					Type:      "tool_result",
					ToolUseID: msgs[i].ToolCallID,
					Content:   msgs[i].Content,
				})
				i++
			}
			i-- // outer loop will increment
			out = append(out, claudeMessage{Role: "user", Content: blocks})

		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			var blocks []claudeBlock
			if m.Content != "" {
				blocks = append(blocks, claudeBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := tc.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, claudeBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: input,
				})
			}
			out = append(out, claudeMessage{Role: "assistant", Content: blocks})

		default:
			out = append(out, claudeMessage{Role: m.Role, Content: m.Content})
		}
	}
	return out
}

func toClaudeTools(tools []ToolDefinition) []claudeTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]claudeTool, len(tools))
	for i, t := range tools {
		out[i] = claudeTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	return out
}

// claudeRemovesSampling reports whether a model rejects temperature/top_p/top_k
// (Opus 4.7 and later return 400). For those we omit sampling params and enable
// adaptive thinking instead — the supported, higher-quality reasoning mode.
func claudeRemovesSampling(model string) bool {
	return strings.Contains(model, "opus-4-7") || strings.Contains(model, "opus-4-8")
}

// buildClaudeSystem wraps the system prompt in a single cached text block.
// Marking it with cache_control caches the whole stable prefix (tools render
// before system), so the tool loop's repeated turns and multi-turn chat reuse
// it instead of re-paying full input price. Returns nil when empty so `system`
// is omitted.
func buildClaudeSystem(prompt string) []claudeSystemBlock {
	if prompt == "" {
		return nil
	}
	return []claudeSystemBlock{{Type: "text", Text: prompt, CacheControl: ephemeralCache}}
}

// buildBody assembles the wire request shared by Chat and Stream: structured
// (cached) system, tools, messages, and model-conditional sampling/thinking.
func (c *ClaudeProvider) buildBody(req Request, stream bool) claudeRequest {
	body := claudeRequest{
		Model:     req.Model,
		MaxTokens: orDefault(req.MaxTokens, 8192),
		System:    buildClaudeSystem(req.SystemPrompt),
		Messages:  toClaudeMessages(req.Messages),
		Tools:     toClaudeTools(req.Tools),
		Stream:    stream,
	}
	if claudeRemovesSampling(req.Model) {
		// Opus 4.7+ rejects temperature/top_p/top_k entirely — omit them.
		// Enable adaptive thinking only when there are no tools: with tool use
		// the API requires preserving thinking blocks (with signatures) across
		// turns, which the provider-neutral Message model doesn't carry, so a
		// follow-up tool-result turn would 400. The agentic loop therefore runs
		// Opus without thinking; plain chat keeps it.
		if len(req.Tools) == 0 {
			body.Thinking = &claudeThinking{Type: "adaptive"}
		}
	} else {
		body.Temperature = req.Temperature
	}
	return body
}

func (c *ClaudeProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	body := c.buildBody(req, false)
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", claudeAPIURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", claudeAPIVersion)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode != http.StatusOK {
		var apiErr claudeErrorResp
		if err := json.Unmarshal(respBody, &apiErr); err == nil {
			switch {
			case resp.StatusCode == 401:
				return nil, ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return nil, rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return nil, fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return nil, fmt.Errorf("claude API: %s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("claude API error (status %d)", resp.StatusCode)
	}

	var parsed claudeResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse claude response: %w", err)
	}
	if len(parsed.Content) == 0 {
		return nil, errors.New("claude returned no content")
	}

	// Aggregate text blocks and collect any tool_use blocks. With tools enabled
	// a response may contain only tool_use blocks (no text), so we don't require
	// text to be present.
	var text string
	var toolCalls []ToolCall
	for _, c := range parsed.Content {
		switch c.Type {
		case "text":
			text += c.Text
		case "tool_use":
			toolCalls = append(toolCalls, ToolCall{ID: c.ID, Name: c.Name, Input: c.Input})
		}
	}

	return &Response{
		Content:      text,
		TokensIn:     parsed.Usage.InputTokens,
		TokensOut:    parsed.Usage.OutputTokens,
		FinishReason: parsed.StopReason,
		ToolCalls:    toolCalls,
	}, nil
}

func (c *ClaudeProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	body := c.buildBody(req, true)
	jsonBody, _ := json.Marshal(body)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", claudeAPIURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}

	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", claudeAPIVersion)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		var apiErr claudeErrorResp
		if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
			switch {
			case resp.StatusCode == 401:
				return ErrApiKeyInvalid
			case resp.StatusCode == 429:
				return rateLimitErr(resp)
			case resp.StatusCode >= 500:
				return fmt.Errorf("%w: %s", ErrProviderDown, apiErr.Error.Message)
			}
			return fmt.Errorf("claude stream error (status %d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w (status %d)", ErrProviderDown, resp.StatusCode)
		}
		return fmt.Errorf("claude stream error (status %d)", resp.StatusCode)
	}

	return parseClaudeSSE(resp.Body, onChunk)
}
