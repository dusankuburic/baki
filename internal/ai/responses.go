package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"pad-core/ai/scrubber"
	"pad-core/logger"
)

// The OpenAI Responses API as served by a Foundry PROJECT endpoint
// ({project}/openai/v1/responses) is how baki talks to Foundry agents: the
// request names the agent (agent_reference) and carries the conversation
// STATELESSLY — every turn re-sends the history as input items, the same way
// chat completions do, so nothing about server-side conversations or run
// expiry has to be tracked, and a late tool result is just the next request.
//
// The agent's own definition (portal / CI) decides its instructions and
// tools. baki does not send request-level tools with an agent reference;
// when the agent declares function tools named like baki's flow tools
// (search_flow, get_block, …) the model's function calls come back as
// ToolCalls on the Done chunk, the chat service's tool loop runs them, and
// the results go back as function_call_output items. Tools the agent runs
// server-side (web search, file search, MCP, code interpreter) surface as
// ToolEvent chunks for the client's tool trail.
//
// Only function calls and text carry over between tool-loop iterations:
// under store:false the service cannot resolve replayed hosted items (web
// search, MCP, code interpreter) or reasoning, so an agent may repeat a
// hosted step in a later iteration (accepted risk R15).

type responsesRequest struct {
	// AgentReference names the agent. The service rejects the older `agent`
	// key ("The 'agent' property is deprecated. Use 'agent_reference'").
	AgentReference  *agentRef       `json:"agent_reference,omitempty"`
	Model           string          `json:"model,omitempty"`
	Instructions    string          `json:"instructions,omitempty"`
	Input           []responsesItem `json:"input"`
	Tools           []any           `json:"tools,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Store           *bool           `json:"store,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	// No temperature: the agent definition (portal / CI) owns its sampling
	// settings, and baki's provider-level temperature must not override them
	// — it also cost reasoning-backed agents a rejection round trip per turn.
}

type agentRef struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// responsesItem is one input item: a message (string content), a replayed
// function_call, or a function_call_output.
type responsesItem struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

// toResponsesInput maps the neutral history onto Responses input items. The
// system prompt (flow context, guidelines, custom instructions) rides as the
// first message with the system role; the agent keeps its own instructions.
func toResponsesInput(systemPrompt string, msgs []Message) []responsesItem {
	var out []responsesItem
	if systemPrompt != "" {
		out = append(out, responsesItem{Type: "message", Role: "system", Content: systemPrompt})
	}
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			out = append(out, responsesItem{Type: "function_call_output", CallID: m.ToolCallID, Output: nonEmptyToolOutput(m.Content)})
		case "assistant":
			if m.Content != "" {
				out = append(out, responsesItem{Type: "message", Role: "assistant", Content: m.Content})
			}
			for _, tc := range m.ToolCalls {
				args := string(tc.Input)
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				out = append(out, responsesItem{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: args})
			}
		default:
			role := m.Role
			if role == "" {
				role = "user"
			}
			out = append(out, responsesItem{Type: "message", Role: role, Content: m.Content})
		}
	}
	return out
}

// nonEmptyToolOutput keeps a function_call_output item valid: `output` is
// required by the Responses API and marshals with omitempty, so an empty
// result goes out as a placeholder the model can read instead of a 400.
func nonEmptyToolOutput(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no output)"
	}
	return s
}

// minResponsesOutputTokens is the Responses API's floor for max_output_tokens
// (a smaller value is a 400 "integer below minimum value", which is not a
// repairable parameter): a smaller caller cap — the connection test's — is
// raised to it.
const minResponsesOutputTokens = 16

// agentRequest builds the Responses request for an agent turn.
func (p *AzureFoundryProvider) agentRequest(req Request, stream bool) responsesRequest {
	noStore := false
	body := responsesRequest{
		AgentReference: &agentRef{Type: "agent_reference", Name: agentNameOf(req.Model)},
		Input:          toResponsesInput(req.SystemPrompt, req.Messages),
		Stream:         stream,
		// Flow content must not be retained server-side beyond the request;
		// an endpoint that rejects the parameter fails the turn (see
		// postResponses) instead of being retried without it, which the
		// Responses API would default to storing.
		Store: &noStore,
	}
	if req.MaxTokens > 0 {
		body.MaxOutputTokens = max(req.MaxTokens, minResponsesOutputTokens)
	}
	return body
}

