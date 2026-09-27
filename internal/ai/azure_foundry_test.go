package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// azureTestServer records every request (method, path, headers, decoded JSON
// body) and answers with the handler's response.
type azureTestServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []azureSeenRequest
}

type azureSeenRequest struct {
	Path    string
	Headers http.Header
	Body    map[string]any
}

func newAzureTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int, body map[string]any)) *azureTestServer {
	t.Helper()
	s := &azureTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, azureSeenRequest{Path: r.URL.Path, Headers: r.Header.Clone(), Body: body})
		n := len(s.reqs)
		s.mu.Unlock()
		handler(w, r, n, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *azureTestServer) requests() []azureSeenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]azureSeenRequest, len(s.reqs))
	copy(out, s.reqs)
	return out
}

func azureKeyConfig(endpoint string) AzureFoundryConfig {
	return AzureFoundryConfig{Endpoint: endpoint, Auth: AzureFoundryAuth{Mode: AzureAuthModeAPIKey, Key: "secret-key"}}
}

// newAzureKeyProvider builds an API-key provider against a local server
// (validation is bypassed: httptest hosts are loopback).
func newAzureKeyProvider(t *testing.T, srv *azureTestServer, mutate func(*AzureFoundryConfig)) *AzureFoundryProvider {
	t.Helper()
	ResetAzureFoundryCaches()
	cfg := azureKeyConfig(srv.URL)
	if mutate != nil {
		mutate(&cfg)
	}
	return newAzureFoundryProvider(cfg, nil, srv.Client())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func chatCompletionJSON(text, model string) map[string]any {
	return map[string]any{
		"id":    "chatcmpl-1",
		"model": model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}
}

func TestAzureFoundry_ChatRequestShape(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 200, chatCompletionJSON("hello", "gpt-4o-2024-11-20"))
	})
	p := newAzureKeyProvider(t, srv, nil)

	resp, err := p.Chat(context.Background(), Request{Model: "my-gpt4o", Temperature: 0.3, MaxTokens: 256,
		Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hello" || resp.ServedModel != "gpt-4o-2024-11-20" || resp.TokensIn != 10 || resp.TokensOut != 5 {
		t.Errorf("response = %+v", resp)
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests", len(reqs))
	}
	got := reqs[0]
	if got.Path != "/openai/v1/chat/completions" {
		t.Errorf("path = %q", got.Path)
	}
	if got.Headers.Get("api-key") != "secret-key" || got.Headers.Get("Authorization") != "Bearer secret-key" {
		t.Errorf("auth headers = %v", got.Headers)
	}
	if got.Body["model"] != "my-gpt4o" {
		t.Errorf("model = %v, want the deployment name", got.Body["model"])
	}
	if _, has := got.Body["max_tokens"]; has {
		t.Error("max_tokens must not be sent to Azure (reasoning deployments reject it)")
	}
	if got.Body["max_completion_tokens"] != float64(256) {
		t.Errorf("max_completion_tokens = %v", got.Body["max_completion_tokens"])
	}
}

const azureStreamFixture = `data: {"id":"x","model":"gpt-4o-2024-11-20","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search_flow","arguments":"{\"q\":"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`

func TestAzureFoundry_StreamCarriesServedModelAndTools(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(azureStreamFixture))
	})
	p := newAzureKeyProvider(t, srv, nil)

	var text strings.Builder
	var done Chunk
	err := p.Stream(context.Background(), Request{Model: "my-gpt4o", Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []ToolDefinition{{Name: "search_flow", InputSchema: []byte(`{"type":"object"}`)}}}, func(c Chunk) {
		text.WriteString(c.Text)
		if c.Done {
			done = c
		}
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if text.String() != "Hello" {
		t.Errorf("text = %q", text.String())
	}
	if done.ServedModel != "gpt-4o-2024-11-20" || done.TokensIn != 10 || done.TokensOut != 5 || done.FinishReason != "tool_calls" {
		t.Errorf("done chunk = %+v", done)
	}
	if len(done.ToolCalls) != 1 || done.ToolCalls[0].ID != "call_1" || done.ToolCalls[0].Name != "search_flow" || string(done.ToolCalls[0].Input) != `{"q":"x"}` {
		t.Errorf("tool calls = %+v", done.ToolCalls)
	}
	if body := srv.requests()[0].Body; body["stream"] != true || body["tools"] == nil {
		t.Errorf("stream request body = %v", body)
	}
}

func TestAzureFoundry_RepairsUnsupportedTemperature(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if _, has := body["temperature"]; has {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"message": "Unsupported value: 'temperature' does not support 0.3 with this model. Only the default (1) value is supported.",
				"type":    "invalid_request_error", "param": "temperature", "code": "unsupported_value"}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "o4-mini-2025-04-16"))
	})
	p := newAzureKeyProvider(t, srv, nil)
	resp, err := p.Chat(context.Background(), Request{Model: "reasoning", Temperature: 0.3, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want exactly one repair round trip", len(reqs))
	}
	if _, has := reqs[1].Body["temperature"]; has {
		t.Error("second request still carries temperature")
	}
}

