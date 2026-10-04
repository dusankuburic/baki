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
	// messagesURL is the Messages endpoint of a provider speaking the Anthropic wire format on another host —
	// Claude deployments on an Azure AI Foundry resource. Empty reads the package URL so tests can swap it.
	messagesURL string
	// authorize sets the request's credential; nil sends the API key as x-api-key (the plain provider).
	authorize func(ctx context.Context, req *http.Request) error
	// unauthorized, when set, classifies a 401/403 from its status and body (Azure: a refused Entra ID token
	// carries Azure's explanation); nil keeps the plain mapping (401 → ErrApiKeyInvalid, 403 → the message).
	unauthorized func(status int, body []byte) error
	// servedModelFor, when set, names the model a deployment serves (the project listing's modelName, else the
	// served-model memo), "" when unknown — the sampling policy is decided from it; "prod-claude" says nothing.
	servedModelFor func(deployment string) string
	// label names the route in logs; "" is "claude".
	label string
	// itemsOwner is the provider ID whose replay records (Message.ProviderItems) this route replays: for a
	// Claude deployment on Azure AI Foundry, the Azure provider's ID. "" is this provider's own ID.
	itemsOwner string
}

func NewClaudeProvider(apiKey string) *ClaudeProvider {
	return &ClaudeProvider{
		apiKey: apiKey,
		client: sharedHTTPClient,
	}
}

// endpoint is the Messages URL requests go to: the instance override, else the package URL.
func (c *ClaudeProvider) endpoint() string {
	if c.messagesURL != "" {
		return c.messagesURL
	}
	return claudeAPIURL
}

func (c *ClaudeProvider) auth(ctx context.Context, req *http.Request) error {
	if c.authorize != nil {
		return c.authorize(ctx, req)
	}
	req.Header.Set("x-api-key", c.apiKey)
	return nil
}

// classifyUnauthorized runs the unauthorized hook for a 401/403; nil (no hook, or another status) leaves the shared mapping.
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

// Models is the Claude catalogue without the invite-only models (claudeInviteOnly): those stay in the catalogue
// so a Foundry deployment of one is sized, but are not offered in this picker.
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
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// Temperature is a pointer so an explicit 0 (deterministic sampling) is sent; nil omits it.
	Temperature *float64            `json:"temperature,omitempty"`
	System      []claudeSystemBlock `json:"system,omitempty"`
	Messages    []claudeMessage     `json:"messages"`
	Tools       []claudeTool        `json:"tools,omitempty"`
	Thinking    *claudeThinking     `json:"thinking,omitempty"`
	// OutputConfig carries output_config.effort: the user-chosen reasoning depth, sent ONLY on the models that
	// also reject sampling parameters (claudeRemovesSampling); older models would reject the object.
	OutputConfig *claudeOutputConfig `json:"output_config,omitempty"`
	Stream       bool                `json:"stream,omitempty"`
	// CacheControl is the TOP-LEVEL automatic-caching marker: the API picks the last cacheable block itself and
	// moves the breakpoint forward, so a tool loop's growing history is cache reads (~0.1x input) instead of
	// full-price input; compatible with the explicit system-block marker (same 5-minute TTL).
	CacheControl *claudeCacheControl `json:"cache_control,omitempty"`
}

// claudeOutputConfig is the Messages request's output_config object; its effort field is Claude's reasoning-effort lever.
type claudeOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// claudeCacheControl marks a prompt-cache breakpoint (top level: automatic caching). Cache reads bill
// ~0.025-0.1x the input rate by model, 5-minute writes 1.25x — it pays off whenever the same prefix is re-sent.
type claudeCacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

var ephemeralCache = &claudeCacheControl{Type: "ephemeral"}

// claudeThinking enables adaptive extended thinking — the required form on Opus 4.7+ (manual budget_tokens is
// rejected there). The blocks are recorded for verbatim replay in a tool loop, never shown.
type claudeThinking struct {
	Type string `json:"type"` // "adaptive"
}

// claudeSystemBlock is one system-prompt content block. Sending system as an array is what lets us attach cache_control to it.
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

