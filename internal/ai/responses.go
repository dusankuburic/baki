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

type responsesRequest struct {
	Agent           *agentRef       `json:"agent,omitempty"`
	Model           string          `json:"model,omitempty"`
	Instructions    string          `json:"instructions,omitempty"`
	Input           []responsesItem `json:"input"`
	Tools           []any           `json:"tools,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Store           *bool           `json:"store,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Temperature     float64         `json:"temperature,omitempty"`
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
			out = append(out, responsesItem{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})
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

// agentRequest builds the Responses request for an agent turn.
func (p *AzureFoundryProvider) agentRequest(req Request, stream bool) responsesRequest {
	noStore := false
	body := responsesRequest{
		Agent:  &agentRef{Type: "agent_reference", Name: agentNameOf(req.Model)},
		Input:  toResponsesInput(req.SystemPrompt, req.Messages),
		Stream: stream,
		// Flow content must not be retained server-side beyond the request;
		// an endpoint that rejects the parameter has it stripped by the
		// repair round trip.
		Store: &noStore,
	}
	if req.MaxTokens > 0 {
		body.MaxOutputTokens = req.MaxTokens
	}
	if req.Temperature > 0 {
		body.Temperature = req.Temperature
	}
	return body
}

func (p *AzureFoundryProvider) agentConfigError() error {
	if p.cfg.ProjectEndpoint == "" || p.cfg.Auth.Mode != AzureAuthModeEntra {
		return fmt.Errorf("%w: Foundry agents need a project endpoint and Microsoft Entra ID credentials", ErrProviderConfigInvalid)
	}
	return nil
}

// postResponses POSTs to {project}/openai/v1/responses with the same one-shot
// parameter repair as the chat-completions path (store / temperature /
// max_output_tokens may be refused for a given agent or model).
func (p *AzureFoundryProvider) postResponses(ctx context.Context, body responsesRequest) (*http.Response, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	endpoint := p.cfg.ProjectEndpoint + "/openai/v1/responses"
	repaired := false
	for {
		jsonBody, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal request: %w", err)
		}
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
			return resp, nil, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		if !repaired {
			if param, ok := unsupportedParam(resp.StatusCode, errBody); ok && repairParam(payload, param) {
				repaired = true
				logger.Debug("azure ai foundry: responses parameter rejected — retrying without it", "param", param)
				continue
			}
		}
		return resp, errBody, nil
	}
}

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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
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
	for _, item := range parsed.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					text.WriteString(c.Text)
				}
			}
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, item.toolCall())
		}
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p.handleStreamError(resp, errBody)
	}
	return parseResponsesSSE(resp.Body, onChunk)
}

// --- wire types ---

type responsesResponse struct {
	ID     string                `json:"id"`
	Model  string                `json:"model"`
	Status string                `json:"status"`
	Output []responsesOutputItem `json:"output"`
	Usage  *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *responsesError `json:"error"`
}

