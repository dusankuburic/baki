package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOpenAICompatProviders_StreamSendsTools guards a regression: the agentic
// tool loop streams turns (provider.Stream), so every tool-capable provider must
// include tools in its streaming request body — otherwise enabling "Use tools"
// silently offers the model no tools. Claude is covered separately
// (TestClaudeProvider_StreamSurfacesToolCalls); this covers the OpenAI family.
func TestOpenAICompatProviders_StreamSendsTools(t *testing.T) {
	tools := []ToolDefinition{{Name: "search_flow", Description: "x", InputSchema: schema(`{"type":"object"}`)}}

	check := func(t *testing.T, urlPtr *string, p Provider) {
		t.Helper()
		if !p.SupportsTools() {
			t.Skipf("%s does not support tools", p.ID())
		}
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}))
		defer server.Close()

		orig := *urlPtr
		*urlPtr = server.URL
		defer func() { *urlPtr = orig }()

		if err := p.Stream(context.Background(), Request{
			Model:    "m",
			Messages: []Message{{Role: "user", Content: "hi"}},
			Tools:    tools,
		}, func(Chunk) {}); err != nil {
			t.Fatalf("%s Stream: %v", p.ID(), err)
		}
		if _, ok := body["tools"]; !ok {
			t.Errorf("%s streaming request omitted tools — agentic mode would offer the model no tools", p.ID())
		}
	}

	t.Run("openai", func(t *testing.T) { check(t, &openAIBaseURL, NewOpenAIProvider("k")) })
	t.Run("xai", func(t *testing.T) { check(t, &xaiBaseURL, NewXAIProvider("k")) })
	t.Run("glm", func(t *testing.T) { check(t, &glmBaseURL, NewGLMProvider("k")) })
	t.Run("github-models", func(t *testing.T) { check(t, &githubModelsBaseURL, NewGitHubModelsProvider("k")) })
}

// TestGeminiStream_ParallelToolResultsInOneUserContent (R-tool-loop-12): on the wire, the history of a
// two-call turn reaches streamGenerateContent as one model content followed by ONE user content — the
// chat loop appends one tool message per call, and Gemini wants all of a turn's responses together.
func TestGeminiStream_ParallelToolResultsInOneUserContent(t *testing.T) {
	var body struct {
		Contents []struct {
			Role  string           `json:"role"`
			Parts []map[string]any `json:"parts"`
		} `json:"contents"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"done\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	}))
	defer server.Close()
	prev := geminiBaseHost
	geminiBaseHost = server.URL
	defer func() { geminiBaseHost = prev }()

	p := &GeminiProvider{apiKey: "k", client: server.Client()}
	calls := []ToolCall{
		{ID: "call_a", Name: "search_flow", Input: schema(`{}`)},
		{ID: "call_b", Name: "get_block", Input: schema(`{}`)},
	}
	err := p.Stream(context.Background(), Request{
		Model: "gemini-2.5-pro",
		Messages: []Message{
			{Role: "user", Content: "q"},
			{Role: "assistant", ToolCalls: calls},
			{Role: "tool", Content: "result-a", ToolCallID: "call_a"},
			{Role: "tool", Content: "result-b", ToolCallID: "call_b"},
		},
		Tools: []ToolDefinition{{Name: "search_flow", InputSchema: schema(`{"type":"object"}`)}, {Name: "get_block", InputSchema: schema(`{"type":"object"}`)}},
	}, func(Chunk) {})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	modelAt := -1
	for i, c := range body.Contents {
		if c.Role == "model" {
			modelAt = i
		}
	}
	if modelAt < 0 {
		t.Fatalf("no model content in the request: %+v", body.Contents)
	}
	userAfter, responses := 0, 0
	for _, c := range body.Contents[modelAt+1:] {
		if c.Role == "user" {
			userAfter++
		}
		for _, part := range c.Parts {
			if _, ok := part["functionResponse"]; ok {
				responses++
			}
		}
	}
	if userAfter != 1 || responses != 2 {
		t.Errorf("after the two-call model turn the request carries %d user contents with %d functionResponse parts, want 1 user content with 2 parts", userAfter, responses)
	}
}
