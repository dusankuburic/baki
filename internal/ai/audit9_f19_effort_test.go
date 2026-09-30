package ai

// F-19 (reasoning-effort control): the chat-completions reasoning_effort,
// its repairs and the gpt-5.6 "none" override, and Claude's
// output_config.effort inside the claudeRemovesSampling branch.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chat200 records each request body and answers a minimal chat completion.
func chat200(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		writeJSON(w, 200, map[string]any{"model": "o3", "choices": []map[string]any{
			{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func effortProvider(srv *httptest.Server) *OpenAIProvider {
	base := srv.URL + "/chat/completions"
	return &OpenAIProvider{openaiBase: openaiBase{apiKey: "k", client: srv.Client(), baseURL: &base, providerLabel: "openai"}}
}

// TestOpenAICompat_ReasoningEffortSentWhenSet: the user-chosen effort rides
// the chat-completions body; empty omits the field.
func TestOpenAICompat_ReasoningEffortSentWhenSet(t *testing.T) {
	srv, bodies := chat200(t)
	p := effortProvider(srv)
	if _, err := p.Chat(context.Background(), Request{Model: "o3", ReasoningEffort: "high",
		Messages: []Message{{Role: "user", Content: "q"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if (*bodies)[0]["reasoning_effort"] != "high" {
		t.Errorf("body = %v, want reasoning_effort high", (*bodies)[0])
	}
	if _, err := p.Chat(context.Background(), Request{Model: "o3",
		Messages: []Message{{Role: "user", Content: "q"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, has := (*bodies)[1]["reasoning_effort"]; has {
		t.Errorf("body = %v, want no reasoning_effort by default", (*bodies)[1])
	}
}

// TestOpenAICompat_ReasoningEffortParameterRepairMemoises: a model that
// rejects the parameter outright (unsupported_parameter) gets the one-shot
// repair, and the memo pre-applies the drop to later values (the parameter
// itself is unsupported — any value would be rejected), so the next turn is
// a single request.
func TestOpenAICompat_ReasoningEffortParameterRepairMemoises(t *testing.T) {
	var bodies []map[string]any
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if _, has := body["reasoning_effort"]; has {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"code": "unsupported_parameter", "param": "reasoning_effort",
				"message": "Unsupported parameter: 'reasoning_effort' is not supported with this model."}})
			return
		}
		writeJSON(w, 200, map[string]any{"model": "gpt-4o", "choices": []map[string]any{
			{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}}})
	}))
	t.Cleanup(srv.Close)
	p := effortProvider(srv)
	if _, err := p.Chat(context.Background(), Request{Model: "gpt-4o", ReasoningEffort: "high",
		Messages: []Message{{Role: "user", Content: "q"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if n != 2 {
		t.Fatalf("%d requests, want the rejection and the repair", n)
	}
	// The memo holds the generic drop, apart from the tools "none" marker.
	if params := repairsFor(p.repairsKey("gpt-4o")); len(params) != 1 || params[0] != reasoningEffortParam {
		t.Errorf("memo = %v, want reasoning_effort", params)
	}
	// A later value: the memo pre-applies the drop — one request, no
	// rejection round trip.
	if _, err := p.Chat(context.Background(), Request{Model: "gpt-4o", ReasoningEffort: "low",
		Messages: []Message{{Role: "user", Content: "q"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if n != 3 {
		t.Errorf("%d requests after the second turn, want 3 (the memo drops the value up front)", n)
	}
	if _, has := bodies[2]["reasoning_effort"]; has {
		t.Errorf("second turn body = %v, want reasoning_effort dropped", bodies[2])
	}
}

// TestOpenAICompat_ReasoningEffortDropIsNotReasoningOff (A2): on the Stream +
// tools path, a non-reasoning model refusing the user's effort outright is
// the generic drop, not the gpt-5.6 "none" repair — no reasoning_off notice
// (the model never reasoned), and the second turn is one request.
func TestOpenAICompat_ReasoningEffortDropIsNotReasoningOff(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["reasoning_effort"]; has {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"code": "unsupported_parameter", "param": "reasoning_effort",
				"message": "Unsupported parameter: 'reasoning_effort' is not supported with this model."}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(azureStreamFixture))
	}))
	t.Cleanup(srv.Close)
	p := effortProvider(srv)
	req := Request{Model: "gpt-4o", ReasoningEffort: "high",
		Messages: []Message{{Role: "user", Content: "fix it"}},
		Tools:    []ToolDefinition{{Name: "search_flow", InputSchema: []byte(`{"type":"object"}`)}}}
	for turn, want := range []int{2, 3} {
		err := p.Stream(context.Background(), req, func(c Chunk) {
			if c.Notice != nil {
				t.Errorf("turn %d: notice %+v — dropping a refused effort is not reasoning off", turn+1, c.Notice)
			}
		})
		if err != nil {
			t.Fatalf("turn %d: Stream: %v", turn+1, err)
		}
		if n != want {
			t.Errorf("turn %d: %d requests in total, want %d", turn+1, n, want)
		}
	}
}

// TestOpenAICompat_ReasoningEffortValueRejectionDoesNotMemoise: a refused
// VALUE (unsupported_value) is retried without the field but never memoised
// — a later valid value still reaches the model.
func TestOpenAICompat_ReasoningEffortValueRejectionDoesNotMemoise(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["reasoning_effort"]; has {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"code": "unsupported_value", "param": "reasoning_effort",
				"message": "Unsupported value: 'reasoning_effort' does not support 'high' with this model."}})
			return
		}
		writeJSON(w, 200, map[string]any{"model": "o3", "choices": []map[string]any{
			{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}}})
	}))
	t.Cleanup(srv.Close)
	p := effortProvider(srv)
	if _, err := p.Chat(context.Background(), Request{Model: "o3", ReasoningEffort: "high",
		Messages: []Message{{Role: "user", Content: "q"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if n != 2 {
		t.Fatalf("%d requests, want the rejection and the retry without the field", n)
	}
	// No memo: the next turn sends the value again (and again falls back).
	if _, err := p.Chat(context.Background(), Request{Model: "o3", ReasoningEffort: "medium",
		Messages: []Message{{Role: "user", Content: "q"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if n != 4 {
		t.Errorf("%d requests after the second turn, want 4 (no memo: the value is still tried)", n)
	}
}

// TestOpenAICompat_UserEffortOverriddenByToolsNoneRepair: on a gpt-5.6-style
// deployment the learned "none" repair overrides the user's value — the two
// features must not fight — and the reasoning_off notice says so.
func TestOpenAICompat_UserEffortOverriddenByToolsNoneRepair(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["tools"]; has && body["reasoning_effort"] != "none" {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"message": gpt56ToolsRejection, "type": "invalid_request_error", "param": "reasoning_effort"}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(azureStreamFixture))
	}))
	t.Cleanup(srv.Close)
	p := effortProvider(srv)
	req := Request{Model: "gpt56", ReasoningEffort: "low",
		Messages: []Message{{Role: "user", Content: "fix it"}},
		Tools:    []ToolDefinition{{Name: "search_flow", InputSchema: []byte(`{"type":"object"}`)}}}
	// Turn 1 repairs afresh (rejection + "none"); turn 2 pre-applies the
	// memo's "none" in one request — both run with reasoning off and say so.
	for turn, want := range []int{2, 3} {
		var notice bool
		err := p.Stream(context.Background(), req, func(c Chunk) {
			if c.Notice != nil && c.Notice.Name == "reasoning_off" {
				notice = true
			}
		})
		if err != nil {
			t.Fatalf("turn %d: Stream: %v", turn+1, err)
		}
		if !notice {
			t.Errorf("turn %d: the reasoning_off notice must fire when the none repair overrides the user's effort", turn+1)
		}
		if n != want {
			t.Errorf("turn %d: %d requests in total, want %d", turn+1, n, want)
		}
	}
	// The memo holds the "none" marker, not the generic drop.
	if params := repairsFor(p.repairsKey("gpt56")); len(params) != 1 || params[0] != reasoningEffortNoneRepair {
		t.Errorf("memo = %v, want only %s", params, reasoningEffortNoneRepair)
	}
}

// TestClaudeOutputConfigEffort: output_config.effort is sent exactly on the
// models that reject sampling parameters (claudeRemovesSampling), never on
// the older ones.
func TestClaudeOutputConfigEffort(t *testing.T) {
	c := &ClaudeProvider{}
	wire := func(model string, effort string) string {
		body, _ := json.Marshal(c.buildBody(Request{Model: model, ReasoningEffort: effort,
			Messages: []Message{{Role: "user", Content: "q"}}}, false))
		return string(body)
	}
	if got := wire("claude-opus-5", "high"); !strings.Contains(got, `"output_config":{"effort":"high"}`) {
		t.Errorf("opus 5 wire = %s, want output_config.effort high", got)
	}
	if got := wire("claude-mythos-preview", "low"); !strings.Contains(got, `"output_config":{"effort":"low"}`) {
		t.Errorf("mythos wire = %s, want output_config.effort low", got)
	}
	if got := wire("claude-opus-5", ""); strings.Contains(got, "output_config") {
		t.Errorf("opus 5 without an effort must omit output_config: %s", got)
	}
	// An older model keeps sampling parameters and never sees the object.
	c2 := &ClaudeProvider{}
	wireT, _ := json.Marshal(c2.buildBody(Request{Model: "claude-sonnet-4-5", ReasoningEffort: "high",
		Temperature: 0.4, Messages: []Message{{Role: "user", Content: "q"}}}, false))
	if got := string(wireT); strings.Contains(got, "output_config") {
		t.Errorf("sonnet 4.5 wire = %s, must not carry output_config", got)
	} else if !strings.Contains(got, `"temperature":0.4`) {
		t.Errorf("sonnet 4.5 wire = %s, want temperature 0.4", got)
	}
}