// terminalError maps a failed/filtered response onto the shared sentinels.
func (r *responsesResponse) terminalError() error {
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

type responsesOutputItem struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Role        string `json:"role"`
	CallID      string `json:"call_id"`
	Name        string `json:"name"`
	ServerLabel string `json:"server_label"`
	Arguments   string `json:"arguments"`
	Output      string `json:"output"`
	Error       string `json:"error"`
	Content     []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
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
// contain, as they appear in the tool trail.
var hostedToolLabels = map[string]string{
	"web_search_call":       "Agent · web search",
	"file_search_call":      "Agent · file search",
	"code_interpreter_call": "Agent · code interpreter",
	"mcp_call":              "Agent · MCP tool",
	"mcp_list_tools":        "Agent · MCP tools",
	"image_generation_call": "Agent · image generation",
	"computer_call":         "Agent · computer use",
	"a2a_call":              "Agent · connected agent",
}

func hostedToolLabel(item responsesOutputItem) string {
	label, ok := hostedToolLabels[item.Type]
	if !ok {
		return ""
	}
	if item.Type == "mcp_call" && item.Name != "" {
		return label + ": " + item.Name
	}
	return label
}

// responsesEvent is the union of the streaming event payloads baki reads.
type responsesEvent struct {
	Type        string               `json:"type"`
	Delta       string               `json:"delta"`
	Arguments   string               `json:"arguments"`
	ItemID      string               `json:"item_id"`
	OutputIndex int                  `json:"output_index"`
	Item        *responsesOutputItem `json:"item"`
	Response    *responsesResponse   `json:"response"`
	Error       *responsesError      `json:"error"`
	// Top-level error fields (the `error` event carries them inline).
	Code    string `json:"code"`
	Message string `json:"message"`
}

type pendingResponsesCall struct {
	index int
	item  responsesOutputItem
	args  strings.Builder
	final bool
}

// parseResponsesSSE turns the Responses streaming events into chunks: text
// deltas as they arrive, hosted tool steps as ToolEvents, function calls and
// usage on the Done chunk, failures as error chunks.
func parseResponsesSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		doneSent    bool
		parseErrors int
		calls       = map[string]*pendingResponsesCall{}
		started     = map[string]time.Time{}
	)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var ev responsesEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			parseErrors++
			continue
		}

		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				onChunk(Chunk{Text: ev.Delta})
			}

		case "response.output_item.added":
			if ev.Item == nil {
				continue
			}
			if ev.Item.Type == "function_call" {
				calls[ev.Item.ID] = &pendingResponsesCall{index: ev.OutputIndex, item: *ev.Item}
				continue
			}
			if label := hostedToolLabel(*ev.Item); label != "" {
				started[ev.Item.ID] = time.Now()
				onChunk(Chunk{ToolEvent: &ToolEvent{Name: ev.Item.Type, Label: label}})
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
			if ev.Item == nil {
				continue
			}
			if ev.Item.Type == "function_call" {
				c, ok := calls[ev.Item.ID]
				if !ok {
					c = &pendingResponsesCall{index: ev.OutputIndex}
					calls[ev.Item.ID] = c
				}
				c.item = *ev.Item
				if ev.Item.Arguments != "" {
					c.args.Reset()
					c.args.WriteString(ev.Item.Arguments)
					c.final = true
				}
				continue
			}
			if label := hostedToolLabel(*ev.Item); label != "" {
				var took int64
				if t0, ok := started[ev.Item.ID]; ok {
					took = time.Since(t0).Milliseconds()
					delete(started, ev.Item.ID)
				}
				onChunk(Chunk{ToolEvent: &ToolEvent{
					Name:       ev.Item.Type,
					Label:      label,
					Done:       true,
					OK:         ev.Item.Status != "failed" && ev.Item.Status != "incomplete" && ev.Item.Error == "",
					DurationMs: took,
					Summary:    hostedToolSummary(*ev.Item),
				}})
			}

		case "response.completed", "response.incomplete":
			if doneSent {
				continue
			}
			var servedModel, finish string
			tokensIn, tokensOut := 0, 0
			if ev.Response != nil {
				servedModel = ev.Response.Model
				if ev.Response.Usage != nil {
					tokensIn = ev.Response.Usage.InputTokens
					tokensOut = ev.Response.Usage.OutputTokens
				}
				if ev.Response.IncompleteDetails != nil {
					switch ev.Response.IncompleteDetails.Reason {
					case "content_filter":
						onChunk(Chunk{Error: fmt.Errorf("%w: the response was stopped by the content filter", ErrContentFiltered)})
						return nil
					case "max_output_tokens":
						finish = "length"
					}
				}
			}
			toolCalls := assembleResponsesCalls(calls)
			if finish == "" {
				finish = "stop"
				if len(toolCalls) > 0 {
					finish = "tool_calls"
				}
			}
			onChunk(Chunk{Done: true, TokensIn: tokensIn, TokensOut: tokensOut, ToolCalls: toolCalls, FinishReason: finish, ServedModel: servedModel})
			return nil

		case "response.failed":
			if ev.Response != nil && ev.Response.Error != nil {
				onChunk(Chunk{Error: classifyResponsesError(ev.Response.Error.Code, ev.Response.Error.Type, ev.Response.Error.Message)})
			} else {
				onChunk(Chunk{Error: classifyResponsesError("", "", "")})
			}
			return nil

		case "error":
			code, typ, msg := ev.Code, "", ev.Message
			if ev.Error != nil {
				code, typ, msg = ev.Error.Code, ev.Error.Type, ev.Error.Message
			}
			onChunk(Chunk{Error: classifyResponsesError(code, typ, msg)})
			return nil

		default:
			// Progress notifications during long hosted-tool runs
			// (*.in_progress, *.searching, *.interpreting): an empty chunk
			// feeds the chat service's idle watchdog without adding text.
			if strings.HasSuffix(ev.Type, ".in_progress") || strings.HasSuffix(ev.Type, ".searching") ||
				strings.HasSuffix(ev.Type, ".interpreting") || strings.HasSuffix(ev.Type, ".generating") {
				onChunk(Chunk{})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading azure responses SSE stream: %w", err)
	}
	if !doneSent {
		if parseErrors > 0 {
			return errStreamMalformed("azure responses", parseErrors)
		}
		return errStreamTruncated("azure responses")
	}
	return nil
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

// hostedToolSummary is the trail's one-line result for a finished hosted
// step: an MCP tool's output (or error) when present, else nothing.
func hostedToolSummary(item responsesOutputItem) string {
	s := item.Error
	if s == "" {
		s = item.Output
	}
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