func (p *AzureFoundryProvider) agentConfigError() error {
	if p.cfg.ProjectEndpoint == "" || p.cfg.Auth.Mode != AzureAuthModeEntra {
		return fmt.Errorf("%w: Foundry agents need a project endpoint and Microsoft Entra ID credentials", ErrProviderConfigInvalid)
	}
	return nil
}

// postResponses POSTs to {project}/openai/v1/responses with the same parameter
// repair as the chat-completions path (max_output_tokens may be refused for a
// given agent or model): one rejection round trip per parameter, capped,
// because a model can refuse several. `store` is the one parameter that must
// never be repaired — the Responses API defaults it to true, so resending
// without it would retain flow content server-side, the opposite of why it is
// sent; an endpoint that rejects it fails the turn instead.
//
// The body is encoded once; its generic map form, which a repair mutates, is
// built only when a repair is needed (the three-way conversion on every
// request cost milliseconds and MBs of garbage per turn and tool iteration).
func (p *AzureFoundryProvider) postResponses(ctx context.Context, body responsesRequest) (*http.Response, []byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	agentName := ""
	if body.AgentReference != nil {
		agentName = body.AgentReference.Name
	}
	var payload map[string]any
	ensurePayload := func() bool {
		return payload != nil || json.Unmarshal(jsonBody, &payload) == nil
	}
	remarshal := func() error {
		out, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		jsonBody = out
		return nil
	}
	endpoint := p.cfg.ProjectEndpoint + "/openai/v1/responses"
	// repaired guards the loop (each parameter at most once) and is what a
	// 200 memoises; learned marks the entries pre-applied from the memo.
	// Repairs learned from an earlier turn with the same agent go out up
	// front: each used to cost a rejection round trip — a full re-upload of
	// the history — on every turn and every tool iteration. The key (project
	// endpoint + agent name) holds no secret; `store` is never learned (its
	// rejection fails the turn) and never pre-applied.
	repaired := map[string]bool{}
	learned := map[string]bool{}
	memoKey := endpoint + "\x00" + agentName
	if params := repairsFor(memoKey); len(params) > 0 && ensurePayload() {
		for _, param := range params {
			if param == "store" || !repairParam(payload, param) {
				continue
			}
			repaired[param], learned[param] = true, true
		}
		if len(learned) > 0 {
			if err := remarshal(); err != nil {
				return nil, nil, err
			}
		}
	}
	memoStale := false
	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if body.Stream {
			httpReq.Header.Set("Accept", "text/event-stream")
		}
		if err := p.authorize(ctx, httpReq); err != nil {
			return nil, nil, err
		}
		resp, err := p.client.Do(httpReq)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == http.StatusOK {
			if len(repaired) > 0 && !memoStale {
				rememberRepairs(memoKey, repaired)
			}
			return resp, nil, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		param, paramOK := unsupportedParam(resp.StatusCode, errBody)
		// The store guard runs BEFORE the repair-budget cap: a store rejection
		// must fail the turn with this deliberate error even when the budget
		// is already spent, not surface as the raw 400 prose.
		if paramOK && param == "store" {
			return nil, nil, fmt.Errorf("%w: the Foundry endpoint rejected store=false; resending without it would retain flow content server-side, so the request is refused", ErrProviderConfigInvalid)
		}
		if !paramOK || len(repaired) >= maxResponsesRepairs {
			return resp, errBody, nil
		}
		if learned[param] {
			// The memo's rewrite of this parameter is what the model rejects
			// now: forget the memo and treat the parameter as freshly
			// rejected instead of failing the turn for the memo's lifetime.
			forgetRepairs(memoKey)
			memoStale = true
			delete(learned, param)
			delete(repaired, param)
		}
		if !repaired[param] && ensurePayload() && repairParam(payload, param) {
			repaired[param] = true
			if err := remarshal(); err != nil {
				return nil, nil, err
			}
			logger.Debug("azure ai foundry: responses parameter rejected — retrying without it", "param", param)
			continue
		}
		return resp, errBody, nil
	}
}

