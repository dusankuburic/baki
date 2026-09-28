package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestToResponsesInput(t *testing.T) {
	items := toResponsesInput("SYS", []Message{
		{Role: "user", Content: "find issues"},
		{Role: "assistant", Content: "looking", ToolCalls: []ToolCall{{ID: "call_1", Name: "search_flow", Input: json.RawMessage(`{"q":"x"}`)}, {ID: "call_2", Name: "get_block"}}},
		{Role: "tool", ToolCallID: "call_1", Content: "result one"},
		{Role: "tool", ToolCallID: "call_2", Content: "error: unknown tool get_block"},
		{Role: "assistant", Content: "done"},
		{Role: "tool", ToolCallID: "call_3", Content: ""},
	})
	want := []responsesItem{
		{Type: "message", Role: "system", Content: "SYS"},
		{Type: "message", Role: "user", Content: "find issues"},
		{Type: "message", Role: "assistant", Content: "looking"},
		{Type: "function_call", CallID: "call_1", Name: "search_flow", Arguments: `{"q":"x"}`},
		{Type: "function_call", CallID: "call_2", Name: "get_block", Arguments: "{}"},
		{Type: "function_call_output", CallID: "call_1", Output: "result one"},
		{Type: "function_call_output", CallID: "call_2", Output: "error: unknown tool get_block"},
		{Type: "message", Role: "assistant", Content: "done"},
		// D-35: `output` is required and marshals with omitempty — an empty
		// result goes out as a placeholder, not as an invalid item.
		{Type: "function_call_output", CallID: "call_3", Output: "(no output)"},
	}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(items), len(want), items)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, items[i], want[i])
		}
	}
}

const responsesStreamFixture = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}

event: response.web_search_call.searching
data: {"type":"response.web_search_call.searching","item_id":"ws_1"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"completed"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"Hel"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"lo"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_9","name":"search_flow","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"q\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"x\"}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"q\":\"x\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_9","name":"search_flow","arguments":"{\"q\":\"x\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-4o-2024-11-20","usage":{"input_tokens":42,"output_tokens":7}}}

