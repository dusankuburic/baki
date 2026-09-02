package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDetectToolsUnsupportedError pins the two-sided matching contract: the
// message must both mention tools/function calling AND say they're
// unavailable. An error merely containing "tool" (tool result too long, tool
// execution failed) must NOT classify — the chat loop would wrongly strip a
// working tool setup.
func TestDetectToolsUnsupportedError(t *testing.T) {
	positive := []struct {
		name string
		msg  string
	}{
		{"azure model-mesh wording", "Model llama-3.1-70b does not support tools."},
		{"openai param wording", "tool_calls is not supported for this model"},
		{"function calling", "This model does not support function calling."},
		{"disabled", "Tools are disabled for the selected model."},
		{"not available", "Function calling is not available on gpt-3.5-turbo-instruct"},
		{"uppercase mix", "TOOL USE IS NOT SUPPORTED"},
	}
	for _, tc := range positive {
		t.Run("match/"+tc.name, func(t *testing.T) {
			if err := detectToolsUnsupportedError(400, tc.msg); !errors.Is(err, ErrToolsUnsupported) {
				t.Errorf("detectToolsUnsupportedError(400, %q) = %v, want ErrToolsUnsupported", tc.msg, err)
			}
		})
	}

	negative := []struct {
		name string
		msg  string
	}{
		{"tool mentioned, available", "tool execution failed"},
		{"tool result size", "tool result too long"},
		{"unrelated 400", "invalid request body"},
		{"empty message", ""},
	}
	for _, tc := range negative {
		t.Run("nomatch/"+tc.name, func(t *testing.T) {
			if err := detectToolsUnsupportedError(400, tc.msg); err != nil {
				t.Errorf("detectToolsUnsupportedError(400, %q) = %v, want nil", tc.msg, err)
			}
		})
	}

	// Only a 400 classifies — a 500 saying "tools not supported" is a server
	// fault, not a capability statement.
	if err := detectToolsUnsupportedError(500, "tools are not supported"); err != nil {
		t.Errorf("non-400 status classified: %v", err)
	}
}

// serveOpenAIError points the OpenAI provider at a fake API answering every
// request with the given status + JSON error body (openAIBaseURL is captured
// by pointer, so swapping the package var's value retargets the provider).
func serveOpenAIError(t *testing.T, status int, message string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error": {"message": "` + message + `", "type": "invalid_request_error"}}`))
	}))
	prev := openAIBaseURL
	openAIBaseURL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		openAIBaseURL = prev
	})
}

// TestOpenAI_ToolsUnsupportedClassified proves the wiring end-to-end for the
// OpenAI-compatible family (OpenAI, Azure, GitHub Models, GLM, xAI): a 400
// "model does not support tools" surfaces as the ErrToolsUnsupported sentinel
// on BOTH the Chat and Stream paths, so the chat tool loop can degrade
// gracefully instead of failing the turn with a raw 400.
func TestOpenAI_ToolsUnsupportedClassified(t *testing.T) {
	serveOpenAIError(t, 400, "Model llama-3.1-70b does not support tools.")
	p := NewOpenAIProvider("k")
	req := Request{Model: "llama-3.1-70b", Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []ToolDefinition{{Name: "read_doc", Description: "d", InputSchema: []byte(`{"type":"object"}`)}}}

	if _, err := p.Chat(context.Background(), req); !errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("Chat: want ErrToolsUnsupported, got %v", err)
	}
	err := p.Stream(context.Background(), req, func(Chunk) {})
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Errorf("Stream: want ErrToolsUnsupported, got %v", err)
	}
}

// TestGitHubModels_NoKeySkipsLiveFetch: the metadata provider is constructed
// with an empty key for unconfigured users; Models must return the catalog
// without any outbound HTTP call (the doomed GET 401'd on every ListProviders
// call — synchronously, inside the provider listing loop).
func TestGitHubModels_NoKeySkipsLiveFetch(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(401)
	}))
	defer srv.Close()
	prev := githubModelsBaseURL
	githubModelsBaseURL = srv.URL + "/chat/completions"
	t.Cleanup(func() { githubModelsBaseURL = prev })

	p := NewGitHubModelsProvider("")
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if hits != 0 {
		t.Errorf("expected no HTTP call with empty key, got %d", hits)
	}
	if len(models) == 0 {
		t.Error("expected catalog fallback models, got none")
	}
}
