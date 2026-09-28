package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"pad-core/logger"
)

var claudeAPIURL = providerURL("CLAUDE_API_URL", "https://api.anthropic.com/v1/messages")
var claudeAPIVersion = "2023-06-01"

type ClaudeProvider struct {
	apiKey string
	client *http.Client
	// messagesURL is the Messages endpoint of a provider that speaks the
	// Anthropic wire format on another host — Claude deployments on an Azure
	// AI Foundry resource (azure_claude.go). Empty for the plain provider,
	// which reads the package URL (claudeAPIURL) at request time, so the
	// plain provider's requests are unchanged and tests can still swap the
	// package variable after construction.
	messagesURL string
	// authorize sets the request's credential; nil sends the API key as
	// x-api-key (the plain provider).
	authorize func(ctx context.Context, req *http.Request) error
	// unauthorized, when set, classifies a 401/403 response from its status
	// and body (Azure: an Entra ID access token the resource refused carries
	// Azure's explanation); nil keeps the plain mapping (401 → ErrApiKeyInvalid,
	// 403 → the service's message).
	unauthorized func(status int, body []byte) error
	// servedModelFor, when set, names the model a deployment serves (Azure
	// AI Foundry: the project listing's modelName, else the served-model
	// memo), "" when unknown. The sampling policy is decided from it: a
	// deployment name such as "prod-claude" says nothing about the model.
	servedModelFor func(deployment string) string
	// label names the route in logs; "" is "claude".
	label string
}

func NewClaudeProvider(apiKey string) *ClaudeProvider {
	return &ClaudeProvider{
		apiKey: apiKey,
		client: sharedHTTPClient,
	}
}

// endpoint is the Messages URL requests go to: the instance override, else
// the package URL.
func (c *ClaudeProvider) endpoint() string {
	if c.messagesURL != "" {
		return c.messagesURL
	}
	return claudeAPIURL
}

// auth sets the credential header(s) on req.
func (c *ClaudeProvider) auth(ctx context.Context, req *http.Request) error {
	if c.authorize != nil {
		return c.authorize(ctx, req)
	}
	req.Header.Set("x-api-key", c.apiKey)
	return nil
}

// classifyUnauthorized runs the unauthorized hook for a 401/403; nil (no
// hook, or another status) leaves the response to the shared mapping.
func (c *ClaudeProvider) classifyUnauthorized(status int, body []byte) error {
	if c.unauthorized == nil || (status != http.StatusUnauthorized && status != http.StatusForbidden) {
		return nil
	}
	return c.unauthorized(status, body)
}

func (c *ClaudeProvider) SupportsTools() bool { return true }

func (c *ClaudeProvider) ID() string           { return "claude" }
func (c *ClaudeProvider) Name() string         { return "Claude" }
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

