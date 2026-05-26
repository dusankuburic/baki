package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaudeProvider_ID(t *testing.T) {
	p := NewClaudeProvider("test-key")
	if p.ID() != "claude" {
		t.Errorf("expected claude, got %s", p.ID())
	}
	if p.Name() != "Claude" {
		t.Errorf("expected Claude, got %s", p.Name())
	}
	if p.DefaultModel() != "claude-sonnet-4-5" {
		t.Errorf("expected claude-sonnet-4-5, got %s", p.DefaultModel())
	}
}

func TestClaudeProvider_Models(t *testing.T) {
	p := NewClaudeProvider("test-key")
	models := p.Models()
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	found := false
	for _, m := range models {
		if m.ID == "claude-sonnet-4-5" {
			found = true
			if m.ContextLimit != 200000 {
				t.Errorf("expected 200000 context limit, got %d", m.ContextLimit)
			}
		}
	}
	if !found {
		t.Error("expected to find claude-sonnet-4-5 model")
	}
}

func TestClaudeProvider_Chat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Error("missing x-api-key header")
		}
		if r.Header.Get("anthropic-version") != claudeAPIVersion {
			t.Error("missing anthropic-version header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "msg_123",
			"type": "message",
			"role": "assistant",
			"content": [{"type": "text", "text": "Hello from Claude"}],
			"model": "claude-sonnet-4-5",
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer server.Close()

	p := NewClaudeProvider("test-key")
	p.client = server.Client()

	origURL := claudeAPIURL
	defer func() { claudeAPIURL = origURL }()
	claudeAPIURL = server.URL

	resp, err := p.Chat(context.Background(), Request{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello from Claude" {
		t.Errorf("expected 'Hello from Claude', got %s", resp.Content)
	}
	if resp.TokensIn != 10 {
		t.Errorf("expected 10 input tokens, got %d", resp.TokensIn)
	}
	if resp.TokensOut != 5 {
		t.Errorf("expected 5 output tokens, got %d", resp.TokensOut)
	}
}

func TestClaudeProvider_Chat_401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"type": "authentication_error", "message": "invalid key"}}`))
	}))
	defer server.Close()

	p := NewClaudeProvider("bad-key")
	p.client = server.Client()

	origURL := claudeAPIURL
	defer func() { claudeAPIURL = origURL }()
	claudeAPIURL = server.URL

	_, err := p.Chat(context.Background(), Request{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != ErrApiKeyInvalid {
		t.Errorf("expected ErrApiKeyInvalid, got %v", err)
	}
}

func TestClaudeProvider_Chat_429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error": {"type": "rate_limit_error", "message": "slow down"}}`))
	}))
	defer server.Close()

	p := NewClaudeProvider("test-key")
	p.client = server.Client()

	origURL := claudeAPIURL
	defer func() { claudeAPIURL = origURL }()
	claudeAPIURL = server.URL

	_, err := p.Chat(context.Background(), Request{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != ErrRateLimited {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
}

func TestClaudeProvider_Stream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: content_block_delta\n"))
		w.Write([]byte("data: {\"delta\": {\"type\": \"text_delta\", \"text\": \"Hello\"}}\n\n"))
		w.Write([]byte("event: content_block_delta\n"))
		w.Write([]byte("data: {\"delta\": {\"type\": \"text_delta\", \"text\": \" World\"}}\n\n"))
		w.Write([]byte("event: message_delta\n"))
		w.Write([]byte("data: {\"usage\": {\"output_tokens\": 3}, \"delta\": {\"stop_reason\": \"end_turn\"}}\n\n"))
	}))
	defer server.Close()

	p := NewClaudeProvider("test-key")
	p.client = server.Client()

	origURL := claudeAPIURL
	defer func() { claudeAPIURL = origURL }()
	claudeAPIURL = server.URL

	var chunks []string
	var done bool
	err := p.Stream(context.Background(), Request{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(chunk Chunk) {
		if chunk.Done {
			done = true
		} else if chunk.Text != "" {
			chunks = append(chunks, chunk.Text)
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("expected done signal")
	}
	if len(chunks) != 2 || chunks[0] != "Hello" || chunks[1] != " World" {
		t.Errorf("expected [Hello,  World], got %v", chunks)
	}
}

func TestOpenAIProvider_Chat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "chatcmpl-123",
			"choices": [{
				"message": {"role": "assistant", "content": "Hello from GPT"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 8, "completion_tokens": 4, "total_tokens": 12}
		}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("test-key")
	p.client = server.Client()

	origURL := openAIBaseURL
	defer func() { openAIBaseURL = origURL }()
	openAIBaseURL = server.URL

	resp, err := p.Chat(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello from GPT" {
		t.Errorf("expected 'Hello from GPT', got %s", resp.Content)
	}
}

func TestOpenAIProvider_Stream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"Hi\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \" there\"}, \"finish_reason\": \"stop\"}], \"usage\": {\"completion_tokens\": 3}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAIProvider("test-key")
	p.client = server.Client()

	origURL := openAIBaseURL
	defer func() { openAIBaseURL = origURL }()
	openAIBaseURL = server.URL

	var chunks []string
	var done bool
	err := p.Stream(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(chunk Chunk) {
		if chunk.Done {
			done = true
		} else if chunk.Text != "" {
			chunks = append(chunks, chunk.Text)
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("expected done signal")
	}
	if len(chunks) != 2 || chunks[0] != "Hi" || chunks[1] != " there" {
		t.Errorf("expected [Hi,  there], got %v", chunks)
	}
}

func TestOpenAIProvider_Chat_401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"message": "invalid key", "type": "invalid_request_error"}}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("bad-key")
	p.client = server.Client()

	origURL := openAIBaseURL
	defer func() { openAIBaseURL = origURL }()
	openAIBaseURL = server.URL

	_, err := p.Chat(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != ErrApiKeyInvalid {
		t.Errorf("expected ErrApiKeyInvalid, got %v", err)
	}
}

func TestGeminiProvider_Chat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.String(), "key=test-key") {
			t.Error("missing API key in URL")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"candidates": [{
				"content": {"parts": [{"text": "Hello from Gemini"}], "role": "model"},
				"finishReason": "STOP"
			}],
			"usageMetadata": {"promptTokenCount": 6, "candidatesTokenCount": 4, "totalTokenCount": 10}
		}`))
	}))
	defer server.Close()

	p := NewGeminiProvider("test-key")
	p.client = server.Client()

	origGeminiURL := geminiURL
	defer func() { geminiURL = origGeminiURL }()
	geminiURL = func(model, apiKey, action string) string {
		return server.URL + "?key=" + apiKey
	}

	resp, err := p.Chat(context.Background(), Request{
		Model:    "gemini-2.5-pro",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello from Gemini" {
		t.Errorf("expected 'Hello from Gemini', got %s", resp.Content)
	}
	if resp.TokensIn != 6 {
		t.Errorf("expected 6 input tokens, got %d", resp.TokensIn)
	}
}

func TestGeminiProvider_Models(t *testing.T) {
	p := NewGeminiProvider("test-key")
	models := p.Models()
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if p.DefaultModel() != "gemini-2.5-pro" {
		t.Errorf("expected gemini-2.5-pro default, got %s", p.DefaultModel())
	}
}

func TestGitHubModelsProvider_ID(t *testing.T) {
	p := NewGitHubModelsProvider("test-token")
	if p.ID() != "github-models" {
		t.Errorf("expected github-models, got %s", p.ID())
	}
	if p.Name() != "GitHub Models" {
		t.Errorf("expected GitHub Models, got %s", p.Name())
	}
}

func TestGitHubModelsProvider_Chat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing Authorization header")
		}
		if r.Header.Get("x-ms-model-mesh-model-id") != "gpt-4o" {
			t.Error("missing x-ms-model-mesh-model-id header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "chatcmpl-123",
			"choices": [{
				"message": {"role": "assistant", "content": "Hello from GitHub Models"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11}
		}`))
	}))
	defer server.Close()

	p := NewGitHubModelsProvider("test-token")
	p.client = server.Client()

	origURL := githubModelsBaseURL
	defer func() { githubModelsBaseURL = origURL }()
	githubModelsBaseURL = server.URL

	resp, err := p.Chat(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello from GitHub Models" {
		t.Errorf("expected 'Hello from GitHub Models', got %s", resp.Content)
	}
}

func TestGitHubModelsProvider_Chat_401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"message": "token expired", "type": "auth_error"}}`))
	}))
	defer server.Close()

	p := NewGitHubModelsProvider("bad-token")
	p.client = server.Client()

	origURL := githubModelsBaseURL
	defer func() { githubModelsBaseURL = origURL }()
	githubModelsBaseURL = server.URL

	_, err := p.Chat(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != ErrApiKeyInvalid {
		t.Errorf("expected ErrApiKeyInvalid, got %v", err)
	}
}
