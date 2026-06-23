package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- toOpenAIMessages: system prompt path ----------------------------------

func TestToOpenAIMessages_WithSystemPrompt(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "hello"}}
	out := toOpenAIMessages("You are helpful", msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(out))
	}
	if out[0].Role != "system" {
		t.Errorf("first message should be system, got %q", out[0].Role)
	}
	if out[0].Content != "You are helpful" {
		t.Errorf("system content = %q, want %q", out[0].Content, "You are helpful")
	}
	if out[1].Role != "user" {
		t.Errorf("second message should be user, got %q", out[1].Role)
	}
}

func TestToOpenAIMessages_NoSystemPrompt(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "hi"}}
	out := toOpenAIMessages("", msgs)
	if len(out) != 1 {
		t.Fatalf("expected 1 message without system prompt, got %d", len(out))
	}
}

// ---- toGeminiContents: assistant → "model" role ----------------------------

func TestToGeminiContents_AssistantRole_MappedToModel(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	out := toGeminiContents(msgs)
	if len(out) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(out))
	}
	if out[0].Role != "user" {
		t.Errorf("user role not preserved: %q", out[0].Role)
	}
	if out[1].Role != "model" {
		t.Errorf("assistant role should map to 'model', got %q", out[1].Role)
	}
}

// ---- DemoProvider: FreeModel, Chat, Stream ---------------------------------

func TestDemoProvider_FreeModel(t *testing.T) {
	p := NewDemoProvider()
	if p.FreeModel() != "" {
		t.Errorf("FreeModel() = %q, want empty", p.FreeModel())
	}
}

func TestDemoProvider_Chat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content": "Demo response", "tokensIn": 5, "tokensOut": 3, "finishReason": "stop"}`))
	}))
	defer server.Close()

	origURL := DemoProxyURL
	DemoProxyURL = server.URL
	defer func() { DemoProxyURL = origURL }()

	d := newDemoProvider(server.Client())
	resp, err := d.Chat(context.Background(), Request{
		Model:    "demo",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Demo response" {
		t.Errorf("content = %q, want %q", resp.Content, "Demo response")
	}
}

func TestDemoProvider_Chat_429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error": "too many requests"}`))
	}))
	defer server.Close()

	origURL := DemoProxyURL
	DemoProxyURL = server.URL
	defer func() { DemoProxyURL = origURL }()

	d := newDemoProvider(server.Client())
	_, err := d.Chat(context.Background(), Request{
		Model:    "demo",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
}

func TestDemoProvider_Chat_ErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"error": "service unavailable"}`))
	}))
	defer server.Close()

	origURL := DemoProxyURL
	DemoProxyURL = server.URL
	defer func() { DemoProxyURL = origURL }()

	d := newDemoProvider(server.Client())
	_, err := d.Chat(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Error("expected error for 503 response")
	}
	if !strings.Contains(err.Error(), "service unavailable") {
		t.Errorf("error should contain message, got: %v", err)
	}
}

func TestDemoProvider_Stream_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"Demo\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	origURL := DemoProxyURL
	DemoProxyURL = server.URL
	defer func() { DemoProxyURL = origURL }()

	d := newDemoProvider(server.Client())
	var text strings.Builder
	var done bool
	err := d.Stream(context.Background(), Request{
		Model:    "demo",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk Chunk) {
		if chunk.Done {
			done = true
		} else {
			text.WriteString(chunk.Text)
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("expected done signal")
	}
	if text.String() != "Demo" {
		t.Errorf("text = %q, want %q", text.String(), "Demo")
	}
}

// newDemoProvider creates a DemoProvider with a custom HTTP client for testing.
func newDemoProvider(client *http.Client) *DemoProvider {
	return &DemoProvider{client: client}
}

// ---- GitHubModelsProvider: Stream via httptest -----------------------------

func TestGitHubModelsProvider_Stream_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing Authorization header")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"GitHub\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewGitHubModelsProvider("test-token")
	p.client = server.Client()

	origURL := githubModelsBaseURL
	defer func() { githubModelsBaseURL = origURL }()
	githubModelsBaseURL = server.URL

	var text strings.Builder
	var done bool
	err := p.Stream(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(chunk Chunk) {
		if chunk.Done {
			done = true
		} else {
			text.WriteString(chunk.Text)
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("expected done signal")
	}
	if text.String() != "GitHub" {
		t.Errorf("text = %q, want %q", text.String(), "GitHub")
	}
}

// ---- ClaudeProvider: additional error paths --------------------------------

func TestClaudeProvider_Chat_500(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error": {"type": "api_error", "message": "internal server error"}}`))
	}))
	defer server.Close()

	p := NewClaudeProvider("test-key")
	p.client = server.Client()

	origURL := claudeAPIURL
	defer func() { claudeAPIURL = origURL }()
	claudeAPIURL = server.URL

	_, err := p.Chat(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Error("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Errorf("error should contain server message, got: %v", err)
	}
}