// maxResponsesRepairs caps postResponses' repair round trips per request, like
// send's maxSendRepairs: each parameter is repaired at most once anyway; the
// cap bounds a server that keeps naming new ones.
const maxResponsesRepairs = 4

// maxAgentResponseBytes caps the non-streaming agent response read. One byte
// more than the cap is read so an over-limit body can be told apart from a
// clean one — a silently truncated body would only surface later as a JSON
// parse error ("unexpected end of JSON input").
const maxAgentResponseBytes = 10 << 20

// agentChat is the non-streaming agent turn (the connection test).
func (p *AzureFoundryProvider) agentChat(ctx context.Context, req Request) (*Response, error) {
	if err := p.agentConfigError(); err != nil {
		return nil, err
	}
	resp, errBody, err := p.postResponses(ctx, p.agentRequest(req, false))
	if err != nil {
		return nil, fmt.Errorf("azure ai foundry agent request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, p.handleChatError(resp, errBody)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxAgentResponseBytes {
		return nil, fmt.Errorf("the Foundry agent response exceeded %d MB and cannot be read", maxAgentResponseBytes>>20)
	}
	var parsed responsesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse azure ai foundry agent response: %w", err)
	}
	if err := parsed.terminalError(); err != nil {
		return nil, err
	}
	out := &Response{ServedModel: parsed.Model, FinishReason: "stop"}
	if parsed.Usage != nil {
		out.TokensIn = parsed.Usage.InputTokens
		out.TokensOut = parsed.Usage.OutputTokens
	}
	var text strings.Builder
	approvals := map[string]bool{}
	for _, raw := range parsed.Output {
		// Items are decoded one by one: a hosted tool item of an unexpected
		// shape must not fail the whole turn.
		item, ok := decodeOutputItem(raw)
		if !ok {
			logger.Debug("azure ai foundry: skipping undecodable agent output item")
			continue
		}
		switch item.Type {
		case "message":
			text.WriteString(item.messageText())
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, item.toolCall())
		case "mcp_approval_request":
			approvals[mcpToolName(item)] = true
		}
	}
	if len(approvals) > 0 {
		return nil, mcpApprovalError(approvals)
	}
	out.Content = text.String()
	if len(out.ToolCalls) > 0 {
		out.FinishReason = "tool_calls"
	}
	if parsed.Status == "incomplete" && parsed.IncompleteDetails != nil && parsed.IncompleteDetails.Reason == "max_output_tokens" {
		out.FinishReason = "length"
	}
	return out, nil
}

// agentStream is the streaming agent turn.
func (p *AzureFoundryProvider) agentStream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	if err := p.agentConfigError(); err != nil {
		return err
	}
	resp, errBody, err := p.postResponses(ctx, p.agentRequest(req, true))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return p.handleStreamError(resp, errBody)
	}
	// The parser returns at response.completed, before the stream's chunked
	// terminator: drain the rest so the HTTP/1.1 connection goes back to the
	// pool instead of being discarded (a new TCP + TLS handshake per turn).
	defer drainAndClose(resp.Body)
	return parseResponsesSSE(resp.Body, onChunk)
}

// --- wire types ---

// responsesResponseMeta is the response object without its output items: the
// streamed terminal events (response.completed / incomplete / failed) are
// decoded through it, so no output item shape can sink the event that
// carries usage, the served model and the finish state.
type responsesResponseMeta struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Usage  *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *responsesError `json:"error"`
}

// responsesResponse is the non-streaming response; its output items stay raw
// and are decoded one by one (decodeOutputItem).
type responsesResponse struct {
	responsesResponseMeta
	Output []json.RawMessage `json:"output"`
}

