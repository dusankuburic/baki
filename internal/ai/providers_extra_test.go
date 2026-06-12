package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- ClaudeProvider: identity methods not yet covered ----------------------

func TestClaudeProvider_ContextLimit(t *testing.T) {
	p := NewClaudeProvider("key")
	if p.ContextLimit() != 200000 {
		t.Errorf("ContextLimit() = %d, want 200000", p.ContextLimit())
	}
}

func TestClaudeProvider_FreeModel(t *testing.T) {
	p := NewClaudeProvider("key")
	if p.FreeModel() != "" {
		t.Errorf("FreeModel() = %q, want empty", p.FreeModel())
	}
}

func TestClaudeProvider_PricePerMillionTokens(t *testing.T) {
	p := NewClaudeProvider("key")
	pricing := p.PricePerMillionTokens()
	if pricing.InputCostPerM <= 0 || pricing.OutputCostPerM <= 0 {
		t.Errorf("expected positive pricing, got %+v", pricing)
	}
}

func TestClaudeProvider_EstimateTokens(t *testing.T) {
	p := NewClaudeProvider("key")
	if got := p.EstimateTokens("hello world"); got <= 0 {
		t.Errorf("EstimateTokens should return positive value, got %d", got)
	}
}

// ---- OpenAIProvider: all identity methods ----------------------------------

func TestOpenAIProvider_Identity(t *testing.T) {
	p := NewOpenAIProvider("key")
	if p.ID() != "openai" {
		t.Errorf("ID() = %q, want %q", p.ID(), "openai")
	}
	if p.Name() != "OpenAI" {
		t.Errorf("Name() = %q, want %q", p.Name(), "OpenAI")
	}
	if p.ContextLimit() != 128000 {
		t.Errorf("ContextLimit() = %d, want 128000", p.ContextLimit())
	}
	if p.DefaultModel() != "gpt-4o" {
		t.Errorf("DefaultModel() = %q, want %q", p.DefaultModel(), "gpt-4o")
	}
	if p.FreeModel() != "" {
		t.Errorf("FreeModel() = %q, want empty", p.FreeModel())
	}
	pricing := p.PricePerMillionTokens()
	if pricing.InputCostPerM <= 0 {
		t.Errorf("expected positive input pricing, got %v", pricing.InputCostPerM)
	}
	if got := p.EstimateTokens("hello"); got <= 0 {
		t.Errorf("EstimateTokens returned %d, want > 0", got)
	}
}

func TestOpenAIProvider_Models_NonEmpty(t *testing.T) {
	p := NewOpenAIProvider("key")
	models, err := p.Models(context.Background())
	if err != nil {
		t.Errorf("Models() error: %v", err)
	} else if len(models) == 0 {
		t.Error("Models() must return at least one model")
	}
	for _, m := range models {
		if m.ID == "" {
			t.Error("model with empty ID")
		}
	}
}

// ---- GeminiProvider: identity + stream + defaultGeminiURL ------------------

func TestGeminiProvider_Identity(t *testing.T) {
	p := NewGeminiProvider("key")
	if p.ID() != "gemini" {
		t.Errorf("ID() = %q", p.ID())
	}
	if p.Name() != "Gemini" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.ContextLimit() == 0 {
		t.Error("ContextLimit() must be non-zero")
	}
	if p.FreeModel() != "" {
		t.Errorf("FreeModel() = %q, want empty", p.FreeModel())
	}
	pricing := p.PricePerMillionTokens()
	if pricing.InputCostPerM <= 0 {
		t.Errorf("expected positive input pricing: %v", pricing)
	}
	if got := p.EstimateTokens("test"); got <= 0 {
		t.Errorf("EstimateTokens = %d, want > 0", got)
	}
}

func TestDefaultGeminiURL_GenerateContent(t *testing.T) {
	url := defaultGeminiURL("gemini-2.5-pro", "my-key", "generateContent")
	if !strings.Contains(url, "gemini-2.5-pro") {
		t.Errorf("URL should contain model name: %q", url)
	}
	// The API key must NOT appear in the URL — it is sent via the
	// x-goog-api-key header so it never leaks into proxy/access logs.
	if strings.Contains(url, "my-key") || strings.Contains(url, "key=") {
		t.Errorf("URL must not contain the API key: %q", url)
	}
	if strings.Contains(url, "alt=sse") {
		t.Errorf("non-stream URL should not contain alt=sse: %q", url)
	}
}