func TestAzureFoundry_SwapsMaxTokensParam(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if _, has := body["max_completion_tokens"]; has {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"message": "Unrecognized request argument supplied: max_completion_tokens", "type": "invalid_request_error"}})
			return
		}
		if body["max_tokens"] != float64(128) {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "max_tokens missing after swap", "code": "bad"}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "legacy"))
	})
	p := newAzureKeyProvider(t, srv, nil)
	if _, err := p.Chat(context.Background(), Request{Model: "legacy", MaxTokens: 128, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Errorf("got %d requests, want 2", n)
	}
}

func TestAzureFoundry_DoesNotRepairLoadBearingParams(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{
			"message": "Unsupported parameter: 'messages' is not supported", "param": "messages", "code": "unsupported_parameter"}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	_, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected the 400 to surface")
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("a rejection naming a required field must not be retried; got %d requests", n)
	}
}

func TestAzureFoundry_ContentFilterOnPrompt(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{
			"message":    "The response was filtered due to the prompt triggering Azure OpenAI's content management policy.",
			"type":       nil,
			"code":       "content_filter",
			"innererror": map[string]any{"code": "ResponsibleAIPolicyViolation"}}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	req := Request{Model: "m", Messages: []Message{{Role: "user", Content: "bad"}}}
	if _, err := p.Chat(context.Background(), req); !errors.Is(err, ErrContentFiltered) {
		t.Errorf("Chat: want ErrContentFiltered, got %v", err)
	}
	if err := p.Stream(context.Background(), req, func(Chunk) {}); !errors.Is(err, ErrContentFiltered) {
		t.Errorf("Stream: want ErrContentFiltered, got %v", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Errorf("content filter rejections must not be retried/repaired: %d requests", n)
	}
}

func TestParseOpenAISSE_ContentFilterFinishIsAnError(t *testing.T) {
	body := "data: {\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n" +
		"data: [DONE]\n\n"
	var chunks []Chunk
	err := parseOpenAISSE(strings.NewReader(body), func(c Chunk) { chunks = append(chunks, c) })
	if err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if len(chunks) != 2 || chunks[0].Text != "partial" || !errors.Is(chunks[1].Error, ErrContentFiltered) {
		t.Errorf("chunks = %+v", chunks)
	}
	for _, c := range chunks {
		if c.Done {
			t.Error("a filtered completion must not end with a clean Done chunk")
		}
	}
}

func TestAzureFoundry_ChatContentFilterFinish(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		resp := chatCompletionJSON("partial", "gpt-4o")
		resp["choices"].([]map[string]any)[0]["finish_reason"] = "content_filter"
		writeJSON(w, 200, resp)
	})
	p := newAzureKeyProvider(t, srv, nil)
	if _, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrContentFiltered) {
		t.Errorf("want ErrContentFiltered, got %v", err)
	}
}

func TestAzureFoundry_401IsInvalidKey(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "Access denied due to invalid subscription key", "code": "401"}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	if _, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("want ErrApiKeyInvalid, got %v", err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("API-key mode must not retry a 401: %d requests", n)
	}
}

func TestAzureFoundry_EntraRetriesOnceWithFreshToken(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "token rejected", "code": "401"}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "gpt-4o"))
	})
	ResetAzureFoundryCaches()
	cfg := AzureFoundryConfig{Endpoint: srv.URL, Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra,
		TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}}
	p := newAzureFoundryProvider(cfg, nil, srv.Client())
	cred := &fakeCred{}
	p.tokens = newEntraTokenSource(cred)
	p.tokenFn = p.entraToken

	resp, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	reqs := srv.requests()
	if len(reqs) != 2 || reqs[0].Headers.Get("Authorization") != "Bearer tok-1" || reqs[1].Headers.Get("Authorization") != "Bearer tok-2" {
		t.Errorf("requests = %+v", reqs)
	}
	if reqs[0].Headers.Get("api-key") != "" {
		t.Error("entra mode must not send an api-key header")
	}

	// A rejection that survives the fresh token is final (no loop).
	srv2 := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "no role", "code": "401"}})
	})
	cfg.Endpoint = srv2.URL
	p2 := newAzureFoundryProvider(cfg, nil, srv2.Client())
	p2.tokens = newEntraTokenSource(&fakeCred{})
	p2.tokenFn = p2.entraToken
	if _, err := p2.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("want ErrApiKeyInvalid, got %v", err)
	}
	if n := len(srv2.requests()); n != 2 {
		t.Errorf("want exactly one retry, got %d requests", n)
	}
}

func TestParseRetryAfter_PrefersMilliseconds(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "2")
	resp.Header.Set("retry-after-ms", "1500")
	if d := parseRetryAfter(resp); d != 1500*time.Millisecond {
		t.Errorf("parseRetryAfter = %v, want 1.5s", d)
	}
	resp.Header.Del("retry-after-ms")
	if d := parseRetryAfter(resp); d != 2*time.Second {
		t.Errorf("parseRetryAfter without ms = %v, want 2s", d)
	}
}