func TestClaudeProvider_Stream_ErrorStatus(t *testing.T) {
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

	err := p.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(Chunk) {})
	if err == nil {
		t.Error("expected error for 429 stream response")
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got: %v", err)
	}
}

// ---- OpenAIProvider: additional error paths --------------------------------

func TestOpenAIProvider_Chat_429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error": {"message": "rate limit exceeded", "type": "requests"}}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key")
	p.client = server.Client()

	origURL := openAIBaseURL
	defer func() { openAIBaseURL = origURL }()
	openAIBaseURL = server.URL

	_, err := p.Chat(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
}

func TestOpenAIProvider_Chat_500(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error": {"message": "server meltdown", "type": "server_error"}}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key")
	p.client = server.Client()

	origURL := openAIBaseURL
	defer func() { openAIBaseURL = origURL }()
	openAIBaseURL = server.URL

	_, err := p.Chat(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestOpenAIProvider_Stream_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"message": "unauthorized", "type": "auth_error"}}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("bad-key")
	p.client = server.Client()

	origURL := openAIBaseURL
	defer func() { openAIBaseURL = origURL }()
	openAIBaseURL = server.URL

	err := p.Stream(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(Chunk) {})
	if err == nil {
		t.Error("expected error for 401 stream response")
	}
	if !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("expected ErrApiKeyInvalid, got: %v", err)
	}
}

// ---- GeminiProvider: additional error paths --------------------------------

func TestGeminiProvider_Chat_400_ApiKeyError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error": {"code": 400, "message": "API key not valid. Please pass a valid API key.", "status": "INVALID_ARGUMENT"}}`))
	}))
	defer server.Close()

	p := NewGeminiProvider("bad-key")
	p.client = server.Client()

	origGeminiURL := geminiURL
	defer func() { geminiURL = origGeminiURL }()
	geminiURL = func(model, apiKey, action string) string { return server.URL }

	_, err := p.Chat(context.Background(), Request{
		Model:    "gemini-2.5-pro",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("expected ErrApiKeyInvalid, got %v", err)
	}
}

func TestGeminiProvider_Chat_429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error": {"code": 429, "message": "quota exceeded", "status": "RESOURCE_EXHAUSTED"}}`))
	}))
	defer server.Close()

	p := NewGeminiProvider("test-key")
	p.client = server.Client()

	origGeminiURL := geminiURL
	defer func() { geminiURL = origGeminiURL }()
	geminiURL = func(model, apiKey, action string) string { return server.URL }

	_, err := p.Chat(context.Background(), Request{
		Model:    "gemini-2.5-pro",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
}

func TestGeminiProvider_Chat_500(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error": {"code": 500, "message": "internal error", "status": "INTERNAL"}}`))
	}))
	defer server.Close()

	p := NewGeminiProvider("test-key")
	p.client = server.Client()

	origGeminiURL := geminiURL
	defer func() { geminiURL = origGeminiURL }()
	geminiURL = func(model, apiKey, action string) string { return server.URL }

	_, err := p.Chat(context.Background(), Request{
		Model:    "gemini-2.5-pro",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Error("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "internal error") {
		t.Errorf("error should contain message, got: %v", err)
	}
}