// terminalError maps a failed/filtered response onto the shared sentinels.
func (r *responsesResponseMeta) terminalError() error {
	if r.Status == "failed" {
		if r.Error != nil {
			return classifyResponsesError(r.Error.Code, r.Error.Type, r.Error.Message)
		}
		return fmt.Errorf("azure ai foundry agent run failed")
	}
	if r.Status == "incomplete" && r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "content_filter" {
		return fmt.Errorf("%w: the response was stopped by the content filter", ErrContentFiltered)
	}
	return nil
}

// responsesOutputItem is one output item. Output and Error stay raw: Foundry
// tool items carry string, object or array outputs (ToolCallOutputContent)
// and MCP items an object error — read them through rawText.
type responsesOutputItem struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Status      string          `json:"status"`
	Role        string          `json:"role"`
	CallID      string          `json:"call_id"`
	Name        string          `json:"name"`
	ServerLabel string          `json:"server_label"`
	Arguments   string          `json:"arguments"`
	Output      json.RawMessage `json:"output"`
	Error       json.RawMessage `json:"error"`
	Content     []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

// messageText is a message item's answer: output_text parts and refusal
// parts (a refusal is the model's answer too, not silence — dropping it left
// the user an empty bubble with no explanation).
func (it responsesOutputItem) messageText() string {
	var b strings.Builder
	for _, c := range it.Content {
		switch c.Type {
		case "output_text":
			b.WriteString(c.Text)
		case "refusal":
			b.WriteString(c.Refusal)
		}
	}
	return b.String()
}

// decodeOutputItem decodes one output item. When the full decode fails (a
// preview tool item with an unexpected field shape), the item's string header
// (id, type, status, names) is still returned so the tool trail and the
// replay guard keep working; ok is false only for an unreadable item.
func decodeOutputItem(raw json.RawMessage) (responsesOutputItem, bool) {
	var item responsesOutputItem
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return item, false
	}
	if err := json.Unmarshal(raw, &item); err == nil {
		return item, true
	}
	var head struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		Status      string `json:"status"`
		CallID      string `json:"call_id"`
		Name        string `json:"name"`
		ServerLabel string `json:"server_label"`
	}
	if err := json.Unmarshal(raw, &head); err != nil || head.Type == "" {
		return responsesOutputItem{}, false
	}
	return responsesOutputItem{ID: head.ID, Type: head.Type, Status: head.Status, CallID: head.CallID, Name: head.Name, ServerLabel: head.ServerLabel}, true
}

// rawText flattens a string | object | array | null JSON value for display:
// a string is unquoted, an object or array compacted, null or absent is "".
func rawText(r json.RawMessage) string {
	r = bytes.TrimSpace(r)
	if len(r) == 0 || bytes.Equal(r, []byte("null")) {
		return ""
	}
	if r[0] == '"' {
		var s string
		if json.Unmarshal(r, &s) == nil {
			return s
		}
	}
	var buf bytes.Buffer
	if json.Compact(&buf, r) == nil {
		return buf.String()
	}
	return string(r)
}

// errorText is rawText for an error value, preferring an object's message
// ({"type":"mcp_protocol_error","code":32600,"message":"Session terminated"}).
func errorText(r json.RawMessage) string {
	r = bytes.TrimSpace(r)
	if len(r) > 0 && r[0] == '{' {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(r, &e) == nil && strings.TrimSpace(e.Message) != "" {
			return e.Message
		}
	}
	return rawText(r)
}

// mcpToolName names an MCP item's tool as "<server_label>/<name>".
func mcpToolName(it responsesOutputItem) string {
	switch {
	case it.ServerLabel != "" && it.Name != "":
		return it.ServerLabel + "/" + it.Name
	case it.Name != "":
		return it.Name
	case it.ServerLabel != "":
		return it.ServerLabel
	}
	return "(unnamed)"
}