func TestAzureFoundry_429CarriesRetryAfterMs(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("retry-after-ms", "1500")
		writeJSON(w, 429, map[string]any{"error": map[string]any{"message": "Requests to the ChatCompletions_Create Operation have exceeded token rate limit", "code": "429"}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	_, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	var rle *RateLimitError
	if !errors.As(err, &rle) || rle.RetryAfter != 1500*time.Millisecond {
		t.Errorf("want RateLimitError{1.5s}, got %v", err)
	}
}

func TestAzureFoundry_ModelsListsDeploymentsAndCaches(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if r.URL.Path != "/openai/v1/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "my-gpt4o", "object": "model", "owned_by": "openai"},
			{"id": "text-embed", "object": "model", "owned_by": "openai"},
			{"id": "my-gpt4o", "object": "model"},
		}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "my-gpt4o" || models[1].ID != "text-embed" || models[0].ContextLimit <= 0 {
		t.Errorf("models = %+v", models)
	}
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("second listing within the TTL must hit the cache: %d requests", n)
	}
	if got := srv.requests()[0].Headers.Get("api-key"); got != "secret-key" {
		t.Errorf("listing not authenticated: api-key=%q", got)
	}
}

func TestAzureFoundry_ModelsFallbackAndErrors(t *testing.T) {
	forbidden := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 403, map[string]any{"error": map[string]any{"message": "forbidden"}})
	})
	p := newAzureKeyProvider(t, forbidden, func(c *AzureFoundryConfig) { c.ChatDeployment = "chat1"; c.EmbeddingDeployment = "emb1" })
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "chat1" || models[1].ID != "emb1" {
		t.Errorf("fallback listing = %+v, want the configured deployments", models)
	}

	unauthorized := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "bad key"}})
	})
	p = newAzureKeyProvider(t, unauthorized, nil)
	if _, err := p.Models(context.Background()); !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("want ErrApiKeyInvalid, got %v", err)
	}
}

func TestAzureFoundry_EmbedSendsDimensionsForTextEmbedding3(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if r.URL.Path != "/openai/v1/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		inputs, _ := body["input"].([]any)
		out := []map[string]any{}
		for range inputs {
			out = append(out, map[string]any{"embedding": []float32{0.1}})
		}
		writeJSON(w, 200, map[string]any{"data": out})
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.EmbeddingDeployment = "text-embedding-3-large" })
	p.setEmbeddingDimensions(1536)
	if _, err := p.Embed(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	body := srv.requests()[0].Body
	if body["model"] != "text-embedding-3-large" || body["dimensions"] != float64(1536) {
		t.Errorf("embeddings body = %v", body)
	}

	// The document's deployment wins over the deployment-wide override; a
	// document without one takes the override.
	p.setEmbeddingModel("other")
	if p.EmbeddingModel() != "text-embedding-3-large" {
		t.Errorf("override replaced the document's deployment: %q", p.EmbeddingModel())
	}
	q := newAzureKeyProvider(t, srv, nil)
	if _, err := q.Embed(context.Background(), []string{"a"}); err == nil {
		t.Error("no embedding deployment: Embed must fail with guidance")
	}
	q.setEmbeddingModel("ada")
	if q.EmbeddingModel() != "ada" {
		t.Errorf("override not applied when the document has no deployment: %q", q.EmbeddingModel())
	}
}

func TestNewAzureFoundryProvider_ConfigStates(t *testing.T) {
	ResetAzureFoundryCaches()
	unconfigured := NewAzureFoundryProvider("")
	models, err := unconfigured.Models(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("unconfigured Models = %v, %v", models, err)
	}
	if unconfigured.DefaultModel() != "gpt-4.1" || unconfigured.ID() != "azure-foundry" || !unconfigured.SupportsTools() {
		t.Errorf("metadata provider = %+v", unconfigured)
	}

	broken := NewAzureFoundryProvider("not json")
	if _, err := broken.Chat(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("broken document Chat: %v", err)
	}
	if err := broken.Stream(context.Background(), Request{Model: "m"}, func(Chunk) {}); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("broken document Stream: %v", err)
	}
	if _, err := broken.Models(context.Background()); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("broken document Models: %v", err)
	}

	disallowed := NewAzureFoundryProvider(`{"endpoint":"https://evil.example","auth":{"mode":"api_key","key":"k"}}`)
	if _, err := disallowed.Chat(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("disallowed host must never be dialled: %v", err)
	}

	valid := NewAzureFoundryProvider(`{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"chat"}`)
	if valid.configErr != nil || valid.DefaultModel() != "chat" || *valid.baseURL != "https://my-res.openai.azure.com/openai/v1/chat/completions" {
		t.Errorf("valid provider = err %v default %q base %q", valid.configErr, valid.DefaultModel(), *valid.baseURL)
	}
}
