package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCopilotProvider_ID(t *testing.T) {
	p := NewCopilotProvider("test-token")
	if p.ID() != "copilot" {
		t.Errorf("ID() = %q, want copilot", p.ID())
	}
	if p.Name() != "GitHub Copilot" {
		t.Errorf("Name() = %q, want GitHub Copilot", p.Name())
	}
	if p.DefaultModel() != "gpt-4o" {
		t.Errorf("DefaultModel() = %q, want gpt-4o", p.DefaultModel())
	}
	if p.ContextLimit() != 128000 {
		t.Errorf("ContextLimit() = %d, want 128000", p.ContextLimit())
	}
	if p.FreeModel() != "" {
		t.Errorf("FreeModel() = %q, want empty (Copilot subscription covers all models)", p.FreeModel())
	}
}

func TestCopilotProvider_Models(t *testing.T) {
	// 1. Valid fetch (dynamic)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"gpt-4o","name":"GPT-4o","context_limit":128000},{"id":"gpt-4o-mini","name":"GPT-4o Mini","context_limit":128000},{"id":"claude-3.5-sonnet","name":"Claude 3.5 Sonnet","context_limit":200000}]}`))
	}))
	defer server.Close()

	orig := copilotModelsURL
	defer func() { copilotModelsURL = orig }()
	copilotModelsURL = server.URL + "/models"

	p := NewCopilotProvider("test-token")
	p.client = server.Client()

	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	ids := make(map[string]bool)
	for _, m := range models {
		ids[m.ID] = true
	}
	for _, want := range []string{"gpt-4o", "gpt-4o-mini", "claude-3.5-sonnet"} {
		if !ids[want] {
			t.Errorf("missing expected model %q", want)
		}
	}

	// 2. Cache hit (server should NOT be called again)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not have been called due to cache")
	})
	models2, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models2) != 3 {
		t.Fatal("cached results lost")
	}
}

func TestCopilotProvider_Pricing_IsZero(t *testing.T) {
	p := NewCopilotProvider("test-token")
	pricing := p.PricePerMillionTokens()
	if pricing.InputCostPerM != 0 || pricing.OutputCostPerM != 0 {
		t.Errorf("Copilot pricing should be zero (subscription-based), got %+v", pricing)
	}
	// We can't easily test p.Models() here without a mock server or pre-filling the cache
}

// --- Chat ---

func TestCopilotProvider_Chat_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Copilot-specific required headers
		if r.Header.Get("Authorization") != "Bearer cop-session-tok" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Copilot-Integration-Id") != "vscode-chat" {
			t.Errorf("Copilot-Integration-Id = %q, want vscode-chat", r.Header.Get("Copilot-Integration-Id"))
		}
		if r.Header.Get("Editor-Version") == "" {
			t.Error("Editor-Version header missing")
		}
		if r.Header.Get("Editor-Plugin-Version") == "" {
			t.Error("Editor-Plugin-Version header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "chatcmpl-abc",
			"choices": [{"message": {"role": "assistant", "content": "Hello Copilot"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 3}
		}`))
	}))
	defer server.Close()

	orig := copilotBaseURL
	defer func() { copilotBaseURL = orig }()
	copilotBaseURL = server.URL

	p := NewCopilotProvider("cop-session-tok")
	p.client = server.Client()

	resp, err := p.Chat(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello Copilot" {
		t.Errorf("Content = %q, want Hello Copilot", resp.Content)
	}
	if resp.TokensIn != 10 {
		t.Errorf("TokensIn = %d, want 10", resp.TokensIn)
	}
	if resp.TokensOut != 3 {
		t.Errorf("TokensOut = %d, want 3", resp.TokensOut)
	}
}

func TestCopilotProvider_Chat_401_ReturnsErrApiKeyInvalid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"message": "token expired", "type": "auth_error"}}`))
	}))
	defer server.Close()

	orig := copilotBaseURL
	defer func() { copilotBaseURL = orig }()
	copilotBaseURL = server.URL

	p := NewCopilotProvider("expired-tok")
	p.client = server.Client()

	_, err := p.Chat(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("expected ErrApiKeyInvalid, got %v", err)
	}
}

func TestCopilotProvider_Chat_429_ReturnsErrRateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error": {"message": "rate limited", "type": "rate_limit"}}`))
	}))
	defer server.Close()

	orig := copilotBaseURL
	defer func() { copilotBaseURL = orig }()
	copilotBaseURL = server.URL

	p := NewCopilotProvider("valid-tok")
	p.client = server.Client()

	_, err := p.Chat(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
}

func TestCopilotProvider_Chat_500(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error": {"message": "internal error", "type": "server_error"}}`))
	}))
	defer server.Close()

	orig := copilotBaseURL
	defer func() { copilotBaseURL = orig }()
	copilotBaseURL = server.URL

	p := NewCopilotProvider("tok")
	p.client = server.Client()

	_, err := p.Chat(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Error("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "internal error") {
		t.Errorf("error should contain server message, got: %v", err)
	}
}