// mcpApprovalError explains a run that stopped for MCP tool approval (an
// mcp_approval_request item — Foundry's default for MCP tools). The app
// cannot answer approvals (stateless requests, no stored response to
// continue), so the turn ends with this message instead of an empty answer.
// A plain error on purpose: the chat service passes its text through as is.
func mcpApprovalError(tools map[string]bool) error {
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("the Foundry agent asked for approval to run MCP tool %s, and this app cannot approve MCP tool calls: set require_approval to \"never\" for that MCP tool in the agent definition, then try again", strings.Join(names, ", "))
}

func (it responsesOutputItem) toolCall() ToolCall {
	args := it.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	id := it.CallID
	if id == "" {
		id = it.ID
	}
	return ToolCall{ID: id, Name: it.Name, Input: json.RawMessage(args)}
}

// responsesCallKey keys pending function calls, preferring call_id and
// falling back to id — the same preference toolCall uses for the emitted
// ToolCall.ID. Canonicalising BOTH the added and the done event this way is
// what pairs them when the shapes differ (a Foundry added event carrying only
// call_id, a done event carrying id and call_id): keying by id-first made the
// two keys disagree and double-emitted the call. Argument-delta events still
// reference the item by its item_id; a stream that keys its added event by
// call_id while its deltas use the id accumulates nothing (the done event
// carries the complete arguments), an accepted limitation.
func responsesCallKey(id, callID string) string {
	if callID != "" {
		return callID
	}
	return id
}