// Models is the Claude catalogue without the invite-only models
// (claudeInviteOnly): those stay in the catalogue so a Foundry deployment of
// one is priced, but are not offered in the direct provider's picker.
func (c *ClaudeProvider) Models(_ context.Context) ([]ModelInfo, error) {
	models := catalogModels("claude")
	out := models[:0]
	for _, m := range models {
		if !claudeInviteOnly[m.ID] {
			out = append(out, m)
		}
	}
	return out, nil
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
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	InputSchema  json.RawMessage     `json:"input_schema"`
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
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    []claudeContent `json:"content"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      claudeUsage     `json:"usage"`
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
	// Cache tokens are billed separately (reads ~0.1×, writes ~1.25×) and are
	// EXCLUDED from input_tokens — folded into the count at full rate so
	// cached conversations don't silently bypass the budget (see
	// parseClaudeSSE for the same treatment on streams).
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// totalInputTokens returns input_tokens plus cache reads/writes.
func (u claudeUsage) totalInputTokens() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
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

// claudeModelVersionRe finds the family and version in a Claude model or
// deployment name ("claude-opus-4-7", "prod-claude-sonnet-5-20260801"): the
// major version and an optional one- or two-digit minor version (a longer
// number is a snapshot date: "claude-opus-4-20250514" is Opus 4).
var claudeModelVersionRe = regexp.MustCompile(`(opus|sonnet|haiku|fable|mythos)-(\d{1,2})(?:[-.](\d{1,2}))?(?:[^0-9]|$)`)

// claudeRemovesSampling reports whether a model rejects temperature/top_p/top_k
// with a 400: Opus 4.7 and later, every model of generation 5 and later
// (Sonnet 5 and 5.5 reject non-default values too) and the Fable and Mythos
// families. For those we omit sampling params and enable adaptive thinking
// instead — the supported, higher-quality reasoning mode. Matched
// case-insensitively; callers pass the SERVED model when it is known
// (policyModel), else the deployment name. A name that reveals nothing keeps
// temperature, and send repairs a rejection of it once.
func claudeRemovesSampling(model string) bool {
	m := claudeModelVersionRe.FindStringSubmatch(strings.ToLower(model))
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[2])
	minor := 0
	if m[3] != "" {
		minor, _ = strconv.Atoi(m[3])
	}
	switch {
	case m[1] == "fable" || m[1] == "mythos":
		return true
	case major >= 5:
		return true
	case m[1] == "opus" && major == 4 && minor >= 7:
		return true
	}
	return false
}

// policyModel is the lower-cased name the sampling policy is decided from:
// the model the deployment serves when known (servedModelFor), else the
// requested model / deployment name.
func (c *ClaudeProvider) policyModel(model string) string {
	if c.servedModelFor != nil {
		if served := strings.TrimSpace(c.servedModelFor(model)); served != "" {
			return strings.ToLower(served)
		}
	}
	return strings.ToLower(model)
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
	if claudeRemovesSampling(c.policyModel(req.Model)) {
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

// claudeErrorMessageMax bounds the service text kept on an error.
const claudeErrorMessageMax = 500

// post sends one Messages request (the body already carries the stream flag).
func (c *ClaudeProvider) post(ctx context.Context, body claudeRequest) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if err := c.auth(ctx, httpReq); err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", claudeAPIVersion)
	if body.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude API request: %w", err)
	}
	return resp, nil
}

// claudeSamplingParam is the learnedRepairs entry of a deployment that
// rejects temperature: later requests go out without it.
const claudeSamplingParam = "temperature"

// send posts the request and returns a 200 response with its body unread, or
// the rejection with its body read (and closed). A 400 that names
// `temperature` or `top_p` while the request carries a temperature is
// repaired ONCE: the request is sent again without it, and the repair a 200
// confirmed is remembered per endpoint and deployment (learnedRepairs, shared
// with the OpenAI-compatible route) so the next turn does not pay the
// rejection round trip again. The name-based sampling policy
// (claudeRemovesSampling) cannot know every model behind a custom deployment
// name; this is the safety net.
func (c *ClaudeProvider) send(ctx context.Context, body claudeRequest) (*http.Response, []byte, error) {
	memoKey := c.endpoint() + "\x00" + body.Model
	if body.Temperature != 0 && slices.Contains(repairsFor(memoKey), claudeSamplingParam) {
		body.Temperature = 0 // omitted (omitempty)
	}
	repaired := false
	for {
		resp, err := c.post(ctx, body)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == http.StatusOK {
			if repaired {
				rememberRepairs(memoKey, map[string]bool{claudeSamplingParam: true})
			}
			return resp, nil, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		c.logUpstreamRejection(resp, body.Model)
		if !repaired && body.Temperature != 0 && claudeSamplingRejected(resp.StatusCode, errBody) {
			logger.Debug("Claude rejected the sampling parameters — retrying without temperature",
				"provider", c.logLabel(), "model", body.Model)
			body.Temperature = 0
			repaired = true
			continue
		}
		return resp, errBody, nil
	}
}

// claudeSamplingRejected reports whether a rejection is the model refusing
// sampling parameters: an invalid-request 400 whose message names
// `temperature` or `top_p` ("temperature is deprecated for this model",
// "`temperature` and `top_p` cannot both be specified").
func claudeSamplingRejected(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var apiErr claudeErrorResp
	if json.Unmarshal(body, &apiErr) != nil {
		return false
	}
	if t := apiErr.Error.Type; t != "" && t != "invalid_request_error" {
		return false
	}
	msg := strings.ToLower(apiErr.Error.Message)
	return strings.Contains(msg, "temperature") || strings.Contains(msg, "top_p")
}

// logLabel names the route in logs.
func (c *ClaudeProvider) logLabel() string {
	if c.label != "" {
		return c.label
	}
	return "claude"
}

// claudeRequestID is the correlation id of a Messages response: the gateway's
// (apim-request-id on Azure AI Foundry, see upstreamRequestID), else
// Anthropic's own request-id. Support asks for both; one is enough to search.
func claudeRequestID(resp *http.Response) string {
	if rid := upstreamRequestID(resp); rid != "" {
		return rid
	}
	if resp == nil {
		return ""
	}
	return resp.Header.Get("request-id")
}

// logUpstreamRejection records a non-200's status and request id (as the
// OpenAI-compatible route does): a parameter rejection may still be repaired
// by send, so it logs at debug.
func (c *ClaudeProvider) logUpstreamRejection(resp *http.Response, model string) {
	rid := claudeRequestID(resp)
	if rid == "" {
		return
	}
	if parameterRejectionStatus(resp.StatusCode) {
		logger.Debug("AI upstream rejection", "provider", c.logLabel(), "model", model, "status", resp.StatusCode, "requestId", rid)
		return
	}
	logger.Warn("AI upstream error", "provider", c.logLabel(), "model", model, "status", resp.StatusCode, "requestId", rid)
}

// permissionDeniedMessage is the explanation kept on a permission-denied
// error, with a fallback when the service sent none.
func permissionDeniedMessage(msg string) string {
	if msg = oneLine(strings.Join(strings.Fields(msg), " "), claudeErrorMessageMax); msg != "" {
		return msg
	}
	return "the credential is not allowed to use this model"
}

// classifyError maps a non-200 Messages response onto the shared errors.
// STATUS first, the JSON envelope only enriching the message (as
// openaiBase.handleChatError): a gateway 502/503 with an HTML or empty body
// is still ErrProviderDown (retried, counted by the breaker), a 429 without
// a body is still rate-limited with its Retry-After. 401/403 go to the
// unauthorized hook (Azure AI Foundry); otherwise an Anthropic
// permission_error is a permission denial and every other 401/403 an invalid
// credential. Residual errors carry the upstream request id.
func (c *ClaudeProvider) classifyError(resp *http.Response, body []byte, stream bool) error {
	status := resp.StatusCode
	var apiErr claudeErrorResp
	_ = json.Unmarshal(body, &apiErr)
	msg := oneLine(strings.Join(strings.Fields(apiErr.Error.Message), " "), claudeErrorMessageMax)
	suffix := ""
	if rid := claudeRequestID(resp); rid != "" {
		suffix = ", request id " + rid
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if err := c.classifyUnauthorized(status, body); err != nil {
			return err
		}
		if apiErr.Error.Type == "permission_error" {
			return &permissionDeniedError{msg: permissionDeniedMessage(msg)}
		}
		return ErrApiKeyInvalid
	case status == http.StatusTooManyRequests:
		return rateLimitErr(resp)
	case status >= 500:
		if msg != "" {
			return fmt.Errorf("%w: %s (status %d%s)", ErrProviderDown, msg, status, suffix)
		}
		return fmt.Errorf("%w (status %d%s)", ErrProviderDown, status, suffix)
	}
	if msg != "" {
		if err := detectContextLimitError(status, apiErr.Error.Message); err != nil {
			return err
		}
		if err := detectToolsUnsupportedError(status, apiErr.Error.Message); err != nil {
			return err
		}
		if stream {
			return fmt.Errorf("claude stream error (status %d%s): %s", status, suffix, msg)
		}
		return fmt.Errorf("claude API: %s (status %d%s)", msg, status, suffix)
	}
	if stream {
		return fmt.Errorf("claude stream error (status %d%s)", status, suffix)
	}
	return fmt.Errorf("claude API error (status %d%s)", status, suffix)
}

// claudeRefusalError is a response Claude ended with stop_reason "refusal"
// (its safety classifiers declined the request). Deterministic on the input
// like a content-filter block, so it is ErrContentFiltered: never retried,
// never a breaker failure, and explained by the chat service instead of
// presenting whatever text arrived as a complete answer.
func claudeRefusalError() error {
	return fmt.Errorf("%w: the model declined to respond (stop_reason refusal)", ErrContentFiltered)
}

func (c *ClaudeProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	resp, errBody, err := c.send(ctx, c.buildBody(req, false))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.classifyError(resp, errBody, false)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read claude response: %w", err)
	}

	var parsed claudeResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse claude response: %w", err)
	}
	if parsed.StopReason == "refusal" {
		return nil, claudeRefusalError()
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
		TokensIn:     parsed.Usage.totalInputTokens(),
		TokensOut:    parsed.Usage.OutputTokens,
		FinishReason: parsed.StopReason,
		ToolCalls:    toolCalls,
		ServedModel:  parsed.Model,
	}, nil
}

func (c *ClaudeProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	resp, errBody, err := c.send(ctx, c.buildBody(req, true))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return c.classifyError(resp, errBody, true)
	}
	defer resp.Body.Close()
	return parseClaudeSSE(resp.Body, onChunk)
}