`

func TestParseResponsesSSE_TextToolsAndHostedSteps(t *testing.T) {
	var chunks []Chunk
	err := parseResponsesSSE(strings.NewReader(responsesStreamFixture), func(c Chunk) { chunks = append(chunks, c) })
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var text strings.Builder
	var events []ToolEvent
	var done *Chunk
	keepalives := 0
	for i := range chunks {
		c := chunks[i]
		switch {
		case c.ToolEvent != nil:
			events = append(events, *c.ToolEvent)
		case c.Done:
			done = &chunks[i]
		case c.Text != "":
			text.WriteString(c.Text)
		case c.Error != nil:
			t.Fatalf("unexpected error chunk: %v", c.Error)
		case c.ServerStep:
			t.Errorf("a labelled hosted step or a function call must not emit a ServerStep marker: %+v", c)
		default:
			keepalives++
		}
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q", text.String())
	}
	if len(events) != 2 || events[0].Done || events[0].Label != "Agent · web search" || !events[1].Done || !events[1].OK {
		t.Errorf("tool events = %+v", events)
	}
	if keepalives != 7 {
		t.Errorf("every silent event (response.created, the function-call accumulation events, the searching notification) must yield one keepalive, got %d", keepalives)
	}
	if done == nil {
		t.Fatal("no Done chunk")
	}
	if done.TokensIn != 42 || done.TokensOut != 7 || done.ServedModel != "gpt-4o-2024-11-20" || done.FinishReason != "tool_calls" {
		t.Errorf("done = %+v", done)
	}
	if len(done.ToolCalls) != 1 || done.ToolCalls[0].ID != "call_9" || done.ToolCalls[0].Name != "search_flow" || string(done.ToolCalls[0].Input) != `{"q":"x"}` {
		t.Errorf("tool calls = %+v", done.ToolCalls)
	}
}

func TestParseResponsesSSE_Terminals(t *testing.T) {
	filtered := "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"content_filter\"}}}\n\n"
	var got []Chunk
	if err := parseResponsesSSE(strings.NewReader(filtered), func(c Chunk) { got = append(got, c) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !errors.Is(got[0].Error, ErrContentFiltered) {
		t.Errorf("content_filter chunks = %+v", got)
	}

	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}}\n\n"
	got = nil
	if err := parseResponsesSSE(strings.NewReader(failed), func(c Chunk) { got = append(got, c) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !errors.Is(got[0].Error, ErrRateLimited) {
		t.Errorf("failed chunks = %+v", got)
	}

	errEvent := "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"boom\"}\n\n"
	got = nil
	_ = parseResponsesSSE(strings.NewReader(errEvent), func(c Chunk) { got = append(got, c) })
	if len(got) != 1 || !errors.Is(got[0].Error, ErrProviderDown) {
		t.Errorf("error event chunks = %+v", got)
	}

	lengthCut := "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"model\":\"gpt-4o\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"
	got = nil
	if err := parseResponsesSSE(strings.NewReader(lengthCut), func(c Chunk) { got = append(got, c) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Done || got[0].FinishReason != "length" {
		t.Errorf("max_output_tokens chunks = %+v", got)
	}

	// A reason the mapper does not know (Foundry can invent new ones) is a
	// truncation too, never a clean stop.
	unknownCut := "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"model\":\"gpt-5\",\"incomplete_details\":{\"reason\":\"reasoning_dial_removed_mid_turn\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"
	got = nil
	if err := parseResponsesSSE(strings.NewReader(unknownCut), func(c Chunk) { got = append(got, c) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Done || got[0].FinishReason != "length" {
		t.Errorf("unknown incomplete reason chunks = %+v", got)
	}

	truncated := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	err := parseResponsesSSE(strings.NewReader(truncated), func(Chunk) {})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("stream without terminal event: %v", err)
	}
}

// TestParseResponsesSSE_ReasoningKeepalives: an o-series / gpt-5 agent can
// reason for minutes before the first output token, and a reasoning stream
// carries no text and no hosted-tool steps — every reasoning event must feed
// the chat service's idle watchdog or the run is cancelled mid-thought.
func TestParseResponsesSSE_ReasoningKeepalives(t *testing.T) {
	body := sseEvents(
		`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","output_index":0,"summary_index":0}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"thinking "}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"hard"}`,
		`{"type":"response.reasoning_summary_text.done","item_id":"rs_1","text":"thinking hard"}`,
		`{"type":"response.completed","response":{"status":"completed","model":"gpt-5","usage":{"input_tokens":10,"output_tokens":2}}}`,
	)
	var chunks []Chunk
	if err := parseResponsesSSE(strings.NewReader(body), func(c Chunk) { chunks = append(chunks, c) }); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 5 {
		t.Fatalf("chunks = %+v", chunks)
	}
	for i := 0; i < 4; i++ {
		c := chunks[i]
		if c.Text != "" || c.Done || c.ServerStep || c.Error != nil || c.ToolEvent != nil {
			t.Errorf("reasoning event %d must yield an empty keepalive chunk, got %+v", i, c)
		}
	}
	if !chunks[4].Done || chunks[4].FinishReason != "stop" || chunks[4].ServedModel != "gpt-5" {
		t.Errorf("done = %+v", chunks[4])
	}
}

// TestParseResponsesSSE_CallIDKeyedFunctionCalls: Foundry function-call items
// can carry a call_id and no id (as its hosted tool items do); two parallel
// calls must accumulate arguments independently instead of colliding on the
// empty id key.
func TestParseResponsesSSE_CallIDKeyedFunctionCalls(t *testing.T) {
	callA := `{"type":"function_call","call_id":"call_a","name":"search_flow","arguments":"","status":"completed"}`
	callB := `{"type":"function_call","call_id":"call_b","name":"get_block","arguments":"","status":"completed"}`
	events := []string{
		itemEvent("response.output_item.added", 0, callA),
		itemEvent("response.output_item.added", 1, callB),
		`{"type":"response.function_call_arguments.delta","item_id":"call_a","delta":"{\"q\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"call_b","delta":"{\"id\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"call_a","delta":"\"f\"}"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"call_b","delta":"\"b1\"}"}`,
		itemEvent("response.output_item.done", 0, callA),
		itemEvent("response.output_item.done", 1, callB),
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}
	s := summarize(parseChunks(t, sseEvents(events...)))
	if s.done == nil {
		t.Fatal("no Done chunk")
	}
	got := s.done.ToolCalls
	if len(got) != 2 {
		t.Fatalf("tool calls = %+v", got)
	}
	if got[0].ID != "call_a" || got[0].Name != "search_flow" || string(got[0].Input) != `{"q":"f"}` {
		t.Errorf("call a = %+v", got[0])
	}
	if got[1].ID != "call_b" || got[1].Name != "get_block" || string(got[1].Input) != `{"id":"b1"}` {
		t.Errorf("call b = %+v", got[1])
	}
}