type responsesError struct {
	Code    string `json:"code"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

func classifyResponsesError(code, typ, msg string) error {
	switch {
	case code == "rate_limit_exceeded" || typ == "too_many_requests" || typ == "rate_limit_error":
		return fmt.Errorf("%w: %s", ErrRateLimited, msg)
	case typ == "server_error" || code == "server_error" || code == "no_capacity":
		return fmt.Errorf("%w: %s", ErrProviderDown, msg)
	case code == "content_filter":
		return fmt.Errorf("%w: %s", ErrContentFiltered, msg)
	case code == "" && msg == "":
		return fmt.Errorf("azure ai foundry agent run failed")
	}
	return fmt.Errorf("azure ai foundry agent error: %s", msg)
}

// hostedToolLabels names the server-side tool steps a Foundry agent run can
// contain, as they appear in the tool trail: the OpenAI hosted tools plus
// Foundry's own tool items (v1 spec). Only the *_call items are listed — the
// matching *_call_output items are not separate steps. An unlisted
// server-side item still blocks a replay (Chunk.ServerStep).
var hostedToolLabels = map[string]string{
	"web_search_call":                   "Agent · web search",
	"file_search_call":                  "Agent · file search",
	"code_interpreter_call":             "Agent · code interpreter",
	"mcp_call":                          "Agent · MCP tool",
	"mcp_list_tools":                    "Agent · MCP tools",
	"image_generation_call":             "Agent · image generation",
	"computer_call":                     "Agent · computer use",
	"a2a_preview_call":                  "Agent · connected agent",
	"azure_ai_search_call":              "Agent · Azure AI Search",
	"bing_grounding_call":               "Agent · Bing search",
	"bing_custom_search_preview_call":   "Agent · Bing custom search",
	"openapi_call":                      "Agent · OpenAPI tool",
	"azure_function_call":               "Agent · Azure Function",
	"sharepoint_grounding_preview_call": "Agent · SharePoint",
	"fabric_dataagent_preview_call":     "Agent · Fabric data agent",
	"browser_automation_preview_call":   "Agent · browser automation",
	"memory_search_call":                "Agent · memory search",
}

// hostedToolNamed lists the item types whose trail label names the tool or
// agent that ran (the item's `name`).
var hostedToolNamed = map[string]bool{
	"mcp_call":            true,
	"a2a_preview_call":    true,
	"openapi_call":        true,
	"azure_function_call": true,
}

func hostedToolLabel(item responsesOutputItem) string {
	label, ok := hostedToolLabels[item.Type]
	if !ok {
		return ""
	}
	if hostedToolNamed[item.Type] && item.Name != "" {
		return label + ": " + item.Name
	}
	return label
}

// clientSideItem reports the output items that are not server-side steps:
// the answer text, reasoning and the function calls the app itself runs.
func clientSideItem(typ string) bool {
	switch typ {
	case "message", "reasoning", "function_call":
		return true
	}
	return false
}

// responsesEvent is the union of the streaming event payloads baki reads.
// Item stays raw (decodeOutputItem) and Response is the output-less meta, so
// an unexpected item shape can never drop an event.
type responsesEvent struct {
	Type        string                 `json:"type"`
	Delta       string                 `json:"delta"`
	Arguments   string                 `json:"arguments"`
	ItemID      string                 `json:"item_id"`
	OutputIndex int                    `json:"output_index"`
	Item        json.RawMessage        `json:"item"`
	Response    *responsesResponseMeta `json:"response"`
	Error       *responsesError        `json:"error"`
	// Top-level error fields (the `error` event carries them inline).
	Code    string `json:"code"`
	Message string `json:"message"`
}

// maxResponsesSSELine caps one SSE line. A Responses stream carries a
// generated image inline as base64 in output_item.done and again in
// response.completed (which repeats every output item), so a single line can
// be several MB; the scanner buffer only grows as far as the longest line
// actually received.
const maxResponsesSSELine = 32 << 20

type pendingResponsesCall struct {
	index int
	item  responsesOutputItem
	args  strings.Builder
	final bool
}

// parseResponsesSSE turns the Responses streaming events into chunks: text
// deltas as they arrive, hosted tool steps as ToolEvents (or a ServerStep
// marker for a server-side item without a trail label), function calls and
// usage on the Done chunk, failures as error chunks.
func parseResponsesSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxResponsesSSELine)

	var (
		parseErrors int
		calls       = map[string]*pendingResponsesCall{}
		// started pairs a hosted step's start and end by output_index:
		// Foundry's own tool items carry a call_id and no id.
		started = map[int]time.Time{}
		// approvals collects the MCP tools the run stopped for (see
		// mcpApprovalError), reported at the terminal event.
		approvals = map[string]bool{}
		// streamedText counts the text streamed per message item id (and
		// under "" for deltas without one); totalStreamed across all. They
		// decide whether a message's done event still carries unseen text.
		streamedText  = map[string]int{}
		totalStreamed int
		// servedModel is the response's model, seeded from EVERY event that
		// carries the response object (response.created / in_progress come
		// before any output), and carried on text and error chunks: a run cut
		// before response.completed (Stop, the idle watchdog, a truncated
		// stream) is still priced by the model that answered instead of the
		// fallback ceiling.
		servedModel string
	)
	dataPrefix := []byte("data: ")
	// emitted reports whether the event being processed produced a chunk; emit
	// is the switch's only chunk outlet, so the flag cannot drift out of sync.
	var emitted bool
	emit := func(c Chunk) {
		emitted = true
		onChunk(c)
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, dataPrefix) {
			continue
		}
		data := line[len(dataPrefix):]
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		var ev responsesEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			parseErrors++
			continue
		}
		emitted = false
		if ev.Response != nil && ev.Response.Model != "" {
			servedModel = ev.Response.Model
		}

		switch ev.Type {
		case "response.output_text.delta", "response.refusal.delta":
			if ev.Delta != "" {
				streamedText[ev.ItemID] += len(ev.Delta)
				totalStreamed += len(ev.Delta)
				emit(Chunk{Text: ev.Delta, ServedModel: servedModel})
			}

		case "response.output_item.added":
			item, ok := decodeOutputItem(ev.Item)
			if !ok {
				// An undecodable item is exactly the unknown-shape event the
				// keepalive below exists for; without this it would `continue`
				// past it and a long silent phase could still be cancelled.
				onChunk(Chunk{})
				continue
			}
			switch {
			case item.Type == "function_call":
				calls[responsesCallKey(item.ID, item.CallID)] = &pendingResponsesCall{index: ev.OutputIndex, item: item}
			case clientSideItem(item.Type):
			case item.Type == "mcp_approval_request":
				approvals[mcpToolName(item)] = true
				emit(Chunk{ServerStep: true})
			default:
				if label := hostedToolLabel(item); label != "" {
					started[ev.OutputIndex] = time.Now()
					emit(Chunk{ToolEvent: &ToolEvent{Name: item.Type, Label: label}})
				} else {
					// A server-side item without a trail label (a *_call_output,
					// a tool type added later): nothing to show, but a replay
					// of the request would run it again.
					emit(Chunk{ServerStep: true})
				}
			}

		case "response.function_call_arguments.delta":
			if c, ok := calls[ev.ItemID]; ok && !c.final {
				c.args.WriteString(ev.Delta)
			}

		case "response.function_call_arguments.done":
			if c, ok := calls[ev.ItemID]; ok {
				c.args.Reset()
				c.args.WriteString(ev.Arguments)
				c.final = true
			}

		case "response.output_item.done":
			item, ok := decodeOutputItem(ev.Item)
			if !ok {
				onChunk(Chunk{}) // see output_item.added
				continue
			}
			switch {
			case item.Type == "function_call":
				key := responsesCallKey(item.ID, item.CallID)
				c, ok := calls[key]
				if !ok {
					c = &pendingResponsesCall{index: ev.OutputIndex}
					calls[key] = c
				}
				c.item = item
				if item.Arguments != "" {
					c.args.Reset()
					c.args.WriteString(item.Arguments)
					c.final = true
				}
			case item.Type == "message":
				// A message whose text never streamed (delivered whole in the
				// done event, or refusal content) still carries the answer;
				// one that did stream is not repeated. Deltas without an item
				// id make the two indistinguishable, so nothing is repeated
				// then.
				if totalStreamed == 0 || (item.ID != "" && streamedText[item.ID] == 0 && streamedText[""] == 0) {
					if text := item.messageText(); text != "" {
						emit(Chunk{Text: text, ServedModel: servedModel})
					}
				}
			case clientSideItem(item.Type):
			case item.Type == "mcp_approval_request":
				approvals[mcpToolName(item)] = true
				emit(Chunk{ServerStep: true})
			default:
				label := hostedToolLabel(item)
				if label == "" {
					emit(Chunk{ServerStep: true}) // see output_item.added
					continue
				}
				var took int64
				if t0, ok := started[ev.OutputIndex]; ok {
					took = time.Since(t0).Milliseconds()
					delete(started, ev.OutputIndex)
				}
				emit(Chunk{ToolEvent: &ToolEvent{
					Name:  item.Type,
					Label: label,
					Done:  true,
					// "error": null is how a successful MCP call reports.
					OK:         item.Status != "failed" && item.Status != "incomplete" && rawText(item.Error) == "",
					DurationMs: took,
					Summary:    hostedToolSummary(item),
				}})
			}

		case "response.completed", "response.incomplete":
			var finish string
			tokensIn, tokensOut := 0, 0
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					tokensIn = ev.Response.Usage.InputTokens
					tokensOut = ev.Response.Usage.OutputTokens
				}
				if ev.Response.IncompleteDetails != nil {
					switch ev.Response.IncompleteDetails.Reason {
					case "content_filter":
						// Usage rides on the error chunk (as the MCP-approvals
						// branch below does) so the tokens spent are still billed.
						emit(Chunk{Error: fmt.Errorf("%w: the response was stopped by the content filter", ErrContentFiltered), TokensIn: tokensIn, TokensOut: tokensOut, ServedModel: servedModel})
						return nil
					default:
						// The run was cut short whatever the reason says
						// (max_output_tokens today, a reason invented later):
						// an unmapped reason must not finish as a clean stop.
						finish = "length"
					}
				}
			}
			if len(approvals) > 0 {
				// The run stopped for MCP tool approval and cannot continue
				// (not even with function outputs replayed): explain instead
				// of finishing with an empty answer. The usage rides on the
				// error chunk so the tokens spent are still billed.
				emit(Chunk{Error: mcpApprovalError(approvals), TokensIn: tokensIn, TokensOut: tokensOut, ServedModel: servedModel})
				return nil
			}
			toolCalls := assembleResponsesCalls(calls)
			if finish == "" {
				finish = "stop"
				if len(toolCalls) > 0 {
					finish = "tool_calls"
				}
			}
			emit(Chunk{Done: true, TokensIn: tokensIn, TokensOut: tokensOut, ToolCalls: toolCalls, FinishReason: finish, ServedModel: servedModel})
			return nil

		case "response.failed":
			tokensIn, tokensOut := 0, 0
			var failure error
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					tokensIn, tokensOut = ev.Response.Usage.InputTokens, ev.Response.Usage.OutputTokens
				}
				if ev.Response.Error != nil {
					failure = classifyResponsesError(ev.Response.Error.Code, ev.Response.Error.Type, ev.Response.Error.Message)
				}
			}
			if failure == nil {
				failure = classifyResponsesError("", "", "")
			}
			// Usage rides on the error chunk (as the content-filter and
			// MCP-approval branches do) so the tokens a failed run spent are
			// still billed by Azure's numbers, not a tokenizer estimate.
			emit(Chunk{Error: failure, TokensIn: tokensIn, TokensOut: tokensOut, ServedModel: servedModel})
			return nil

		case "error":
			code, typ, msg := ev.Code, "", ev.Message
			if ev.Error != nil {
				code, typ, msg = ev.Error.Code, ev.Error.Type, ev.Error.Message
			}
			emit(Chunk{Error: classifyResponsesError(code, typ, msg), ServedModel: servedModel})
			return nil
		}

		// An event that produced no chunk — reasoning summary parts and deltas
		// (an o-series / gpt-5 agent can reason for minutes before the first
		// output token), silent function-call accumulation, progress
		// notifications during hosted-tool runs (*.in_progress, *.searching,
		// *.interpreting) — still proves the run is alive, and the chat
		// service's idle watchdog only sees chunks: one empty keepalive per
		// event keeps a long reasoning phase from being cancelled as a stalled
		// stream (the chat-completions parser does the same).
		if !emitted {
			onChunk(Chunk{})
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading azure responses SSE stream: %w", err)
	}
	// Reaching here means the stream ended before a terminal event (every
	// terminal branch returns): the run was cut off, not completed.
	if parseErrors > 0 {
		return errStreamMalformed("azure responses", parseErrors)
	}
	return errStreamTruncated("azure responses")
}

// assembleResponsesCalls orders the accumulated function calls by output
// position and converts them to the neutral ToolCall.
func assembleResponsesCalls(calls map[string]*pendingResponsesCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	list := make([]*pendingResponsesCall, 0, len(calls))
	for _, c := range calls {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].index < list[j].index })
	out := make([]ToolCall, 0, len(list))
	for _, c := range list {
		item := c.item
		if item.Arguments == "" {
			item.Arguments = c.args.String()
		}
		out = append(out, item.toolCall())
	}
	return out
}

const (
	// hostedSummaryMax is the trail summary's length in bytes (same as the
	// local tools' summary).
	hostedSummaryMax = 120
	// hostedSummaryScan bounds how much of a (possibly multi-MB) tool output
	// is scrubbed to produce the summary.
	hostedSummaryScan = 4 << 10
)

// hostedToolSummary is the trail's one-line result for a finished hosted
// step: the tool's error or output when present, else nothing. The text is
// sent to the client and stored with the conversation, so it goes through the
// secret scrubber — before the cut, so a secret straddling it is still
// matched — and is cut on a rune boundary.
func hostedToolSummary(item responsesOutputItem) string {
	s := errorText(item.Error)
	if s == "" {
		s = rawText(item.Output)
	}
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s, _ = cutAtRune(s, hostedSummaryScan)
	s = strings.TrimSpace(scrubber.ScrubText(s))
	if cut, ok := cutAtRune(s, hostedSummaryMax); ok {
		s = cut + "…"
	}
	return s
}

// cutAtRune shortens s to at most n bytes without splitting a UTF-8
// character; ok reports whether anything was cut.
func cutAtRune(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
