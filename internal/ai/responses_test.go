package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestToResponsesInput(t *testing.T) {
	items := toResponsesInput("SYS", []Message{
		{Role: "user", Content: "find issues"},
		{Role: "assistant", Content: "looking", ToolCalls: []ToolCall{{ID: "call_1", Name: "search_flow", Input: json.RawMessage(`{"q":"x"}`)}, {ID: "call_2", Name: "get_block"}}},
		{Role: "tool", ToolCallID: "call_1", Content: "result one"},
		{Role: "tool", ToolCallID: "call_2", Content: "error: unknown tool get_block"},
		{Role: "assistant", Content: "done"},
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
	if keepalives != 1 {
		t.Errorf("searching notification should yield one keepalive chunk, got %d", keepalives)
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

	truncated := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	err := parseResponsesSSE(strings.NewReader(truncated), func(Chunk) {})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("stream without terminal event: %v", err)
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
	agent, _ := body["agent"].(map[string]any)
	if agent["type"] != "agent_reference" || agent["name"] != "Helper" {
		t.Errorf("agent reference = %v", body["agent"])
	}
	if body["store"] != false || body["tools"] != nil || body["model"] != nil || body["instructions"] != nil {
		t.Errorf("agent request must carry store:false and no request-level tools/model/instructions: %v", body)
	}
	input, _ := body["input"].([]any)
	first, _ := input[0].(map[string]any)
	if len(input) != 2 || first["role"] != "system" || first["content"] != "SYS" {
		t.Errorf("input items = %v", input)
	}
	if body["max_output_tokens"] != float64(100) || body["temperature"] != 0.3 {
		t.Errorf("limits = %v / %v", body["max_output_tokens"], body["temperature"])
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

func TestAzureFoundry_ModelsIncludeProjectDeploymentsAndAgents(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		switch r.URL.Path {
		case "/openai/v1/models":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "chat1"}, {"id": "emb1"}}})
		case "/api/projects/p1/deployments":
			if r.URL.Query().Get("api-version") != "v1" {
				t.Errorf("deployments api-version = %q", r.URL.Query().Get("api-version"))
			}
			writeJSON(w, 200, map[string]any{"value": []map[string]any{
				{"name": "chat1", "type": "ModelDeployment", "modelName": "gpt-4o", "modelPublisher": "OpenAI"},
				{"name": "extra", "type": "ModelDeployment", "modelName": "DeepSeek-V3-0324"},
			}})
		case "/api/projects/p1/agents":
			if r.URL.Query().Get("after") != "" {
				writeJSON(w, 200, map[string]any{"data": []map[string]any{{"name": "Second"}}, "has_more": false})
				return
			}
			writeJSON(w, 200, map[string]any{"data": []map[string]any{
				{"id": "agent_1", "name": "Helper", "versions": map[string]any{"latest": map[string]any{"definition": map[string]any{"kind": "prompt", "model": "gpt-4.1"}}}},
			}, "has_more": true, "last_id": "agent_1"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newAzureAgentProvider(t, srv)
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if m := byID["chat1"]; m.DisplayName != "chat1 · gpt-4o" || m.Pricing.InputCostPerM != 2.5 || m.ServedModel != "gpt-4o" {
		t.Errorf("chat1 = %+v", m)
	}
	if m, ok := byID["extra"]; !ok || m.ServedModel != "DeepSeek-V3-0324" {
		t.Errorf("project-only deployment missing/unenriched: %+v", m)
	}
	if m := byID["agent:Helper"]; m.DisplayName != "Agent · Helper" || m.ServedModel != "gpt-4.1" || m.Pricing.InputCostPerM != 2.0 {
		t.Errorf("agent entry = %+v", m)
	}
	if _, ok := byID["agent:Second"]; !ok {
		t.Error("second agents page not followed")
	}
	if len(models) != 5 {
		t.Errorf("got %d models: %+v", len(models), models)
	}
}