func TestCopilotProvider_Chat_TokenFnError(t *testing.T) {
	tokenErr := fmt.Errorf("session token exchange failed: no subscription")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP server should not be called when tokenFn fails")
	}))
	defer server.Close()

	orig := copilotBaseURL
	defer func() { copilotBaseURL = orig }()
	copilotBaseURL = server.URL

	p := &CopilotProvider{
		tokenFn: func(_ context.Context) (string, error) { return "", tokenErr },
		client:  server.Client(),
	}

	_, err := p.Chat(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error when tokenFn fails")
	}
	if !strings.Contains(err.Error(), tokenErr.Error()) {
		t.Errorf("error should wrap tokenFn error; got: %v", err)
	}
}

// --- Stream ---

func TestCopilotProvider_Stream_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"Hello\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \" Copilot\"}, \"finish_reason\": \"stop\"}], \"usage\": {\"completion_tokens\": 2}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	orig := copilotBaseURL
	defer func() { copilotBaseURL = orig }()
	copilotBaseURL = server.URL

	p := NewCopilotProvider("session-tok")
	p.client = server.Client()

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
	if len(chunks) != 2 || chunks[0] != "Hello" || chunks[1] != " Copilot" {
		t.Errorf("expected [Hello, \" Copilot\"], got %v", chunks)
	}
}

func TestCopilotProvider_Stream_TokenFnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when tokenFn fails")
	}))
	defer server.Close()

	orig := copilotBaseURL
	defer func() { copilotBaseURL = orig }()
	copilotBaseURL = server.URL

	p := &CopilotProvider{
		tokenFn: func(_ context.Context) (string, error) { return "", fmt.Errorf("auth error") },
		client:  server.Client(),
	}

	err := p.Stream(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(Chunk) {})
	if err == nil {
		t.Fatal("expected error when tokenFn fails")
	}
}

func TestCopilotProvider_Stream_401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error": {"message": "unauthorized", "type": "auth"}}`))
	}))
	defer server.Close()

	orig := copilotBaseURL
	defer func() { copilotBaseURL = orig }()
	copilotBaseURL = server.URL

	p := NewCopilotProvider("bad-tok")
	p.client = server.Client()

	err := p.Stream(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(Chunk) {})
	if err == nil {
		t.Error("expected error for 401 stream response")
	}
	if !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("expected ErrApiKeyInvalid, got: %v", err)
	}
}

// --- NewCopilotProviderWithAuth integration ---

func TestCopilotProvider_WithAuth_UsesSessionToken(t *testing.T) {
	// exchange server returns a session token
	exchangeCallCount := 0
	exchangeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchangeCallCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CopilotSessionToken{
			Token:     "dyn-session-tok",
			ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
		})
	}))
	defer exchangeServer.Close()

	// chat server verifies the session token is forwarded
	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dyn-session-tok" {
			t.Errorf("Authorization = %q, want Bearer dyn-session-tok", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}], "usage": {}}`))
	}))
	defer chatServer.Close()

	origExchange := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = origExchange }()
	copilotTokenExchangeURL = exchangeServer.URL

	origBase := copilotBaseURL
	defer func() { copilotBaseURL = origBase }()
	copilotBaseURL = chatServer.URL

	auth := &CopilotAuth{client: &http.Client{}}
	p := NewCopilotProviderWithAuth(auth, "gh-oauth-tok")
	p.client = &http.Client{}

	_, err := p.Chat(context.Background(), Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exchangeCallCount != 1 {
		t.Errorf("expected 1 token exchange, got %d", exchangeCallCount)
	}
}

func TestCopilotProvider_WithAuth_CachesSessionToken(t *testing.T) {
	// Confirms that multiple Chat calls reuse the cached session token (exchange called only once).
	exchangeCallCount := 0
	exchangeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchangeCallCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CopilotSessionToken{
			Token:     "cached-dyn-tok",
			ExpiresAt: time.Now().Add(2 * time.Hour).Unix(),
		})
	}))
	defer exchangeServer.Close()

	chatCallCount := 0
	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCallCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices": [{"message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}], "usage": {}}`))
	}))
	defer chatServer.Close()

	origExchange := copilotTokenExchangeURL
	defer func() { copilotTokenExchangeURL = origExchange }()
	copilotTokenExchangeURL = exchangeServer.URL

	origBase := copilotBaseURL
	defer func() { copilotBaseURL = origBase }()
	copilotBaseURL = chatServer.URL

	auth := &CopilotAuth{client: &http.Client{}}
	p := NewCopilotProviderWithAuth(auth, "gh-oauth-tok")
	p.client = &http.Client{}

	for i := 0; i < 3; i++ {
		if _, err := p.Chat(context.Background(), Request{
			Model:    "gpt-4o",
			Messages: []Message{{Role: "user", Content: "Hi"}},
		}); err != nil {
			t.Fatalf("call %d failed: %v", i+1, err)
		}
	}

	if exchangeCallCount != 1 {
		t.Errorf("expected 1 token exchange for 3 chat calls, got %d", exchangeCallCount)
	}
	if chatCallCount != 3 {
		t.Errorf("expected 3 chat calls, got %d", chatCallCount)
	}
}