func TestDefaultGeminiURL_StreamGenerateContent(t *testing.T) {
	url := defaultGeminiURL("gemini-2.5-flash", "my-key", "streamGenerateContent")
	if !strings.Contains(url, "alt=sse") {
		t.Errorf("stream URL should contain alt=sse: %q", url)
	}
}

func TestGeminiProvider_Stream_Success(t *testing.T) {
	// Gemini SSE sends JSON objects as data lines.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}` + "\n\n"))
		// Note: the second data line was previously malformed (`{"text":" Gemini"]}]` —
		// missing `}` after the string). The old parser silently skipped the
		// malformed line and relied on the EOF-without-DONE fallback to emit a
		// Done chunk. After the truncation fix, that fallback returns an error
		// instead, exposing the broken fixture. JSON is now well-formed and
		// the STOP finishReason serves as the terminal marker.
		w.Write([]byte(`data: {"candidates":[{"content":{"parts":[{"text":" Gemini"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2}}` + "\n\n"))
	}))
	defer server.Close()

	p := NewGeminiProvider("test-key")
	p.client = server.Client()

	origGeminiURL := geminiURL
	defer func() { geminiURL = origGeminiURL }()
	geminiURL = func(model, apiKey, action string) string { return server.URL }

	var chunks []string
	var done bool
	err := p.Stream(context.Background(), Request{
		Model:    "gemini-2.5-flash",
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
	if len(chunks) == 0 {
		t.Error("expected at least one text chunk")
	}
}

func TestGeminiProvider_Stream_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"code":500,"message":"internal error","status":"INTERNAL"}}`))
	}))
	defer server.Close()

	p := NewGeminiProvider("test-key")
	p.client = server.Client()

	origGeminiURL := geminiURL
	defer func() { geminiURL = origGeminiURL }()
	geminiURL = func(model, apiKey, action string) string { return server.URL }

	err := p.Stream(context.Background(), Request{
		Model:    "gemini-2.5-flash",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(Chunk) {})
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

// ---- XAIProvider: all identity + Chat/Stream via httptest ------------------

func TestXAIProvider_Identity(t *testing.T) {
	p := NewXAIProvider("key")
	if p.ID() != "xai" {
		t.Errorf("ID() = %q", p.ID())
	}
	if p.Name() != "xAI (Grok)" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.ContextLimit() == 0 {
		t.Error("ContextLimit() must be non-zero")
	}
	if p.DefaultModel() == "" {
		t.Error("DefaultModel() must not be empty")
	}
	if p.FreeModel() != "" {
		t.Errorf("FreeModel() = %q, want empty", p.FreeModel())
	}
	pricing := p.PricePerMillionTokens()
	if pricing.InputCostPerM <= 0 {
		t.Errorf("expected positive input pricing: %v", pricing)
	}
	if got := p.EstimateTokens("test"); got <= 0 {
		t.Errorf("EstimateTokens = %d, want > 0", got)
	}
}

func TestXAIProvider_Models_NonEmpty(t *testing.T) {
	p := NewXAIProvider("key")
	models, err := p.Models(context.Background())
	if err != nil {
		t.Errorf("Models() error: %v", err)
	} else if len(models) == 0 {
		t.Error("Models() must return at least one model")
	}
}

func TestXAIProvider_Chat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "Hello from Grok"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
		}`))
	}))
	defer server.Close()

	p := NewXAIProvider("test-key")
	p.client = server.Client()

	origURL := xaiBaseURL
	defer func() { xaiBaseURL = origURL }()
	xaiBaseURL = server.URL

	resp, err := p.Chat(context.Background(), Request{
		Model:    "grok-3-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello from Grok" {
		t.Errorf("content = %q, want %q", resp.Content, "Hello from Grok")
	}
}

func TestXAIProvider_Chat_401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"message": "invalid key", "type": "auth_error"}}`))
	}))
	defer server.Close()

	p := NewXAIProvider("bad-key")
	p.client = server.Client()

	origURL := xaiBaseURL
	defer func() { xaiBaseURL = origURL }()
	xaiBaseURL = server.URL

	_, err := p.Chat(context.Background(), Request{
		Model:    "grok-3-mini",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != ErrApiKeyInvalid {
		t.Errorf("expected ErrApiKeyInvalid, got %v", err)
	}
}

func TestXAIProvider_Stream_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"Hello\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \" Grok\"}, \"finish_reason\": \"stop\"}], \"usage\": {\"completion_tokens\": 2}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewXAIProvider("test-key")
	p.client = server.Client()

	origURL := xaiBaseURL
	defer func() { xaiBaseURL = origURL }()
	xaiBaseURL = server.URL

	var text strings.Builder
	var done bool
	err := p.Stream(context.Background(), Request{
		Model:    "grok-3-mini",
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
	if text.String() != "Hello Grok" {
		t.Errorf("text = %q, want %q", text.String(), "Hello Grok")
	}
}

// ---- GLMProvider: all identity + Chat/Stream via httptest ------------------

func TestGLMProvider_Identity(t *testing.T) {
	p := NewGLMProvider("key")
	if p.ID() != "glm" {
		t.Errorf("ID() = %q", p.ID())
	}
	if p.Name() != "GLM (z.ai)" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.ContextLimit() == 0 {
		t.Error("ContextLimit() must be non-zero")
	}
	if p.DefaultModel() == "" {
		t.Error("DefaultModel() must not be empty")
	}
	if p.FreeModel() == "" {
		t.Error("GLM should have a free model")
	}
	pricing := p.PricePerMillionTokens()
	if pricing.InputCostPerM <= 0 {
		t.Errorf("expected positive input pricing: %v", pricing)
	}
	if got := p.EstimateTokens("test"); got <= 0 {
		t.Errorf("EstimateTokens = %d, want > 0", got)
	}
}

func TestGLMProvider_Models_NonEmpty(t *testing.T) {
	p := NewGLMProvider("key")
	models, err := p.Models(context.Background())
	if err != nil {
		t.Errorf("Models() error: %v", err)
	} else if len(models) == 0 {
		t.Error("Models() must return at least one model")
	}
}

func TestGLMProvider_Chat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "Hello from GLM"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7}
		}`))
	}))
	defer server.Close()

	p := NewGLMProvider("test-key")
	p.client = server.Client()

	origURL := glmBaseURL
	defer func() { glmBaseURL = origURL }()
	glmBaseURL = server.URL

	resp, err := p.Chat(context.Background(), Request{
		Model:    "glm-5.1",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello from GLM" {
		t.Errorf("content = %q, want %q", resp.Content, "Hello from GLM")
	}
}