func newAzureAgentProvider(t *testing.T, srv *azureTestServer) *AzureFoundryProvider {
	t.Helper()
	ResetAzureFoundryCaches()
	cfg := AzureFoundryConfig{Endpoint: srv.URL, ProjectEndpoint: srv.URL + "/api/projects/p1",
		Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}}
	p := newAzureFoundryProvider(cfg, nil, srv.Client())
	p.tokens = newEntraTokenSource(&fakeCred{})
	p.tokenFn = p.entraToken
	return p
}

func TestAzureFoundry_AgentChatAndStream(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if r.URL.Path != "/api/projects/p1/openai/v1/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(responsesStreamFixture))
			return
		}
		writeJSON(w, 200, map[string]any{
			"id": "resp_2", "status": "completed", "model": "gpt-4o-2024-11-20",
			"output": []map[string]any{
				{"type": "web_search_call", "id": "ws", "status": "completed"},
				{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "Answer"}}},
				{"type": "function_call", "id": "fc", "call_id": "call_3", "name": "get_block", "arguments": `{"id":"b1"}`},
			},
			"usage": map[string]any{"input_tokens": 5, "output_tokens": 3},
		})
	})
	p := newAzureAgentProvider(t, srv)
	req := Request{Model: "agent:Helper", SystemPrompt: "SYS", Temperature: 0.3, MaxTokens: 100,
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolDefinition{{Name: "search_flow", InputSchema: []byte(`{}`)}}}

	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Answer" || resp.ServedModel != "gpt-4o-2024-11-20" || resp.TokensIn != 5 || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_3" || resp.FinishReason != "tool_calls" {
		t.Errorf("response = %+v", resp)
	}
	body := srv.requests()[0].Body
	agent, _ := body["agent_reference"].(map[string]any)
	if agent["type"] != "agent_reference" || agent["name"] != "Helper" {
		t.Errorf("agent_reference = %v", body["agent_reference"])
	}
	// The service rejects the deprecated key with a 400 on every turn.
	if _, ok := body["agent"]; ok {
		t.Errorf("deprecated 'agent' key sent: %v", body)
	}
	if body["store"] != false || body["tools"] != nil || body["model"] != nil || body["instructions"] != nil {
		t.Errorf("agent request must carry store:false and no request-level tools/model/instructions: %v", body)
	}
	input, _ := body["input"].([]any)
	first, _ := input[0].(map[string]any)
	if len(input) != 2 || first["role"] != "system" || first["content"] != "SYS" {
		t.Errorf("input items = %v", input)
	}
	if body["max_output_tokens"] != float64(100) {
		t.Errorf("max_output_tokens = %v", body["max_output_tokens"])
	}
	// D-36: the agent definition owns its sampling settings.
	if _, ok := body["temperature"]; ok {
		t.Errorf("agent request must not carry baki's temperature: %v", body)
	}
	if got := srv.requests()[0].Headers.Get("Authorization"); got != "Bearer tok-1" {
		t.Errorf("Authorization = %q", got)
	}

	var text strings.Builder
	var done Chunk
	events := 0
	if err := p.Stream(context.Background(), req, func(c Chunk) {
		text.WriteString(c.Text)
		if c.ToolEvent != nil {
			events++
		}
		if c.Done {
			done = c
		}
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if text.String() != "Hello" || events != 2 || len(done.ToolCalls) != 1 || done.ServedModel != "gpt-4o-2024-11-20" {
		t.Errorf("stream: text=%q events=%d done=%+v", text.String(), events, done)
	}
}

// TestAzureFoundry_AgentStoreRejectionFailsTurn: the Responses API defaults to
// storing, so resending without store=false would retain flow content
// server-side — the opposite of why the parameter is sent. A rejecting
// endpoint must fail the turn, never silently repair.
func TestAzureFoundry_AgentStoreRejectionFailsTurn(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if _, ok := body["store"]; ok {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"code": "unsupported_parameter", "param": "store",
				"message": "'store' is not supported with this model",
			}})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "resp_s", "status": "completed", "model": "gpt-4.1", "output": []any{}})
	})
	p := newAzureAgentProvider(t, srv)
	_, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil || !errors.Is(err, ErrProviderConfigInvalid) {
		t.Fatalf("Chat error = %v", err)
	}
	for _, want := range []string{"store", "retain"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	if got := len(srv.requests()); got != 1 {
		t.Errorf("request sent %d times; a store rejection must not be retried without the parameter", got)
	}
}