// claudeMessage.Content is either a plain string (text-only turn) or an array of content blocks, hence any.
type claudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// claudeBlock is one content block: text, tool_use (assistant), or tool_result (user).
type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"` // tool_result payload
	// IsError marks a tool_result whose tool failed (the tool's text starts with "error:"), so the model reads
	// it as a failed call rather than as data.
	IsError bool `json:"is_error,omitempty"`
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
	// Cache tokens are billed separately (reads 0.025-0.1× by family, writes 1.25×) and are EXCLUDED from
	// input_tokens — folded into TokensIn as the total and carried separately for cached-rate pricing.
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

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

// toClaudeMessages converts neutral messages to Claude's wire format: tool results for an assistant turn are
// coalesced into one user message (Anthropic requires them together in the next user turn), and an assistant
// turn carrying this route's replay record is sent as that record verbatim — the API wants its blocks back
// complete and unmodified in a tool loop.
func toClaudeMessages(msgs []Message, owner string) []claudeMessage {
	var out []claudeMessage
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role == "assistant" {
			if blocks, ok := claudeReplayBlocks(m, owner); ok {
				out = append(out, claudeMessage{Role: "assistant", Content: blocks})
				continue
			}
		}
		switch {
		case m.Role == "tool":
			// Gather this and any immediately-following tool results.
			var blocks []claudeBlock
			for i < len(msgs) && msgs[i].Role == "tool" {
				blocks = append(blocks, claudeBlock{
					Type:      "tool_result",
					ToolUseID: msgs[i].ToolCallID,
					Content:   msgs[i].Content,
					IsError:   claudeToolResultIsError(msgs[i].Content),
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
			// A blank turn (an interrupted answer) is rejected by the Messages API; consecutive same-role turns
			// are merged by it, so dropping is safe. The last message is kept so the request is never emptied.
			if strings.TrimSpace(m.Content) == "" && i < len(msgs)-1 {
				continue
			}
			out = append(out, claudeMessage{Role: m.Role, Content: m.Content})
		}
	}
	return out
}

// claudeToolResultIsError reports a tool result that is a failure: the chat tools answer "error: …" when a call fails.
func claudeToolResultIsError(content string) bool {
	return strings.HasPrefix(content, "error:")
}

// claudeReplayTypes are the assistant content-block types a replay record may hold — everything parseClaudeSSE
// and Chat record. A record holding anything else is not replayed (the turn is rebuilt instead).
var claudeReplayTypes = map[string]bool{
	"thinking":          true,
	"redacted_thinking": true,
	"text":              true,
	"tool_use":          true,
}

// claudeReplayBlocks returns an assistant turn's replay record for verbatim resending, or ok=false when the
// turn must be rebuilt: no record, another provider's, not an array of Claude blocks, or tool_use blocks not
// exactly the turn's tool calls in order.
func claudeReplayBlocks(m Message, owner string) (json.RawMessage, bool) {
	if len(m.ProviderItems) == 0 || owner == "" || m.ProviderItemsFrom != owner {
		return nil, false
	}
	var blocks []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal(m.ProviderItems, &blocks) != nil || len(blocks) == 0 {
		return nil, false
	}
	var ids []string
	for _, b := range blocks {
		if !claudeReplayTypes[b.Type] {
			return nil, false
		}
		if b.Type == "tool_use" {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) != len(m.ToolCalls) {
		return nil, false
	}
	for i, tc := range m.ToolCalls {
		if tc.ID != ids[i] {
			return nil, false
		}
	}
	return m.ProviderItems, true
}

// claudeReplayRecord joins an assistant turn's content blocks, in order, into its replay record
// (Response/Chunk.ProviderItems); nil when there is nothing to record or a block cannot be replayed.
func claudeReplayRecord(blocks []json.RawMessage) json.RawMessage {
	if len(blocks) == 0 {
		return nil
	}
	for _, b := range blocks {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &head) != nil || !claudeReplayTypes[head.Type] {
			return nil
		}
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return nil
	}
	return raw
}

func (c *ClaudeProvider) replayOwner() string {
	if c.itemsOwner != "" {
		return c.itemsOwner
	}
	return c.ID()
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

// claudeModelVersionRe finds the family and version in a Claude model or deployment name: the major version
// and an optional one- or two-digit minor (a longer number is a snapshot date: "claude-opus-4-20250514" is 4).
var claudeModelVersionRe = regexp.MustCompile(`(opus|sonnet|haiku|fable|mythos)-(\d{1,2})(?:[-.](\d{1,2}))?(?:[^0-9]|$)`)

// claudeAlwaysThinkingFamilyRe finds the Fable and Mythos families in a name with or without a version.
var claudeAlwaysThinkingFamilyRe = regexp.MustCompile(`(?:^|[^a-z])(?:fable|mythos)(?:[^a-z]|$)`)

// claudeRemovesSampling reports whether a model rejects temperature/top_p/top_k with a 400: Opus 4.7+, every
// generation-5+ model, and the Fable/Mythos families. Callers pass the SERVED model when known (policyModel);
// a name that reveals nothing keeps temperature, and send repairs a rejection once.
func claudeRemovesSampling(model string) bool {
	lower := strings.ToLower(model)
	if claudeAlwaysThinkingFamilyRe.MatchString(lower) {
		return true
	}
	m := claudeModelVersionRe.FindStringSubmatch(lower)
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

// policyModel is the lower-cased name the sampling policy is decided from: the model the deployment serves
// when known (servedModelFor), else the requested model / deployment name.
func (c *ClaudeProvider) policyModel(model string) string {
	if c.servedModelFor != nil {
		if served := strings.TrimSpace(c.servedModelFor(model)); served != "" {
			return strings.ToLower(served)
		}
	}
	return strings.ToLower(model)
}

// buildClaudeSystem wraps the system prompt in a single cached text block: cache_control caches the whole
// stable prefix (tools render before system), so the tool loop's repeated turns reuse it. nil when empty.
func buildClaudeSystem(prompt string) []claudeSystemBlock {
	if prompt == "" {
		return nil
	}
	return []claudeSystemBlock{{Type: "text", Text: prompt, CacheControl: ephemeralCache}}
}

// buildBody assembles the wire request shared by Chat and Stream: structured (cached) system, tools, messages, model-conditional sampling/thinking.
func (c *ClaudeProvider) buildBody(req Request, stream bool) claudeRequest {
	body := claudeRequest{
		Model:        req.Model,
		MaxTokens:    orDefault(req.MaxTokens, 8192),
		System:       buildClaudeSystem(req.SystemPrompt),
		Messages:     toClaudeMessages(req.Messages, c.replayOwner()),
		Tools:        toClaudeTools(req.Tools),
		Stream:       stream,
		CacheControl: ephemeralCache,
	}
	if claudeRemovesSampling(c.policyModel(req.Model)) {
		// These models reject non-default sampling params — omitted — and get adaptive thinking explicitly
		// (omitted means OFF on Opus 4.7/4.8, adaptive on 5.x/Fable/Mythos); thinking survives tool steps
		// via the verbatim replay, and where that is impossible every block drops — adaptive needs none.
		body.Thinking = &claudeThinking{Type: "adaptive"}
		// The reasoning-effort lever, only here — the models that reject sampling parameters accept output_config.effort.
		if req.ReasoningEffort != "" {
			body.OutputConfig = &claudeOutputConfig{Effort: req.ReasoningEffort}
		}
	} else if t, ok := claudeTemperature(req); ok {
		body.Temperature = &t
	}
	return body
}

// claudeTemperature is the caller's temperature clamped to Claude's [0, 1]: sent when the caller set it (0
// included) or when non-zero — an unset 0 means "the model's default" and is omitted.
func claudeTemperature(req Request) (float64, bool) {
	if !req.TemperatureSet && req.Temperature == 0 {
		return 0, false
	}
	return min(max(req.Temperature, 0), 1), true
}

// claudeErrorMessageMax bounds the service text kept on an error.
const claudeErrorMessageMax = 500

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

// claudeSamplingParam is the learnedRepairs entry of a deployment that rejects temperature: later requests omit it.
const claudeSamplingParam = "temperature"

// send posts the request and returns a 200 with its body unread, or the rejection read and closed. A 400
// naming `temperature`/`top_p` while the request carries one is repaired once and memoised per endpoint and
// deployment (learnedRepairs) — the safety net for a custom deployment name the policy cannot know.
func (c *ClaudeProvider) send(ctx context.Context, body claudeRequest) (*http.Response, []byte, error) {
	memoKey := c.endpoint() + "\x00" + body.Model
	if body.Temperature != nil && slices.Contains(repairsFor(memoKey), claudeSamplingParam) {
		body.Temperature = nil // omitted
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
		if !repaired && body.Temperature != nil && claudeSamplingRejected(resp.StatusCode, errBody) {
			logger.Debug("Claude rejected the sampling parameters — retrying without temperature",
				"provider", c.logLabel(), "model", body.Model)
			body.Temperature = nil
			repaired = true
			continue
		}
		return resp, errBody, nil
	}
}

// claudeSamplingRejected reports whether a rejection is the model refusing sampling parameters: an
// invalid-request 400 whose message names `temperature` or `top_p`.
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

func (c *ClaudeProvider) logLabel() string {
	if c.label != "" {
		return c.label
	}
	return "claude"
}

// claudeRequestIDs are the correlation ids of a Messages response: Anthropic's request-id and the gateway's
// (apim-request-id). Anthropic's Foundry page asks for BOTH when contacting support; either may be empty.
func claudeRequestIDs(resp *http.Response) (anthropicID, gatewayID string) {
	if resp == nil {
		return "", ""
	}
	return resp.Header.Get("request-id"), upstreamRequestID(resp)
}

// claudeRequestIDSuffix formats the request ids present for an error text: ", request id req_…, apim-request-id …".
func claudeRequestIDSuffix(resp *http.Response) string {
	anthropicID, gatewayID := claudeRequestIDs(resp)
	var s string
	if anthropicID != "" {
		s += ", request id " + anthropicID
	}
	if gatewayID != "" && gatewayID != anthropicID {
		s += ", apim-request-id " + gatewayID
	}
	return s
}

// logUpstreamRejection records a non-200's status and request ids: a parameter rejection may still be
// repaired by send, so it logs at debug.
func (c *ClaudeProvider) logUpstreamRejection(resp *http.Response, model string) {
	anthropicID, gatewayID := claudeRequestIDs(resp)
	if anthropicID == "" && gatewayID == "" {
		return
	}
	attrs := []any{"provider", c.logLabel(), "model", model, "status", resp.StatusCode,
		"requestId", anthropicID, "apimRequestId", gatewayID}
	if parameterRejectionStatus(resp.StatusCode) {
		logger.Debug("AI upstream rejection", attrs...)
		return
	}
	logger.Warn("AI upstream error", attrs...)
}

// permissionDeniedMessage is the explanation kept on a permission-denied error, with a fallback when the service sent none.
func permissionDeniedMessage(msg string) string {
	if msg = oneLine(strings.Join(strings.Fields(msg), " "), claudeErrorMessageMax); msg != "" {
		return msg
	}
	return "the credential is not allowed to use this model"
}

// classifyError maps a non-200 Messages response onto the shared errors, STATUS first (a bodyless gateway
// 502/429 still counts for retry/breaker); 401/403 go to the unauthorized hook, a permission_error is a
// permission denial, every other 401/403 an invalid credential.
func (c *ClaudeProvider) classifyError(resp *http.Response, body []byte, stream bool) error {
	status := resp.StatusCode
	var apiErr claudeErrorResp
	_ = json.Unmarshal(body, &apiErr)
	msg := oneLine(strings.Join(strings.Fields(apiErr.Error.Message), " "), claudeErrorMessageMax)
	suffix := claudeRequestIDSuffix(resp)
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

// claudeRefusalError is a response Claude ended with stop_reason "refusal" (its safety classifiers declined).
// Deterministic like a content-filter block, so ErrContentFiltered: never retried, never a breaker failure.
func claudeRefusalError() error {
	return fmt.Errorf("%w: the model declined to respond (stop_reason refusal)", ErrContentFiltered)
}

// ErrOutputLimit is a completion that stopped at its output cap (max_tokens or the context window) before
// producing any answer text or tool call. Deterministic, never retried (PROVIDER_OUTPUT_LIMIT); the call was billed.
var ErrOutputLimit = errors.New("the model reached its output limit before answering")

// claudeOutputLimitStop reports a stop_reason that cut the output short: the max_tokens cap, or the context
// window filling during generation (4.5+ models stop with model_context_window_exceeded instead of a 400).
func claudeOutputLimitStop(reason string) bool {
	return reason == "max_tokens" || reason == "model_context_window_exceeded"
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
	// The same content blocks undecoded, for the replay record: every field exactly as received.
	var raw struct {
		Content []json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(respBody, &raw)
	if parsed.StopReason == "refusal" {
		return nil, claudeRefusalError()
	}

	// Aggregate text blocks and collect tool_use blocks: with tools enabled a response may contain only
	// tool_use blocks, so text is not required.
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
	if text == "" && len(toolCalls) == 0 && claudeOutputLimitStop(parsed.StopReason) {
		// Cut at the cap before any answer — typically a thinking model that spent max_tokens reasoning.
		return nil, fmt.Errorf("%w (stop_reason %s)", ErrOutputLimit, parsed.StopReason)
	}
	if len(parsed.Content) == 0 {
		return nil, errors.New("claude returned no content")
	}

	return &Response{
		Content:          text,
		TokensIn:         parsed.Usage.totalInputTokens(),
		TokensOut:        parsed.Usage.OutputTokens,
		CacheReadTokens:  parsed.Usage.CacheReadInputTokens,
		CacheWriteTokens: parsed.Usage.CacheCreationInputTokens,
		FinishReason:     parsed.StopReason,
		ToolCalls:        toolCalls,
		ServedModel:      parsed.Model,
		ProviderItems:    claudeReplayRecord(raw.Content),
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

// claudeCountTokensRequest is the count_tokens body: the same model, system, messages, tools and thinking
// buildBody assembles, without max_tokens, stream, temperature/top_p and the top-level cache marker.
type claudeCountTokensRequest struct {
	Model    string              `json:"model"`
	System   []claudeSystemBlock `json:"system,omitempty"`
	Messages []claudeMessage     `json:"messages"`
	Tools    []claudeTool        `json:"tools,omitempty"`
	Thinking *claudeThinking     `json:"thinking,omitempty"`
}

// countTokensParam is the learnedRepairs entry of an endpoint with no count_tokens route (404): remembered per
// endpoint so later calls fail fast instead of paying the rejection round trip every turn.
const countTokensParam = "count_tokens"

// anthropicNotFound reports whether a 404 body is Anthropic's own error envelope ({"error":{"type":
// "not_found_error"}}): the route exists and refused THIS request, unlike a gateway's route-level 404.
func anthropicNotFound(body []byte) bool {
	var env struct {
		Type  string            `json:"type"`
		Error claudeErrorDetail `json:"error"`
	}
	return json.Unmarshal(body, &env) == nil && env.Type == "error" && env.Error.Type == "not_found_error"
}

// CountTokens asks the provider's own tokenizer how many input tokens req will consume — the OpenAI BPE is
// ~30% low on the 4.7+ tokenizer, and an exact count re-checks a near-window prompt. A route 404 is memoised
// per endpoint; Anthropic's own not_found_error is not (one request's unknown model must not disable counting).
func (c *ClaudeProvider) CountTokens(ctx context.Context, req Request) (int, error) {
	url := c.endpoint() + "/count_tokens"
	if slices.Contains(repairsFor(url), countTokensParam) {
		return 0, fmt.Errorf("claude count_tokens: %s has no count_tokens route (memoised)", c.logLabel())
	}
	body := c.buildBody(req, false)
	countReq := claudeCountTokensRequest{
		Model:    body.Model,
		System:   body.System,
		Messages: body.Messages,
		Tools:    body.Tools,
		Thinking: body.Thinking,
	}
	jsonBody, err := json.Marshal(countReq)
	if err != nil {
		return 0, fmt.Errorf("marshal count_tokens request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return 0, fmt.Errorf("build count_tokens request: %w", err)
	}
	if err := c.auth(ctx, httpReq); err != nil {
		return 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", claudeAPIVersion)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("claude count_tokens request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("read count_tokens response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound && !anthropicNotFound(respBody) {
			rememberRepairs(url, map[string]bool{countTokensParam: true})
			return 0, fmt.Errorf("claude count_tokens: no such route (status 404), memoised for this endpoint")
		}
		return 0, c.classifyError(resp, respBody, false)
	}
	var parsed struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, fmt.Errorf("parse count_tokens response: %w", err)
	}
	return parsed.InputTokens, nil
}
