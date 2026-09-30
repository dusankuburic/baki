package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
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
// Between tool-loop iterations the run's own output items carry over
// (stateless replay): every request asks for include:
// ["reasoning.encrypted_content"], the run's ordered output items — reasoning
// with its encrypted content, the hosted tool items the Responses input
// accepts (web search, file search, code interpreter, image generation, MCP,
// computer use, tool search) and the function calls with their item ids —
// ride on the Done chunk / Response as ProviderItems, and the chat service's
// tool loop hands them back on the assistant turn. responsesInput replays
// them verbatim instead of rebuilding the turn from its text and tool
// calls, so a reasoning agent keeps its chain of thought across a tool step
// and a hosted step is not repeated. Foundry's own tool items (Bing grounding,
// OpenAPI, A2A, …) are not Responses input items and are dropped from the
// replay; an agent may repeat those steps (accepted risk R15). A service that
// rejects the replay (or the include) gets the legacy rebuild instead, once
// per agent and remembered (see postResponses).

type responsesRequest struct {
	// AgentReference names the agent. The service rejects the older `agent`
	// key ("The 'agent' property is deprecated. Use 'agent_reference'").
	AgentReference *agentRef `json:"agent_reference,omitempty"`
	Model          string    `json:"model,omitempty"`
	Instructions   string    `json:"instructions,omitempty"`
	// Input holds responsesItem values (the rebuilt history) and, for a
	// replayed assistant turn, its provider items as json.RawMessage.
	Input []any `json:"input"`
	// Include asks for reasoning.encrypted_content, which makes a reasoning
	// item replayable under store:false. A rejecting endpoint has it dropped
	// by the parameter repair.
	Include         []string `json:"include,omitempty"`
	Tools           []any    `json:"tools,omitempty"`
	Stream          bool     `json:"stream,omitempty"`
	Store           *bool    `json:"store,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	// Reasoning, on an ephemeral-MODEL run (F-18/F-19), carries the
	// user-chosen effort ("low"|"medium"|"high"). Never set on an
	// agent_reference run: the agent definition owns its sampling.
	Reasoning *responsesReasoning `json:"reasoning,omitempty"`
	// No temperature: the agent definition (portal / CI) owns its sampling
	// settings, and baki's provider-level temperature must not override them
	// — it also cost reasoning-backed agents a rejection round trip per turn.

	// legacyInput is Input rebuilt without any replayed provider items (nil
	// when nothing was replayed): postResponses' fallback when the service
	// rejects a replayed item.
	legacyInput []any
	// workflow marks a workflow agent: the request carries the preview
	// opt-in header (see agentRequest).
	workflow bool
}

// responsesIncludeEncryptedReasoning is the include value that returns a
// reasoning item's encrypted_content.
const responsesIncludeEncryptedReasoning = "reasoning.encrypted_content"

// responsesReplayParam is the learned-repairs marker (not a wire parameter)
// for an agent whose endpoint refused the provider-item replay or the
// include: its requests go out as the legacy rebuild without include.
const responsesReplayParam = "input.provider_items"

type agentRef struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// responsesReasoning is the Responses request's reasoning object (the
// ephemeral-model path's reasoning_effort, F-18/F-19).
type responsesReasoning struct {
	Effort string `json:"effort,omitempty"`
}

// responsesFunctionTool is the Responses API's function tool: a FLAT shape
// ({"type":"function","name","description","parameters"}), unlike chat
// completions' nested {type,function} pair. The agent path never sends
// request-level tools (the agent definition declares them); the
// ephemeral-model path (F-18) does.
type responsesFunctionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// responsesFunctionTools maps the neutral tool definitions onto that shape.
func responsesFunctionTools(tools []ToolDefinition) []any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, responsesFunctionTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	return out
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

// toResponsesInput maps the neutral history onto Responses input items (the
// legacy rebuild: no provider items). The system prompt (flow context,
// guidelines, custom instructions) rides as the first message with the
// system role; the agent keeps its own instructions.
func toResponsesInput(systemPrompt string, msgs []Message) []responsesItem {
	var out []responsesItem
	if systemPrompt != "" {
		out = append(out, responsesItem{Type: "message", Role: "system", Content: systemPrompt})
	}
	for _, m := range msgs {
		out = append(out, legacyResponsesItems(m)...)
	}
	return out
}

// legacyResponsesItems rebuilds one message as input items.
func legacyResponsesItems(m Message) []responsesItem {
	switch m.Role {
	case "tool":
		return []responsesItem{{Type: "function_call_output", CallID: m.ToolCallID, Output: nonEmptyToolOutput(m.Content)}}
	case "assistant":
		var out []responsesItem
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
		return out
	}
	role := m.Role
	if role == "" {
		role = "user"
	}
	return []responsesItem{{Type: "message", Role: role, Content: m.Content}}
}

// responsesInput builds a request's input: the legacy rebuild, except that
// an assistant turn carrying this provider's ProviderItems is replaced by
// those items, verbatim and in order (replayTurn). legacy is the full legacy
// rebuild when anything was replayed — postResponses' fallback — and nil
// otherwise.
func responsesInput(systemPrompt string, msgs []Message) (input, legacy []any) {
	replayed := false
	if systemPrompt != "" {
		input = append(input, responsesItem{Type: "message", Role: "system", Content: systemPrompt})
	}
	for _, m := range msgs {
		if items, ok := replayTurn(m); ok {
			input = append(input, items...)
			replayed = true
			continue
		}
		for _, it := range legacyResponsesItems(m) {
			input = append(input, it)
		}
	}
	if !replayed {
		return input, nil
	}
	for _, it := range toResponsesInput(systemPrompt, msgs) {
		legacy = append(legacy, it)
	}
	return input, legacy
}

// replayTurn returns an assistant turn's provider items for replay, when
// this provider wrote them and they still describe the turn: every tool call
// the loop answers must have its function_call item (a function_call_output
// without its call is a 400), and a turn with text must have its message
// item. Anything else falls back to the legacy rebuild of the turn.
func replayTurn(m Message) ([]any, bool) {
	if m.Role != "assistant" || m.ProviderItemsFrom != azureFoundryProviderID || len(m.ProviderItems) == 0 {
		return nil, false
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(m.ProviderItems, &raw); err != nil {
		logger.Debug("azure ai foundry: undecodable provider items; rebuilding the turn", "error", err)
		return nil, false
	}
	items := replayableItems(raw)
	if len(items) == 0 {
		return nil, false
	}
	calls := map[string]bool{}
	hasMessage := false
	for _, it := range items {
		var head struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		_ = json.Unmarshal(it, &head)
		switch head.Type {
		case "function_call":
			calls[head.CallID] = true
		case "message":
			hasMessage = true
		}
	}
	for _, tc := range m.ToolCalls {
		if !calls[tc.ID] {
			logger.Debug("azure ai foundry: provider items miss a tool call of the turn; rebuilding the turn", "call", tc.ID)
			return nil, false
		}
	}
	if strings.TrimSpace(m.Content) != "" && !hasMessage {
		return nil, false
	}
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it
	}
	return out, true
}

// replayableItemTypes are the output item types recorded for replay: members
// of the Responses `input` union that a finished agent run can produce and
// that need nothing from the app — the answer, reasoning, the function calls
// the tool loop answers, and the hosted tools whose items the service
// accepts back. Foundry's own tool items (bing_grounding_call, openapi_call,
// a2a_preview_call, azure_ai_search_call, …, their *_output items,
// workflow_action, structured_outputs) are not input items and are dropped;
// client-executed items end the run with an error before any replay.
var replayableItemTypes = map[string]bool{
	"message":               true,
	"reasoning":             true,
	"function_call":         true,
	"web_search_call":       true,
	"file_search_call":      true,
	"code_interpreter_call": true,
	"image_generation_call": true,
	"mcp_list_tools":        true,
	"mcp_call":              true,
	"computer_call":         true,
	"computer_call_output":  true,
	"tool_search_call":      true,
	"tool_search_output":    true,
}

// replayImageResultMax bounds the image_generation_call `result` (a base64
// image, several MB) kept for replay; a larger one is replaced by null, which
// the input schema allows: re-uploading it on every tool-loop iteration cost
// MBs per request and billed the image as input again.
const replayImageResultMax = 64 << 10

// replayableItems filters a run's ordered output items down to what the
// Responses input accepts back, in order: allowed types only
// (replayableItemTypes), minus the Foundry-only annotations of an output item
// (agent_reference, response_id), a reasoning item without encrypted_content
// (under store:false the service cannot resolve it by id) and an oversized
// image result. A reasoning item belongs to the item right after it — the
// service rejects one "provided without its required following item" — so
// a reasoning item directly before a dropped item is dropped too.
// Idempotent: the replay path runs it again on recorded items.
func replayableItems(raw []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	var outTypes []string
	for _, r := range raw {
		item, typ, ok := replayableItem(r)
		if !ok {
			for len(outTypes) > 0 && outTypes[len(outTypes)-1] == "reasoning" {
				out, outTypes = out[:len(out)-1], outTypes[:len(outTypes)-1]
			}
			continue
		}
		out = append(out, item)
		outTypes = append(outTypes, typ)
	}
	return out
}

// replayableItem normalises one output item for replay (see
// replayableItems); ok is false for an item that must not be replayed.
func replayableItem(raw json.RawMessage) (json.RawMessage, string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, "", false
	}
	var typ string
	_ = json.Unmarshal(fields["type"], &typ)
	if !replayableItemTypes[typ] {
		return nil, typ, false
	}
	delete(fields, "agent_reference")
	delete(fields, "response_id")
	switch typ {
	case "reasoning":
		var enc string
		_ = json.Unmarshal(fields["encrypted_content"], &enc)
		if enc == "" || rawText(fields["id"]) == "" {
			return nil, typ, false
		}
		if _, ok := fields["summary"]; !ok {
			fields["summary"] = json.RawMessage(`[]`) // required on input
		}
	case "message":
		if _, ok := fields["status"]; !ok {
			fields["status"] = json.RawMessage(`"completed"`) // required on input
		}
	case "image_generation_call":
		if len(fields["result"]) > replayImageResultMax {
			fields["result"] = json.RawMessage(`null`)
		}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, typ, false
	}
	return out, typ, true
}

// providerItemsJSON encodes recorded items as the ProviderItems array; nil
// when there is nothing to replay.
func providerItemsJSON(items []json.RawMessage) json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	out, err := json.Marshal(items)
	if err != nil {
		return nil
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
//
// A workflow agent (kind remembered from the listing, see agentKindFor) is a
// preview: the request carries its Foundry-Features opt-in header. Under
// this stateless replay every turn starts the workflow from the top with the
// whole conversation as input: a human-in-the-loop node (a question, an
// approval) does not resume where the previous turn paused — it runs again
// and reads the user's latest message as its answer, if the workflow is
// written to.
func (p *AzureFoundryProvider) agentRequest(req Request, stream bool) responsesRequest {
	noStore := false
	name := agentNameOf(req.Model)
	input, legacy := responsesInput(req.SystemPrompt, req.Messages)
	body := responsesRequest{
		AgentReference: &agentRef{Type: "agent_reference", Name: name},
		Input:          input,
		Include:        []string{responsesIncludeEncryptedReasoning},
		Stream:         stream,
		// Flow content must not be retained server-side beyond the request;
		// an endpoint that rejects the parameter fails the turn (see
		// postResponses) instead of being retried without it, which the
		// Responses API would default to storing.
		Store:       &noStore,
		legacyInput: legacy,
		workflow:    agentKindFor(p.cfg.ProjectEndpoint, name) == agentKindWorkflow,
	}
	if req.MaxTokens > 0 {
		body.MaxOutputTokens = max(req.MaxTokens, minResponsesOutputTokens)
	}
	return body
}

// modelRequest builds the ephemeral-MODEL Responses request (F-18): the
// deployment itself answers on {endpoint}/openai/v1/responses — no
// agent_reference — so a model that rejects chat-completions requests
// combining function tools with reasoning (gpt-5.6) keeps its reasoning
// across tool calls. baki's flow tools ride as request-level function tools
// (the FLAT Responses shape); the stateless replay, store:false and the
// encrypted-reasoning include are the agent path's. No `reasoning` object
// unless the user chose an effort.
func (p *AzureFoundryProvider) modelRequest(req Request, stream bool) responsesRequest {
	noStore := false
	input, legacy := responsesInput(req.SystemPrompt, req.Messages)
	body := responsesRequest{
		Model:       req.Model,
		Input:       input,
		Include:     []string{responsesIncludeEncryptedReasoning},
		Tools:       responsesFunctionTools(req.Tools),
		Stream:      stream,
		Store:       &noStore,
		legacyInput: legacy,
	}
	if req.MaxTokens > 0 {
		body.MaxOutputTokens = max(req.MaxTokens, minResponsesOutputTokens)
	}
	if req.ReasoningEffort != "" {
		body.Reasoning = &responsesReasoning{Effort: req.ReasoningEffort}
	}
	return body
}

func (p *AzureFoundryProvider) agentConfigError() error {
	if p.cfg.ProjectEndpoint == "" || (p.cfg.Auth.Mode != AzureAuthModeEntra && p.cfg.Auth.Mode != AzureAuthModeEntraCLI) {
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
	// The endpoint and the memo key follow the turn kind: an agent turn
	// posts to the PROJECT's Responses route keyed by agent name; an
	// ephemeral-model turn (F-18) posts to the RESOURCE's route keyed by
	// deployment (responsesRouteKey).
	name := body.Model
	endpoint := p.cfg.Endpoint + "/openai/v1/responses"
	if body.AgentReference != nil {
		name = body.AgentReference.Name
		endpoint = p.cfg.ProjectEndpoint + "/openai/v1/responses"
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
	// repaired guards the loop (each parameter at most once) and is what a
	// 200 with a FRESH repair memoises; learned marks the entries pre-applied
	// from the memo (see the 200 branch below for why a mere re-application
	// does not refresh it). Repairs learned from an earlier turn with the
	// same agent go out up front: each used to cost a rejection round trip —
	// a full re-upload of the history — on every turn and every tool
	// iteration. The key (project endpoint + agent name) holds no secret;
	// `store` is never learned (its rejection fails the turn) and never
	// pre-applied.
	repaired := map[string]bool{}
	learned := map[string]bool{}
	memoKey := endpoint + "\x00" + name
	// legacyReplay swaps the payload to the legacy rebuild without include
	// (responsesReplayParam), reporting whether anything changed.
	legacyReplay := func() bool {
		changed := false
		if body.legacyInput != nil {
			payload["input"] = body.legacyInput
			changed = true
		}
		if _, ok := payload["include"]; ok {
			delete(payload, "include")
			changed = true
		}
		return changed
	}
	if params := repairsFor(memoKey); len(params) > 0 && ensurePayload() {
		changed := false
		for _, param := range params {
			if param == responsesReplayParam {
				// Kept learned even when this request replays nothing, so
				// the memo survives turns without provider items.
				changed = legacyReplay() || changed
				repaired[param], learned[param] = true, true
				continue
			}
			if param == "store" || !repairParam(payload, param) {
				continue
			}
			repaired[param], learned[param] = true, true
			changed = true
		}
		if changed {
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
		if body.workflow {
			httpReq.Header.Set(foundryFeaturesHeader, workflowFeatureValue)
		}
		if err := p.authorize(ctx, httpReq); err != nil {
			return nil, nil, err
		}
		resp, err := p.client.Do(httpReq)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == http.StatusOK {
			// Only a repair learned this round (re)starts the memo's TTL: a
			// turn that merely re-applied the memo must not keep a replay
			// rejection — usually transient (a re-pointed deployment, a
			// one-off verification failure) and never re-probed by the forget
			// path, which needs a param — alive for as long as the agent is
			// in use. learned ⊆ repaired, so the length test is "anything
			// fresh". rememberRepairs overwrites, so the full set is passed.
			if len(repaired) > len(learned) && !memoStale {
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
		if len(repaired) >= maxResponsesRepairs {
			return resp, errBody, nil
		}
		// A rejection of the replayed provider items or of the include
		// (unless it is a plain parameter rejection the repair below
		// handles — include itself is repairable): once, the legacy rebuild
		// without include, remembered for this agent on success.
		if (!paramOK || !repairableParams[param]) && !repaired[responsesReplayParam] &&
			(body.legacyInput != nil || len(body.Include) > 0) &&
			replayRejected(resp.StatusCode, errBody) && ensurePayload() && legacyReplay() {
			repaired[responsesReplayParam] = true
			if err := remarshal(); err != nil {
				return nil, nil, err
			}
			logger.Debug("azure ai foundry: responses replay rejected — retrying with the legacy history", "target", name)
			continue
		}
		if !paramOK {
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

// replayRejection matches a 400's prose blaming a replayed item or the
// include when the error names no parameter: "Item with id 'rs_…' not
// found. Items are not persisted when `store` is set to false.", "Item
// 'rs_…' of type 'reasoning' was provided without its required following
// item.", "The encrypted content for item rs_… could not be verified.",
// "Invalid value: 'bing_grounding_call'" on an input item, an unsupported
// include value.
var replayRejection = regexp.MustCompile(`(?i)encrypted[ _]content|item with id|required following item|of type '[a-z_]+'|input\[\d+\]|'include'|include\[\d*\]|reasoning\.encrypted`)

// replayRejected reports whether a Responses rejection blames the replayed
// input items or the include (param input / input[n]… / include, or the
// prose above) rather than the request as a whole. A context-length or
// content-filter rejection also names the input but is not about the
// replay: retrying without the items would only fail again.
func replayRejected(status int, errBody []byte) bool {
	if !parameterRejectionStatus(status) {
		return false
	}
	apiErr, ok := decodeOpenAIError(errBody)
	if !ok {
		return false
	}
	msg := apiErr.Error.Message
	switch apiErr.Error.Code {
	case "context_length_exceeded", "content_filter":
		return false
	}
	if detectContextLimitError(http.StatusBadRequest, msg) != nil {
		return false
	}
	param := strings.ToLower(strings.TrimSpace(apiErr.Error.Param))
	if param == "input" || param == "include" || strings.HasPrefix(param, "input[") ||
		strings.HasPrefix(param, "input.") || strings.HasPrefix(param, "include[") {
		return true
	}
	return replayRejection.MatchString(msg)
}

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
	return p.responsesChat(ctx, resp, errBody)
}

// modelChat is the non-streaming ephemeral-model turn (F-18). When the
// resource's /openai/v1/responses route does not serve the model (404 /
// "Model not supported"), the turn falls back to chat completions — whose
// built-in repair answers a tools request with reasoning_effort "none" —
// and the refusal is memoised so later turns skip the round trip.
func (p *AzureFoundryProvider) modelChat(ctx context.Context, req Request) (*Response, error) {
	resp, errBody, err := p.postResponses(ctx, p.modelRequest(req, false))
	if err != nil {
		return nil, fmt.Errorf("azure ai foundry request: %w", err)
	}
	if responsesRouteRefused(resp, errBody) {
		resp.Body.Close()
		p.rememberResponsesRoute(req.Model, false)
		logger.Debug("azure ai foundry: the responses route refused the model; answering on chat completions", "deployment", req.Model)
		return p.chat(ctx, req)
	}
	return p.responsesChat(ctx, resp, errBody)
}

// responsesChat reads one non-streaming Responses reply (agent or model).
func (p *AzureFoundryProvider) responsesChat(ctx context.Context, resp *http.Response, errBody []byte) (*Response, error) {
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
	// response.model is the agent's deployment: resolve the model behind it
	// before it reaches pricing and the served-model memo.
	servedModel := p.deploymentModelResolver()(parsed.Model)
	tokensIn, tokensOut, cacheRead := 0, 0, 0
	if parsed.Usage != nil {
		tokensIn, tokensOut = parsed.Usage.InputTokens, parsed.Usage.OutputTokens
		if parsed.Usage.InputTokensDetails != nil {
			cacheRead = parsed.Usage.InputTokensDetails.CachedTokens
		}
	}
	if err := parsed.terminalError(); err != nil {
		// The usage rides on the error (as the stream path's error chunk
		// carries it), so a failed or filtered run can still be billed.
		return nil, &heldUsageError{err: err, tokensIn: tokensIn, tokensOut: tokensOut, servedModel: servedModel}
	}
	out := &Response{ServedModel: servedModel, FinishReason: "stop", TokensIn: tokensIn, TokensOut: tokensOut, CacheReadTokens: cacheRead}
	var text strings.Builder
	var blockers runBlockers
	var replay []json.RawMessage
	for _, raw := range parsed.Output {
		// Items are decoded one by one: a hosted tool item of an unexpected
		// shape must not fail the whole turn.
		item, ok := decodeOutputItem(raw)
		if !ok {
			logger.Debug("azure ai foundry: skipping undecodable agent output item")
			continue
		}
		replay = append(replay, raw)
		if blockers.observe(item) {
			continue
		}
		switch item.Type {
		case "message":
			text.WriteString(item.messageText())
		case "function_call":
			out.ToolCalls = append(out.ToolCalls, item.toolCall())
		}
	}
	if err := blockers.err(); err != nil {
		return nil, &heldUsageError{err: err, tokensIn: tokensIn, tokensOut: tokensOut, servedModel: servedModel}
	}
	out.ProviderItems = providerItemsJSON(replayableItems(replay))
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
	return p.responsesStream(ctx, resp, errBody, onChunk)
}

// modelStream is the streaming ephemeral-model turn (F-18): a deployment
// whose chat-completions requests are rejected for combining function tools
// with reasoning answers here with its reasoning intact. A route that does
// not serve the model (404 / "Model not supported") falls back to chat
// completions — the built-in repair answers with reasoning_effort "none"
// and the reasoning_off notice says so — and the refusal is memoised.
func (p *AzureFoundryProvider) modelStream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	resp, errBody, err := p.postResponses(ctx, p.modelRequest(req, true))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		p.rememberResponsesRoute(req.Model, true)
	}
	if responsesRouteRefused(resp, errBody) {
		resp.Body.Close()
		p.rememberResponsesRoute(req.Model, false)
		logger.Debug("azure ai foundry: the responses route refused the model; answering on chat completions", "deployment", req.Model)
		return p.streamInner(ctx, req, onChunk, false)
	}
	return p.responsesStream(ctx, resp, errBody, onChunk)
}

// responsesStream reads one streaming Responses reply (agent or model).
func (p *AzureFoundryProvider) responsesStream(ctx context.Context, resp *http.Response, errBody []byte, onChunk func(Chunk)) error {
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return p.handleStreamError(resp, errBody)
	}
	// The parser returns at response.completed, before the stream's chunked
	// terminator: drain the rest so the HTTP/1.1 connection goes back to the
	// pool instead of being discarded (a new TCP + TLS handshake per turn).
	defer drainAndClose(resp.Body)
	// response.model is the deployment the run answered on: every chunk's
	// served model is resolved to the model behind it
	// (deploymentModelResolver).
	resolve := p.deploymentModelResolver()
	return parseResponsesSSE(resp.Body, func(c Chunk) {
		if c.ServedModel != "" {
			c.ServedModel = resolve(c.ServedModel)
		}
		onChunk(c)
	})
}

// --- the F-18 reroute memo ---

// responsesToolRoutes memoises, per resource endpoint and deployment, the
// outcome of rerouting a tools-with-reasoning chat-completions rejection
// through {resource}/openai/v1/responses (F-18): use it (a 200), or never
// again (404 / "Model not supported" — an api-version without the v1
// Responses route). Deliberately separate from learnedRepairs: it is a route
// choice, not a repaired parameter, and must not be swept by their forget
// path. Keyed like postResponses' memo (endpoint + deployment, no secret);
// same bounds and TTL.
var responsesToolRoutes = struct {
	sync.Mutex
	m map[string]responsesRoute
}{m: map[string]responsesRoute{}}

type responsesRoute struct {
	use  bool
	seen time.Time
}

// responsesRouteFor returns the remembered route decision for key.
func responsesRouteFor(key string) (use, known bool) {
	responsesToolRoutes.Lock()
	defer responsesToolRoutes.Unlock()
	r, ok := responsesToolRoutes.m[key]
	if !ok {
		return false, false
	}
	if time.Since(r.seen) > learnedRepairsTTL {
		delete(responsesToolRoutes.m, key)
		return false, false
	}
	return r.use, true
}

// rememberResponsesRoute records the decision for key.
func rememberResponsesRoute(key string, use bool) {
	responsesToolRoutes.Lock()
	defer responsesToolRoutes.Unlock()
	if len(responsesToolRoutes.m) >= learnedRepairsMax {
		responsesToolRoutes.m = map[string]responsesRoute{}
	}
	responsesToolRoutes.m[key] = responsesRoute{use: use, seen: time.Now()}
}

// resetResponsesToolRoutes clears the memo (tests,
// ResetAzureFoundryCaches).
func resetResponsesToolRoutes() {
	responsesToolRoutes.Lock()
	responsesToolRoutes.m = map[string]responsesRoute{}
	responsesToolRoutes.Unlock()
}

// responsesRouteKey is this resource's Responses route + deployment — the
// same key postResponses builds for an ephemeral-model turn.
func (p *AzureFoundryProvider) responsesRouteKey(model string) string {
	return p.cfg.Endpoint + "/openai/v1/responses\x00" + model
}

func (p *AzureFoundryProvider) rememberResponsesRoute(model string, use bool) {
	rememberResponsesRoute(p.responsesRouteKey(model), use)
}

// responsesRouteRefused reports whether a {endpoint}/openai/v1/responses
// answer means the route does not serve the model: 404, or a 400 whose error
// says the model is not supported (an api-version without the v1 Responses
// route).
func responsesRouteRefused(resp *http.Response, errBody []byte) bool {
	if resp == nil || resp.StatusCode == http.StatusOK {
		return false
	}
	if resp.StatusCode == http.StatusNotFound {
		return true
	}
	if resp.StatusCode != http.StatusBadRequest {
		return false
	}
	apiErr, ok := decodeOpenAIError(errBody)
	if !ok {
		return false
	}
	lower := strings.ToLower(apiErr.Error.Message + " " + apiErr.Error.Code)
	return strings.Contains(lower, "model not supported") ||
		strings.Contains(lower, "not supported for this api version")
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
		// InputTokensDetails.CachedTokens is the prompt-cache read slice —
		// a breakdown INCLUDED in InputTokens (F-05), surfaced as
		// CacheReadTokens like every route's (Azure AI Foundry is unpriced,
		// so nothing is billed from it).
		InputTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
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

// responsesToolAction is a hosted tool item's `action` object (today only
// web_search_call's {"type":"search","query":…} / {"type":"visit","url":…}).
type responsesToolAction struct {
	Type  string `json:"type"`
	Query string `json:"query"`
	URL   string `json:"url"`
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
	// Outputs is code_interpreter_call's logs / image list; Result is
	// image_generation_call's base64 image or program_output's result.
	Outputs json.RawMessage `json:"outputs"`
	Result  json.RawMessage `json:"result"`
	// Kind is workflow_action's CSDL action kind ("InvokeAzureAgent").
	Kind string `json:"kind"`
	// ConsentLink is oauth_consent_request's consent URL.
	ConsentLink string `json:"consent_link"`
	// Execution is tool_search_call's "server" | "client".
	Execution string `json:"execution"`
	// Action is a web_search_call's action ({"type":"search","query":"…"})
	// and Queries a file_search_call's search phrases (F-20): what the agent
	// searched for, shown as the trail step's summary.
	Action  responsesToolAction `json:"action"`
	Queries []string            `json:"queries"`
	Content []struct {
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
		Kind        string `json:"kind"`
		ConsentLink string `json:"consent_link"`
		Execution   string `json:"execution"`
	}
	if err := json.Unmarshal(raw, &head); err != nil || head.Type == "" {
		return responsesOutputItem{}, false
	}
	return responsesOutputItem{ID: head.ID, Type: head.Type, Status: head.Status, CallID: head.CallID, Name: head.Name,
		ServerLabel: head.ServerLabel, Kind: head.Kind, ConsentLink: head.ConsentLink, Execution: head.Execution}, true
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

// clientToolError explains a run that stopped for a tool the CLIENT must
// execute — a custom tool, shell, local shell, apply_patch, computer use
// without a hosted executor, a client-side tool search. The app runs only its
// own flow tools (function calls), so the turn ends with this message instead
// of an empty answer, like mcpApprovalError.
func clientToolError(tools map[string]bool) error {
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("the Foundry agent asked this app to run %s on its side, and this app runs only its own flow tools: remove that tool from the agent definition (or use its hosted variant), then try again", strings.Join(names, ", "))
}

// oauthConsentError explains a run that stopped because an MCP server needs
// the user's OAuth consent (an oauth_consent_request item). The link is the
// service's own consent page for this user; after consenting, the same
// question works.
func oauthConsentError(consents map[string]string) error {
	labels := make([]string, 0, len(consents))
	for l := range consents {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		if link := consents[l]; link != "" {
			parts = append(parts, fmt.Sprintf("MCP server %q: %s", l, link))
		} else {
			parts = append(parts, fmt.Sprintf("MCP server %q", l))
		}
	}
	return fmt.Errorf("the Foundry agent needs your OAuth consent before it can use %s — open the link, sign in and consent, then try again", strings.Join(parts, "; "))
}

// clientExecutedTypes are the output item types that ask the client to run
// something (tool_search_call counts only with execution "client";
// computer_call only when the run carries no computer_call_output for it).
var clientExecutedTypes = map[string]bool{
	"custom_tool_call": true,
	"shell_call":       true,
	"local_shell_call": true,
	"apply_patch_call": true,
}

// runBlockers collects the output items that end an agent run without an
// answer the app can act on: MCP approval requests, OAuth consent requests
// and client-executed tool calls. Fed every output item (added and done
// events alike — repeats are harmless), it reports the explanation at the
// terminal event.
type runBlockers struct {
	approvals map[string]bool
	consents  map[string]string
	client    map[string]bool
	// computer holds computer_call items by call id until their
	// computer_call_output shows the service executed them.
	computer map[string]string
	executed map[string]bool
}

// observe records item; true when it is a blocker (not a trail step).
func (b *runBlockers) observe(item responsesOutputItem) bool {
	switch {
	case item.Type == "mcp_approval_request":
		if b.approvals == nil {
			b.approvals = map[string]bool{}
		}
		b.approvals[mcpToolName(item)] = true
		return true
	case item.Type == "oauth_consent_request":
		if b.consents == nil {
			b.consents = map[string]string{}
		}
		label := item.ServerLabel
		if label == "" {
			label = "(unnamed)"
		}
		if b.consents[label] == "" {
			b.consents[label] = strings.TrimSpace(item.ConsentLink)
		}
		return true
	case clientExecutedTypes[item.Type],
		item.Type == "tool_search_call" && strings.EqualFold(item.Execution, "client"):
		if b.client == nil {
			b.client = map[string]bool{}
		}
		b.client[clientToolName(item)] = true
		return true
	case item.Type == "computer_call":
		if b.computer == nil {
			b.computer = map[string]string{}
		}
		b.computer[responsesCallKey(item.ID, item.CallID)] = clientToolName(item)
	case item.Type == "computer_call_output":
		if b.executed == nil {
			b.executed = map[string]bool{}
		}
		b.executed[item.CallID] = true
	}
	return false
}

// clientToolName names a client-executed item in the explanation.
func clientToolName(item responsesOutputItem) string {
	if item.Name != "" {
		return item.Type + " (" + item.Name + ")"
	}
	return item.Type
}

// err is the run's explanation, nil when nothing blocks it: an OAuth consent
// first (the user can act on it at once), then MCP approvals, then
// client-executed tools.
func (b *runBlockers) err() error {
	if len(b.consents) > 0 {
		return oauthConsentError(b.consents)
	}
	if len(b.approvals) > 0 {
		return mcpApprovalError(b.approvals)
	}
	client := b.client
	for id, name := range b.computer {
		if b.executed[id] {
			continue
		}
		if client == nil {
			client = map[string]bool{}
		}
		client[name] = true
	}
	if len(client) > 0 {
		return clientToolError(client)
	}
	return nil
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

// responsesCallKey is the one key an item is known by where a single key is
// wanted, preferring call_id — what the emitted ToolCall.ID and a
// *_call_output item reference — and falling back to id. Pending function
// calls are indexed under BOTH keys instead (see parseResponsesSSE).
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

// classifyResponsesError maps a failed run's error, or a stream `error`
// event's, onto the shared sentinels. The stream's error types stand for
// HTTP statuses (server_error 500, too_many_requests 429, forbidden 403,
// user_error 400): forbidden is a permission denial — the credential was
// accepted, so no retry, no breaker count and no fresh-token replay —
// and user_error a rejected request, still read by the context-length and
// tools-unsupported detection.
func classifyResponsesError(code, typ, msg string) error {
	switch {
	case code == "rate_limit_exceeded" || typ == "too_many_requests" || typ == "rate_limit_error":
		return fmt.Errorf("%w: %s", ErrRateLimited, msg)
	case typ == "server_error" || code == "server_error" || code == "no_capacity":
		return fmt.Errorf("%w: %s", ErrProviderDown, msg)
	case code == "content_filter":
		return fmt.Errorf("%w: %s", ErrContentFiltered, msg)
	case typ == "forbidden":
		m := oneLine(strings.Join(strings.Fields(msg), " "), azureAuthMessageMax)
		if m == "" {
			m = "the Foundry agent run was refused (forbidden)"
		}
		return &permissionDeniedError{msg: m}
	case typ == "user_error":
		if err := detectContextLimitError(http.StatusBadRequest, msg); err != nil {
			return err
		}
		if err := detectToolsUnsupportedError(http.StatusBadRequest, msg); err != nil {
			return err
		}
		if msg == "" {
			return errors.New("azure ai foundry agent request rejected (bad request)")
		}
		return fmt.Errorf("azure ai foundry agent request rejected (bad request): %s", msg)
	case code == "" && msg == "":
		return errors.New("azure ai foundry agent run failed")
	}
	return fmt.Errorf("azure ai foundry agent error: %s", msg)
}

// hostedToolLabels names the server-side steps a Foundry agent run can
// contain, as they appear in the tool trail: the OpenAI hosted tools plus
// Foundry's own tool items and run steps (v1 spec). Only the calls are
// listed: a separate *_call_output item (Foundry's tools, computer use,
// programs, tool search) completes its call's trail entry, paired by call_id
// (see parseResponsesSSE). An unlisted server-side item still blocks a
// replay (Chunk.ServerStep).
var hostedToolLabels = map[string]string{
	"web_search_call":                   "Agent · web search",
	"file_search_call":                  "Agent · file search",
	"code_interpreter_call":             "Agent · code interpreter",
	"mcp_call":                          "Agent · MCP tool",
	"mcp_list_tools":                    "Agent · MCP tools",
	"image_generation_call":             "Agent · image generation",
	"computer_call":                     "Agent · computer use",
	"tool_search_call":                  "Agent · tool search",
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
	"memory_command_preview_call":       "Agent · memory update",
	"workflow_action":                   "Agent · workflow step",
	"structured_outputs":                "Agent · structured output",
	"program":                           "Agent · program",
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
	switch {
	case hostedToolNamed[item.Type] && item.Name != "":
		return label + ": " + item.Name
	case item.Type == "workflow_action" && item.Kind != "":
		return label + ": " + item.Kind
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

// hostedToolOK reports whether a finished hosted item succeeded: not failed,
// incomplete or cancelled (a workflow action), no error ("error": null is
// how a successful MCP call reports), and an image generation that carries
// its image.
func hostedToolOK(item responsesOutputItem) bool {
	switch item.Status {
	case "failed", "incomplete", "cancelled":
		return false
	}
	if rawText(item.Error) != "" {
		return false
	}
	if item.Type == "image_generation_call" && !hasImageResult(item) {
		return false
	}
	return true
}

// hasImageResult reports a non-empty image_generation_call result without
// decoding the (multi-MB) base64 string.
func hasImageResult(item responsesOutputItem) bool {
	r := bytes.TrimSpace(item.Result)
	return len(r) > 2 && r[0] == '"'
}

// codeInterpreterLogs is a code_interpreter_call's outputs as text: the
// logs, else a note that it produced an image.
func codeInterpreterLogs(outputs json.RawMessage) string {
	var list []struct {
		Type string `json:"type"`
		Logs string `json:"logs"`
	}
	if json.Unmarshal(outputs, &list) != nil {
		return ""
	}
	image := false
	for _, o := range list {
		if strings.TrimSpace(o.Logs) != "" {
			return o.Logs
		}
		image = image || o.Type == "image"
	}
	if image {
		return "image output"
	}
	return ""
}

// heldStep is a finished hosted call whose result arrives as a separate
// *_call_output item: its trail completion waits for that item.
type heldStep struct {
	item  responsesOutputItem
	label string
	start time.Time // zero when the step's start was not seen
}

// stepDoneChunk is a hosted step's trail completion: result is the item that
// carries the outcome (the call itself, or its paired *_call_output).
func stepDoneChunk(call responsesOutputItem, label string, start time.Time, result responsesOutputItem) Chunk {
	var took int64
	if !start.IsZero() {
		took = time.Since(start).Milliseconds()
	}
	summary := hostedToolSummary(result)
	if summary == "" && result.Type != call.Type {
		summary = hostedToolSummary(call)
	}
	return Chunk{ToolEvent: &ToolEvent{
		Name:       call.Type,
		Label:      label,
		Done:       true,
		OK:         hostedToolOK(call) && hostedToolOK(result),
		DurationMs: took,
		Summary:    summary,
	}}
}

// hasOwnResult reports whether a hosted item carries its outcome itself
// (output, error, code interpreter outputs, an image or program result) —
// otherwise, with a call_id, the outcome comes as a *_call_output item.
func hasOwnResult(item responsesOutputItem) bool {
	result := bytes.TrimSpace(item.Result)
	return rawText(item.Output) != "" || rawText(item.Error) != "" || rawText(item.Outputs) != "" ||
		(len(result) > 0 && !bytes.Equal(result, []byte("null")))
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
// marker for a server-side item without a trail label), function calls,
// usage and the run's replayable output items (ProviderItems) on the Done
// chunk, failures and runs the app cannot continue (MCP approval, OAuth
// consent, client-executed tools) as error chunks.
func parseResponsesSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxResponsesSSELine)

	var (
		parseErrors int
		// calls holds each pending function call under its item id AND its
		// call_id (one pointer, two keys): argument delta/done events name
		// the item id, while an added event may carry only the call_id and
		// the done event both — any key finds the one call.
		calls = map[string]*pendingResponsesCall{}
		// started pairs a hosted step's start and end by output_index:
		// Foundry's own tool items carry a call_id and no id.
		started = map[int]time.Time{}
		// held keeps, by call_id, the finished hosted calls whose outcome
		// comes as a separate *_call_output item (heldOrder: arrival order).
		held      = map[string]*heldStep{}
		heldOrder []string
		// blockers collects what ends the run without a usable answer (see
		// runBlockers), reported at the terminal event.
		blockers runBlockers
		// outputs records every finished output item by output_index, for
		// the replay (ProviderItems on the Done chunk).
		outputs = map[int]json.RawMessage{}
		// streamedText counts the text streamed per message item id (and
		// under "" for deltas without one); totalStreamed across all. They
		// decide whether a message's done event still carries unseen text.
		streamedText  = map[string]int{}
		totalStreamed int
		// servedModel is the response's model, seeded from EVERY event that
		// carries the response object (response.created / in_progress come
		// before any output), and carried on text and error chunks: a run cut
		// before response.completed (Stop, the idle watchdog, a truncated
		// stream) still records the model that answered on its usage row and
		// teaches the served-model memo.
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
	findCall := func(id, callID string) *pendingResponsesCall {
		for _, k := range []string{id, callID} {
			if c, ok := calls[k]; ok && k != "" {
				return c
			}
		}
		return nil
	}
	indexCall := func(c *pendingResponsesCall, id, callID string) {
		for _, k := range []string{id, callID} {
			if k != "" {
				calls[k] = c
			}
		}
	}
	// flushHeld completes the held steps whose output never came (a terminal
	// event or the end of the stream is next), from the call alone.
	flushHeld := func() {
		for _, k := range heldOrder {
			if h, ok := held[k]; ok {
				delete(held, k)
				emit(stepDoneChunk(h.item, h.label, h.start, h.item))
			}
		}
		heldOrder = nil
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
				c := findCall(item.ID, item.CallID)
				if c == nil {
					c = &pendingResponsesCall{index: ev.OutputIndex, item: item}
				}
				indexCall(c, item.ID, item.CallID)
			case item.Type == "reasoning":
				// A reasoning item with its summaries off (the portal default)
				// streams NOTHING between this added and its done; bracket the
				// silent phase with Chunk.SilentStep so the chat service holds
				// the hosted-step idle limit while it runs — awaitingOutput was
				// cleared by any earlier text of the turn.
				emit(Chunk{SilentStep: 1})
			case clientSideItem(item.Type):
			// observe records the item (computer calls and their outputs
			// too, which stay trail steps); true for a blocker.
			case blockers.observe(item):
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
			outputs[ev.OutputIndex] = ev.Item
			switch {
			case item.Type == "function_call":
				c := findCall(item.ID, item.CallID)
				if c == nil {
					c = &pendingResponsesCall{index: ev.OutputIndex}
				}
				indexCall(c, item.ID, item.CallID)
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
			case item.Type == "reasoning":
				emit(Chunk{SilentStep: -1}) // closes the phase output_item.added opened
			case clientSideItem(item.Type):
			case blockers.observe(item):
				emit(Chunk{ServerStep: true})
			default:
				label := hostedToolLabel(item)
				if label == "" {
					// A *_call_output completes its held call's trail entry.
					if h, ok := held[item.CallID]; ok && item.CallID != "" && strings.HasSuffix(item.Type, "_output") {
						delete(held, item.CallID)
						emit(stepDoneChunk(h.item, h.label, h.start, item))
						continue
					}
					emit(Chunk{ServerStep: true}) // see output_item.added
					continue
				}
				var t0 time.Time
				if t, ok := started[ev.OutputIndex]; ok {
					t0 = t
					delete(started, ev.OutputIndex)
				}
				if item.CallID != "" && !hasOwnResult(item) {
					// The outcome comes as its *_call_output item: the trail
					// entry completes then (or at the terminal event).
					if _, dup := held[item.CallID]; !dup {
						heldOrder = append(heldOrder, item.CallID)
					}
					held[item.CallID] = &heldStep{item: item, label: label, start: t0}
					emit(Chunk{ServerStep: true})
					continue
				}
				emit(stepDoneChunk(item, label, t0, item))
			}

		case "response.completed", "response.incomplete":
			flushHeld()
			var finish string
			tokensIn, tokensOut, cacheRead := 0, 0, 0
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					tokensIn = ev.Response.Usage.InputTokens
					tokensOut = ev.Response.Usage.OutputTokens
					if ev.Response.Usage.InputTokensDetails != nil {
						cacheRead = ev.Response.Usage.InputTokensDetails.CachedTokens
					}
				}
				if ev.Response.IncompleteDetails != nil {
					switch ev.Response.IncompleteDetails.Reason {
					case "content_filter":
						// Usage rides on the error chunk (as the blockers'
						// branch below does) so the tokens spent are still billed.
						emit(Chunk{Error: fmt.Errorf("%w: the response was stopped by the content filter", ErrContentFiltered), TokensIn: tokensIn, TokensOut: tokensOut, CacheReadTokens: cacheRead, ServedModel: servedModel})
						return nil
					default:
						// The run was cut short whatever the reason says
						// (max_output_tokens today, a reason invented later):
						// an unmapped reason must not finish as a clean stop.
						finish = "length"
					}
				}
			}
			if err := blockers.err(); err != nil {
				// The run stopped for something the app cannot provide (MCP
				// approval, OAuth consent, a client-executed tool) and cannot
				// continue (not even with function outputs replayed): explain
				// instead of finishing with an empty answer. The usage rides
				// on the error chunk so the tokens spent are still billed.
				emit(Chunk{Error: err, TokensIn: tokensIn, TokensOut: tokensOut, CacheReadTokens: cacheRead, ServedModel: servedModel})
				return nil
			}
			toolCalls := assembleResponsesCalls(calls)
			if finish == "" {
				finish = "stop"
				if len(toolCalls) > 0 {
					finish = "tool_calls"
				}
			}
			emit(Chunk{Done: true, TokensIn: tokensIn, TokensOut: tokensOut, CacheReadTokens: cacheRead, ToolCalls: toolCalls, FinishReason: finish, ServedModel: servedModel,
				ProviderItems: providerItemsJSON(replayableItems(orderedOutputs(outputs)))})
			return nil

		case "response.failed":
			flushHeld()
			tokensIn, tokensOut, cacheRead := 0, 0, 0
			var failure error
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					tokensIn = ev.Response.Usage.InputTokens
					tokensOut = ev.Response.Usage.OutputTokens
					if ev.Response.Usage.InputTokensDetails != nil {
						cacheRead = ev.Response.Usage.InputTokensDetails.CachedTokens
					}
				}
				if ev.Response.Error != nil {
					failure = classifyResponsesError(ev.Response.Error.Code, ev.Response.Error.Type, ev.Response.Error.Message)
				}
			}
			if failure == nil {
				failure = classifyResponsesError("", "", "")
			}
			// Usage rides on the error chunk (as the content-filter and
			// blockers' branches do) so the tokens a failed run spent are
			// still billed by Azure's numbers, not a tokenizer estimate.
			emit(Chunk{Error: failure, TokensIn: tokensIn, TokensOut: tokensOut, CacheReadTokens: cacheRead, ServedModel: servedModel})
			return nil

		case "error":
			flushHeld()
			code, typ, msg := ev.Code, "", ev.Message
			if ev.Error != nil {
				code, typ, msg = ev.Error.Code, ev.Error.Type, ev.Error.Message
			}
			emit(Chunk{Error: classifyResponsesError(code, typ, msg), ServedModel: servedModel})
			return nil
		}

		// An event that produced no chunk — reasoning summary parts and deltas
		// (an o-series / gpt-5 agent can reason for minutes before the first
		// output token; they are keepalives only, while the reasoning item's
		// added/done pair brackets the whole silent phase with Chunk.SilentStep,
		// which the idle watchdog counts as a hosted step), silent function-call
		// accumulation, progress notifications during hosted-tool runs
		// (*.in_progress, *.searching, *.interpreting) — still proves the run is
		// alive, and the chat service's idle watchdog only sees chunks: one
		// empty keepalive per event keeps a long reasoning phase from being
		// cancelled as a stalled stream (the chat-completions parser does the
		// same).
		if !emitted {
			onChunk(Chunk{})
		}
	}

	// Reaching here means the stream ended before a terminal event (every
	// terminal branch returns): the run was cut off, not completed. The
	// held trail steps still complete, so the trail shows no step forever
	// running.
	flushHeld()
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading azure responses SSE stream: %w", err)
	}
	if parseErrors > 0 {
		return errStreamMalformed("azure responses", parseErrors)
	}
	return errStreamTruncated("azure responses")
}

// orderedOutputs lists the recorded output items by output_index.
func orderedOutputs(outputs map[int]json.RawMessage) []json.RawMessage {
	idx := make([]int, 0, len(outputs))
	for i := range outputs {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]json.RawMessage, 0, len(idx))
	for _, i := range idx {
		out = append(out, outputs[i])
	}
	return out
}

// assembleResponsesCalls orders the accumulated function calls by output
// position and converts them to the neutral ToolCall. calls indexes each call
// under up to two keys; each is emitted once.
func assembleResponsesCalls(calls map[string]*pendingResponsesCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	seen := make(map[*pendingResponsesCall]bool, len(calls))
	list := make([]*pendingResponsesCall, 0, len(calls))
	for _, c := range calls {
		if !seen[c] {
			seen[c] = true
			list = append(list, c)
		}
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
// step: the tool's error, else its outcome — `output`, a code interpreter's
// `outputs` logs, a program's `result`, "image generated" for an image
// generation (never its base64 `result`) — else nothing. The text is sent to
// the client and stored with the conversation, so it goes through the secret
// scrubber — before the cut, so a secret straddling it is still matched —
// and is cut on a rune boundary.
func hostedToolSummary(item responsesOutputItem) string {
	s := errorText(item.Error)
	if s == "" {
		switch item.Type {
		case "image_generation_call":
			if hasImageResult(item) {
				s = "image generated"
			}
		default:
			s = rawText(item.Output)
			if s == "" {
				s = codeInterpreterLogs(item.Outputs)
			}
			if s == "" {
				s = rawText(item.Result)
			}
			if s == "" {
				// A finished hosted search step often carries no output of
				// its own: show what it searched for instead of nothing
				// (F-20).
				s = hostedSearchSummary(item)
			}
		}
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

// hostedSearchSummary is the trail fallback for a finished hosted search
// step whose item carries no outcome text: what the agent searched for
// (F-20). A web_search_call's action.query (else its action.url), a
// file_search_call's queries, a Bing grounding call's arguments (Bing's
// terms require the query to be displayed with the results).
func hostedSearchSummary(item responsesOutputItem) string {
	switch item.Type {
	case "web_search_call":
		if item.Action.Query != "" {
			return item.Action.Query
		}
		return item.Action.URL
	case "file_search_call":
		return strings.Join(nonEmptyList(item.Queries), "; ")
	case "bing_grounding_call", "bing_custom_search_preview_call":
		return bingQuery(item.Arguments)
	}
	return ""
}

// nonEmptyList drops blank entries.
func nonEmptyList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// bingQuery is a Bing grounding call's search phrase: its arguments carry
// the request URL — the ?q= value when it parses, else the arguments
// verbatim (compacted by the caller's scrub/cut as any other summary).
func bingQuery(args string) string {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || trimmed == "{}" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var a struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(trimmed), &a) == nil && a.URL != "" {
			if u, err := url.Parse(a.URL); err == nil {
				if q := u.Query().Get("q"); q != "" {
					return q
				}
			}
			return a.URL
		}
	}
	return trimmed
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