// TestAzureFoundry_AgentStoreRejectionAfterRepairsStillFailsTurn: the store
// guard sits ABOVE the repair bookkeeping, so a store rejection still fails
// the turn with the deliberate config error when other parameters were
// repaired first, instead of the raw 400 prose. (The Responses wire body
// carries one repairable parameter — max_output_tokens; temperature is no
// longer sent — so this exercises every repair the request can make.)
func TestAzureFoundry_AgentStoreRejectionAfterRepairsStillFailsTurn(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		reject := func(param string) {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"code": "unsupported_parameter", "param": param,
				"message": "'" + param + "' is not supported with this model",
			}})
		}
		if _, ok := body["temperature"]; ok {
			reject("temperature")
			return
		}
		if _, ok := body["max_output_tokens"]; ok {
			reject("max_output_tokens")
			return
		}
		reject("store")
	})
	p := newAzureAgentProvider(t, srv)
	_, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Temperature: 0.4, MaxTokens: 100,
		Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil || !errors.Is(err, ErrProviderConfigInvalid) {
		t.Fatalf("Chat error = %v, want ErrProviderConfigInvalid even after prior repairs", err)
	}
	if !strings.Contains(err.Error(), "retain") {
		t.Errorf("error %q does not explain the store refusal", err)
	}
	if got := len(srv.requests()); got != 2 {
		t.Errorf("request sent %d times, want 2 (the max_output_tokens repair, then the refused store)", got)
	}
}

// TestAzureFoundry_AgentRepairsSeveralParameters: a model can refuse
// parameters, one rejection round trip each; the turn must succeed on the
// request after the last repair (max_output_tokens dropped; temperature is
// never sent, so its rejection branch cannot trigger).
func TestAzureFoundry_AgentRepairsSeveralParameters(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		reject := func(param string) {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"code": "unsupported_parameter", "param": param,
				"message": "'" + param + "' is not supported with this model",
			}})
		}
		if _, ok := body["temperature"]; ok {
			reject("temperature")
			return
		}
		if _, ok := body["max_output_tokens"]; ok {
			reject("max_output_tokens")
			return
		}
		writeJSON(w, 200, map[string]any{
			"id": "resp_r", "status": "completed", "model": "gpt-5.6",
			"output": []map[string]any{{"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "Answer"}}}},
			"usage": map[string]any{"input_tokens": 2, "output_tokens": 1},
		})
	})
	p := newAzureAgentProvider(t, srv)
	resp, err := p.Chat(context.Background(), Request{
		Model: "agent:Helper", Temperature: 0.4, MaxTokens: 100,
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Answer" {
		t.Errorf("response = %+v", resp)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("agent request sent %d times, want 2 (the rejected max_output_tokens, then the accepted one)", len(reqs))
	}
	last := reqs[len(reqs)-1].Body
	if _, ok := last["temperature"]; ok {
		t.Errorf("temperature must never be sent: %v", last)
	}
	if _, ok := last["max_output_tokens"]; ok {
		t.Errorf("max_output_tokens was not dropped: %v", last)
	}
	if _, ok := last["store"]; !ok {
		t.Errorf("store:false must survive the repairs: %v", last)
	}
}

// PERF-6 (Responses path): a repair learned on one turn goes out up front on
// the next turn with the same agent (no rejection round trip, no re-upload of
// the history); `store` is never pre-applied; a memo the model now rejects is
// forgotten instead of failing every turn for its lifetime.
func TestAzureFoundry_AgentRepairIsRememberedAcrossRequests(t *testing.T) {
	var rejectCap atomic.Bool
	rejectCap.Store(true)
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if _, ok := body["max_output_tokens"]; ok && rejectCap.Load() {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"code": "unsupported_parameter", "param": "max_output_tokens",
				"message": "'max_output_tokens' is not supported with this model",
			}})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "resp_m", "status": "completed", "model": "gpt-5",
			"output": []map[string]any{{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "ok"}}}}})
	})
	p := newAzureAgentProvider(t, srv)
	req := Request{Model: "agent:Helper", MaxTokens: 100, Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Fatalf("first turn made %d requests, want the rejection and the repaired request", n)
	}
	// Second turn: repaired up front, one request, store:false still sent.
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 3 {
		t.Fatalf("second turn made %d requests, want exactly one (the learned repair pre-applied)", len(reqs)-2)
	}
	if _, ok := reqs[2].Body["max_output_tokens"]; ok {
		t.Errorf("the learned repair was not pre-applied: %v", reqs[2].Body)
	}
	if reqs[2].Body["store"] != false {
		t.Errorf("store:false must still be sent: %v", reqs[2].Body)
	}
	// Another agent on the same project has its own memo.
	other := Request{Model: "agent:Other", MaxTokens: 100, Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := p.Chat(context.Background(), other); err != nil {
		t.Fatalf("other agent Chat: %v", err)
	}
	if reqs := srv.requests(); len(reqs) != 5 || reqs[3].Body["max_output_tokens"] != float64(100) {
		t.Errorf("another agent must start from the raw request: %d requests, body %v", len(reqs), reqs[3].Body)
	}
	// A memo never learns store: a store rejection still fails the turn.
	ResetAzureFoundryCaches()
	if repairs := repairsFor(srv.URL + "/api/projects/p1/openai/v1/responses\x00Helper"); repairs != nil {
		t.Errorf("ResetAzureFoundryCaches must clear the learned repairs, got %v", repairs)
	}
}