func TestGLMProvider_Chat_401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"message": "invalid key", "type": "auth_error"}}`))
	}))
	defer server.Close()

	p := NewGLMProvider("bad-key")
	p.client = server.Client()

	origURL := glmBaseURL
	defer func() { glmBaseURL = origURL }()
	glmBaseURL = server.URL

	_, err := p.Chat(context.Background(), Request{
		Model:    "glm-5.1",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != ErrApiKeyInvalid {
		t.Errorf("expected ErrApiKeyInvalid, got %v", err)
	}
}

func TestGLMProvider_Stream_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"Hi\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \" GLM\"}, \"finish_reason\": \"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewGLMProvider("test-key")
	p.client = server.Client()

	origURL := glmBaseURL
	defer func() { glmBaseURL = origURL }()
	glmBaseURL = server.URL

	var text strings.Builder
	var done bool
	err := p.Stream(context.Background(), Request{
		Model:    "glm-5.1",
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
	if text.String() != "Hi GLM" {
		t.Errorf("text = %q, want %q", text.String(), "Hi GLM")
	}
}

// ---- GitHubModelsProvider: remaining identity methods ----------------------

func TestGitHubModelsProvider_Identity(t *testing.T) {
	p := NewGitHubModelsProvider("token")
	if p.ContextLimit() != 8192 {
		t.Errorf("ContextLimit() = %d, want 8192 (GitHub Models free-tier request cap)", p.ContextLimit())
	}
	if p.DefaultModel() != "gpt-4o" {
		t.Errorf("DefaultModel() = %q, want gpt-4o", p.DefaultModel())
	}
	if p.FreeModel() != "" {
		t.Errorf("FreeModel() = %q, want empty", p.FreeModel())
	}
	pricing := p.PricePerMillionTokens()
	if pricing.InputCostPerM != 0 || pricing.OutputCostPerM != 0 {
		t.Errorf("GitHub Models should be free (0 cost), got %+v", pricing)
	}
	if got := p.EstimateTokens("test"); got <= 0 {
		t.Errorf("EstimateTokens = %d, want > 0", got)
	}
}

func TestGitHubModelsProvider_Models_NonEmpty(t *testing.T) {
	p := NewGitHubModelsProvider("token")
	models, err := p.Models(context.Background())
	if err != nil {
		t.Errorf("Models() error: %v", err)
	} else if len(models) == 0 {
		t.Error("Models() must return at least one model")
	}
}
