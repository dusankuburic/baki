package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureClaudeBody runs a Chat against a stub server and returns the decoded
// request body the provider sent, so we can assert on sampling/thinking/cache.
func captureClaudeBody(t *testing.T, req Request) map[string]any {
	t.Helper()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	p := NewClaudeProvider("test-key")
	p.client = server.Client()
	orig := claudeAPIURL
	defer func() { claudeAPIURL = orig }()
	claudeAPIURL = server.URL

	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return body
}

// TestClaudeProvider_StreamSurfacesToolCalls is an end-to-end check of the real
// streaming path: ClaudeProvider.Stream → HTTP → parseClaudeSSE → tool calls on
// the Done chunk. It streams a short text preamble then a tool_use block exactly
// as the Anthropic API does, and asserts the assembled ToolCall reaches the
// caller (this is the seam the agentic streaming loop depends on).
func TestClaudeProvider_StreamSurfacesToolCalls(t *testing.T) {
	sse := "event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Searching…\"}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_9\",\"name\":\"search_flow\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"x\\\"}\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"usage\":{\"output_tokens\":11},\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer server.Close()

	p := NewClaudeProvider("test-key")
	p.client = server.Client()
	orig := claudeAPIURL
	defer func() { claudeAPIURL = orig }()
	claudeAPIURL = server.URL

	var text string
	var done Chunk
	err := p.Stream(context.Background(), Request{
		Model:    "claude-sonnet-4-6",
		Messages: []Message{{Role: "user", Content: "analyze"}},
		Tools:    []ToolDefinition{{Name: "search_flow", Description: "x", InputSchema: schema(`{"type":"object"}`)}},
	}, func(c Chunk) {
		if c.Done {
			done = c
		} else if c.Text != "" {
			text += c.Text
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if text != "Searching…" {
		t.Errorf("expected preamble text streamed, got %q", text)
	}
	if len(done.ToolCalls) != 1 || done.ToolCalls[0].Name != "search_flow" || string(done.ToolCalls[0].Input) != `{"query":"x"}` {
		t.Errorf("tool call did not surface end-to-end: %+v", done.ToolCalls)
	}
}

// TestClaude_OpusOmitsTemperatureUsesThinking verifies that Opus 4.7+ requests
// drop the (now-rejected) temperature and enable adaptive thinking instead.
func TestClaude_OpusOmitsTemperatureUsesThinking(t *testing.T) {
	body := captureClaudeBody(t, Request{
		Model:       "claude-opus-4-8",
		Temperature: 0.7,
		Messages:    []Message{{Role: "user", Content: "hi"}},
	})
	if _, ok := body["temperature"]; ok {
		t.Error("temperature must be omitted for Opus 4.7+ (it returns 400)")
	}
	think, ok := body["thinking"].(map[string]any)
	if !ok || think["type"] != "adaptive" {
		t.Errorf("expected adaptive thinking, got %v", body["thinking"])
	}
}

// TestClaude_OpusOmitsThinkingWithTools verifies the agentic-loop guard: when
// tools are attached, adaptive thinking is NOT enabled on Opus (the API would
// require preserving thinking blocks across tool turns). Temperature stays
// omitted regardless.
func TestClaude_OpusOmitsThinkingWithTools(t *testing.T) {
	body := captureClaudeBody(t, Request{
		Model:       "claude-opus-4-8",
		Temperature: 0.7,
		Messages:    []Message{{Role: "user", Content: "hi"}},
		Tools:       []ToolDefinition{{Name: "search_flow", Description: "x", InputSchema: schema(`{"type":"object"}`)}},
	})
	if _, ok := body["temperature"]; ok {
		t.Error("temperature must be omitted for Opus 4.7+")
	}
	if _, ok := body["thinking"]; ok {
		t.Error("thinking must be omitted when tools are present (block-preservation requirement)")
	}
}

// TestClaude_SonnetKeepsTemperatureNoThinking verifies the legacy/sampling path
// is preserved for models that still accept temperature (Sonnet 4.6).
func TestClaude_SonnetKeepsTemperatureNoThinking(t *testing.T) {
	body := captureClaudeBody(t, Request{
		Model:       "claude-sonnet-4-6",
		Temperature: 0.7,
		Messages:    []Message{{Role: "user", Content: "hi"}},
	})
	if body["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", body["temperature"])
	}
	if _, ok := body["thinking"]; ok {
		t.Error("thinking must not be set for Sonnet 4.6 sampling path")
	}
}

// TestClaude_SystemCarriesCacheControl verifies the system prompt is sent as a
// structured block with an ephemeral cache breakpoint (T2.1), so the stable
// tools+system prefix is cached across tool-loop iterations and turns.
func TestClaude_SystemCarriesCacheControl(t *testing.T) {
	body := captureClaudeBody(t, Request{
		Model:        "claude-sonnet-4-6",
		SystemPrompt: "You are a flow analyzer.",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	})
	sys, ok := body["system"].([]any)
	if !ok || len(sys) == 0 {
		t.Fatalf("expected system to be a non-empty array, got %v", body["system"])
	}
	block, _ := sys[0].(map[string]any)
	cc, ok := block["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Errorf("expected ephemeral cache_control on system block, got %v", block["cache_control"])
	}
}