// D-36: baki's provider-level temperature is not forwarded with an
// agent_reference — the portal-tuned agent keeps its own sampling — while the
// output cap (a budget guard) stays.
func TestAzureFoundry_AgentRequestOmitsTemperature(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 200, map[string]any{"id": "resp_t", "status": "completed", "model": "gpt-4.1", "output": []any{}})
	})
	p := newAzureAgentProvider(t, srv)
	if _, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Temperature: 0.7, MaxTokens: 200,
		Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := srv.requests()[0].Body
	if _, ok := body["temperature"]; ok {
		t.Errorf("temperature sent with agent_reference: %v", body)
	}
	if body["max_output_tokens"] != float64(200) {
		t.Errorf("max_output_tokens = %v, want 200", body["max_output_tokens"])
	}
}

// D-32: the Responses API rejects max_output_tokens below 16 with a 400 that
// is not a repairable parameter; a smaller caller cap (the connection test's
// 5) is raised to the floor so the probe does not report a validation error
// as a connection failure.
func TestAzureFoundry_AgentChatRaisesTinyOutputCap(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if cap, _ := body["max_output_tokens"].(float64); cap < 16 {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"code": "integer_below_min_value", "param": "max_output_tokens",
				"message": "Invalid 'max_output_tokens': integer below minimum value. Expected a value >= 16, but got 5 instead.",
			}})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "resp_m", "status": "completed", "model": "gpt-4.1",
			"output": []map[string]any{{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "Hi"}}}}})
	})
	p := newAzureAgentProvider(t, srv)
	resp, err := p.Chat(context.Background(), Request{Model: "agent:Helper", MaxTokens: 5, Messages: []Message{{Role: "user", Content: "Hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Hi" {
		t.Errorf("response = %+v", resp)
	}
	reqs := srv.requests()
	if len(reqs) != 1 || reqs[0].Body["max_output_tokens"] != float64(minResponsesOutputTokens) {
		t.Errorf("requests = %+v, want exactly one with max_output_tokens raised to %d", reqs, minResponsesOutputTokens)
	}
	// A larger cap is passed through unchanged.
	if body := p.agentRequest(Request{Model: "agent:Helper", MaxTokens: 4096}, false); body.MaxOutputTokens != 4096 {
		t.Errorf("max_output_tokens = %d, want 4096", body.MaxOutputTokens)
	}
	if body := p.agentRequest(Request{Model: "agent:Helper"}, false); body.MaxOutputTokens != 0 {
		t.Errorf("no caller cap must send no max_output_tokens, got %d", body.MaxOutputTokens)
	}
}

// TestAzureFoundry_AgentChatRejectsOversizedBody: a body past the read cap
// must fail with a clear error, not surface later as a JSON truncation.
func TestAzureFoundry_AgentChatRejectsOversizedBody(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxAgentResponseBytes+1))
	})
	p := newAzureAgentProvider(t, srv)
	_, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "exceeded 10 MB") {
		t.Fatalf("Chat error = %v", err)
	}
}

func TestAzureFoundry_AgentNeedsProjectAndEntra(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		t.Errorf("no request expected, got %s", r.URL.Path)
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ProjectEndpoint = srv.URL + "/api/projects/p1" })
	if _, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("API-key mode with an agent: %v", err)
	}
	if !IsAgentModel("agent:Helper") || IsAgentModel("gpt-4o") || agentNameOf("agent:Helper") != "Helper" {
		t.Error("agent id helpers")
	}
}
