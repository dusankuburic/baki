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
