package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// F2 (Foundry OpenAI v1 route): non-streaming fallback, the Model Inference
// error envelope, 403 handling, reasoning_content, api-key-only auth, the
// model-router reasoning memo, content-filter usage and explicit temperature.

// Item 10: a model that refuses stream:true is answered non-streaming, the
// reply replayed as chunks, and the deployment remembered.
func TestOpenAICompat_StreamRejectedFallsBackToChat(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if body["stream"] == true {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"type": "invalid_request_error", "param": "stream", "code": "unsupported_value",
				"message": "Unsupported value: 'stream' does not support true with this model. Supported values are: false."}})
			return
		}
		writeJSON(w, 200, map[string]any{
			"id": "c", "model": "o1-2024-12-17",
			"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "content": "Let me look.", "reasoning_content": "thinking",
				"tool_calls": []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "search_flow", "arguments": `{"q":"x"}`}}},
			}}},
			"usage": map[string]any{"prompt_tokens": 9, "completion_tokens": 4},
		})
	})
	p := newAzureKeyProvider(t, srv, nil)
	req := Request{Model: "my-o1", Messages: []Message{{Role: "user", Content: "hi"}}}
	var chunks []Chunk
	if err := p.Stream(context.Background(), req, func(c Chunk) { chunks = append(chunks, c) }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %+v", chunks)
	}
	if c := chunks[0]; c.Text != "Let me look." || c.Reasoning != "thinking" || c.ServedModel != "o1-2024-12-17" {
		t.Errorf("text chunk = %+v", c)
	}
	d := chunks[1]
	if !d.Done || d.TokensIn != 9 || d.TokensOut != 4 || d.ServedModel != "o1-2024-12-17" || d.FinishReason != "tool_calls" ||
		len(d.ToolCalls) != 1 || d.ToolCalls[0].ID != "call_1" || string(d.ToolCalls[0].Input) != `{"q":"x"}` {
		t.Errorf("done chunk = %+v", d)
	}
	if n := len(srv.requests()); n != 2 {
		t.Fatalf("requests = %d, want the rejected stream and the non-streaming retry", n)
	}
	// Remembered for the deployment: straight to the non-streaming request.
	if err := p.Stream(context.Background(), req, func(Chunk) {}); err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 3 || reqs[2].Body["stream"] == true {
		t.Errorf("second turn: %d requests, last stream=%v", len(reqs)-2, reqs[len(reqs)-1].Body["stream"])
	}
}

// Item 10 + 16: a content-filter stop in the non-streaming fallback becomes
// an error chunk carrying its usage, as the streaming parser emits it.
func TestOpenAICompat_StreamFallbackContentFilterCarriesUsage(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if body["stream"] == true {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"param": "stream", "code": "unsupported_value", "message": "no streaming"}})
			return
		}
		writeJSON(w, 200, map[string]any{"model": "o1", "choices": []map[string]any{{"finish_reason": "content_filter", "message": map[string]any{"role": "assistant", "content": "par"}}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	var errs []Chunk
	if err := p.Stream(context.Background(), Request{Model: "o1", Messages: []Message{{Role: "user", Content: "x"}}}, func(c Chunk) {
		if c.Error != nil {
			errs = append(errs, c)
		}
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(errs) != 1 || !errors.Is(errs[0].Error, ErrContentFiltered) || errs[0].TokensIn != 5 || errs[0].TokensOut != 2 || errs[0].ServedModel != "o1" {
		t.Errorf("error chunks = %+v", errs)
	}
}

// Item 11: the Model Inference top-level envelope and the OpenAI envelope
// decode into the same fields.
func TestDecodeOpenAIError_BothShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		body                string
		code, param, substr string
	}{
		"openai":           {`{"error":{"code":"unsupported_parameter","param":"temperature","message":"Unsupported parameter: 'temperature'"}}`, "unsupported_parameter", "temperature", "Unsupported"},
		"inference loc":    {`{"status":422,"code":"parameter_not_supported","detail":{"loc":["body","response_format"],"input":"json_object"},"message":"One of the parameters contain invalid values."}`, "parameter_not_supported", "response_format", "invalid values"},
		"inference filter": {`{"status":400,"code":"content_filter","message":"The response was filtered","param":"messages","type":null}`, "content_filter", "messages", "filtered"},
		"string error":     {`{"error":"UnprocessableContentError","code":"Invalid input","status":422,"message":"Bad input","detail":"'seed' is not allowed"}`, "Invalid input", "", "'seed' is not allowed"},
		"numeric code":     {`{"code":400,"message":"bad"}`, "400", "", "bad"},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := decodeOpenAIError([]byte(tc.body))
			if !ok || got.Error.Code != tc.code || got.Error.Param != tc.param || !strings.Contains(got.Error.Message, tc.substr) {
				t.Errorf("decoded = %+v (ok=%v)", got.Error, ok)
			}
		})
	}
	// API Management's envelope keeps an empty error (the auth hooks read the
	// raw body); non-JSON is not decoded.
	if got, ok := decodeOpenAIError([]byte(`{"statusCode":401,"message":"Unauthorized"}`)); !ok || got.Error.Message != "" {
		t.Errorf("apim envelope = %+v (ok=%v)", got.Error, ok)
	}
	if _, ok := decodeOpenAIError([]byte(`<html>`)); ok {
		t.Error("an HTML body must not decode")
	}
}

// Item 11: a Model Inference 422 naming the parameter in detail.loc is
// repaired; its top-level content_filter is a content-filter block on both
// the chat and the stream path.
func TestAzureFoundry_ModelInferenceEnvelope(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if _, ok := body["temperature"]; ok {
			writeJSON(w, 422, map[string]any{"status": 422, "code": "parameter_not_supported", "message": "One of the parameters contain invalid values.",
				"detail": map[string]any{"loc": []any{"body", "temperature"}, "input": 0.3}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "Mistral-Large-2411"))
	})
	p := newAzureKeyProvider(t, srv, nil)
	resp, err := p.Chat(context.Background(), Request{Model: "mistral", Temperature: 0.3, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil || resp.Content != "ok" {
		t.Fatalf("Chat = %+v, %v", resp, err)
	}
	if reqs := srv.requests(); len(reqs) != 2 {
		t.Errorf("requests = %d, want the rejection and the repaired retry", len(reqs))
	}

	filter := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 400, map[string]any{"status": 400, "code": "content_filter", "message": "The response was filtered", "param": "messages", "type": nil})
	})
	fp := newAzureKeyProvider(t, filter, nil)
	req := Request{Model: "mistral", Messages: []Message{{Role: "user", Content: "x"}}}
	if _, err := fp.Chat(context.Background(), req); !errors.Is(err, ErrContentFiltered) {
		t.Errorf("chat: want ErrContentFiltered, got %v", err)
	}
	if err := fp.Stream(context.Background(), req, func(Chunk) {}); !errors.Is(err, ErrContentFiltered) {
		t.Errorf("stream: want ErrContentFiltered, got %v", err)
	}
}

// Item 12: a 403 that is not PermissionDenied (network rules, a role) keeps
// Azure's message as a permission denial and is never replayed with a fresh
// Entra ID token; API-key mode classifies it the same way.
func TestAzureFoundry_403IsPermissionDeniedWithoutTokenReplay(t *testing.T) {
	const msg = "Access denied due to Virtual Network/Firewall rules."
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		writeJSON(w, 403, map[string]any{"error": map[string]any{"code": "403", "message": msg}})
	})
	ep := newAzureAgentProvider(t, srv) // Entra ID
	req := Request{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "x"}}}
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrProviderPermissionDenied) || errors.Is(err, ErrEntraTokenRejected) || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: want a permission denial carrying Azure's message, got %v", what, err)
		}
		if isRetryable(err) {
			t.Errorf("%s: a 403 must not be retried: %v", what, err)
		}
	}
	_, err := ep.Chat(context.Background(), req)
	check("entra chat", err)
	check("entra stream", ep.Stream(context.Background(), req, func(Chunk) {}))
	if n := len(srv.requests()); n != 2 {
		t.Errorf("requests = %d, want one per call (no fresh-token replay on 403)", n)
	}
	kp := newAzureKeyProvider(t, srv, nil)
	_, err = kp.Chat(context.Background(), req)
	check("api key chat", err)

	empty := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) { w.WriteHeader(403) })
	_, err = newAzureKeyProvider(t, empty, nil).Chat(context.Background(), req)
	if !errors.Is(err, ErrProviderPermissionDenied) || !strings.Contains(err.Error(), "network") {
		t.Errorf("empty 403: want the explanatory fallback, got %v", err)
	}
}

// Item 13: a non-streaming message.reasoning_content is Reasoning, never
// Content.
func TestOpenAICompat_ChatReasoningContent(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"model": "DeepSeek-R1", "choices": []map[string]any{{"finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "42", "reasoning_content": "six times seven"}}}})
	})
	resp, err := newAzureKeyProvider(t, srv, nil).Chat(context.Background(), Request{Model: "r1", Messages: []Message{{Role: "user", Content: "?"}}})
	if err != nil || resp.Content != "42" || resp.Reasoning != "six times seven" {
		t.Errorf("response = %+v, %v", resp, err)
	}
}

// Item 14: API-key mode sends only the api-key header, on chat, embeddings
// and the provider's own requests (authorize).
func TestAzureFoundry_APIKeyModeSendsOnlyAPIKeyHeader(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 2}}}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "gpt-4o"))
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.EmbeddingDeployment = "embed" })
	if _, err := p.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, err := p.Chat(context.Background(), Request{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, rq := range srv.requests() {
		if rq.Headers.Get("api-key") != "secret-key" || rq.Headers.Get("Authorization") != "" {
			t.Errorf("%s: headers = %v", rq.Path, rq.Headers)
		}
	}
	httpReq, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err := p.authorize(context.Background(), httpReq); err != nil || httpReq.Header.Get("Authorization") != "" || httpReq.Header.Get("api-key") != "secret-key" {
		t.Errorf("authorize: headers = %v (%v)", httpReq.Header, err)
	}
}

// Item 15: the learned reasoning_effort repair is not pre-applied to a
// model-router deployment (the next routed model may reject it).
func TestOpenAICompat_ReasoningEffortMemoSkipsModelRouter(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		writeJSON(w, 200, chatCompletionJSON("ok", "gpt-5"))
	})
	p := newAzureKeyProvider(t, srv, nil)
	tools := []ToolDefinition{{Name: "search_flow", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	for _, model := range []string{"model-router", "gpt-56"} {
		rememberRepairs(p.repairsKey(model), map[string]bool{reasoningEffortParam: true})
		if _, err := p.Chat(context.Background(), Request{Model: model, Tools: tools, Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
			t.Fatalf("Chat %s: %v", model, err)
		}
	}
	reqs := srv.requests()
	if _, ok := reqs[0].Body[reasoningEffortParam]; ok {
		t.Errorf("model router got the learned reasoning_effort: %v", reqs[0].Body)
	}
	if reqs[1].Body[reasoningEffortParam] != "none" {
		t.Errorf("a plain deployment keeps the learned repair: %v", reqs[1].Body)
	}
}

// Item 16: a non-streaming content_filter finish carries its usage on the
// error.
func TestOpenAICompat_ChatContentFilterCarriesUsage(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"model": "gpt-4o-2024-11-20", "choices": []map[string]any{{"finish_reason": "content_filter", "message": map[string]any{"role": "assistant", "content": "x"}}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 3}})
	})
	_, err := newAzureKeyProvider(t, srv, nil).Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, ErrContentFiltered) {
		t.Fatalf("want ErrContentFiltered, got %v", err)
	}
	if in, out, served, ok := usageOf(err); !ok || in != 11 || out != 3 || served != "gpt-4o-2024-11-20" {
		t.Errorf("usage on the error = %d/%d/%q (%v)", in, out, served, ok)
	}
}

// Item 17: temperature goes out when set explicitly, even as 0; an unset
// zero is omitted.
func TestOpenAICompat_TemperatureSet(t *testing.T) {
	b := openaiBase{}
	for _, tc := range []struct {
		req  Request
		want *float64
	}{
		{Request{Temperature: 0}, nil},
		{Request{Temperature: 0, TemperatureSet: true}, new(float64)},
		{Request{Temperature: 0.3}, func() *float64 { v := 0.3; return &v }()},
	} {
		body := b.buildRequest(tc.req, false)
		raw, _ := json.Marshal(body)
		sent := strings.Contains(string(raw), `"temperature"`)
		switch {
		case tc.want == nil && sent:
			t.Errorf("%+v: temperature sent: %s", tc.req, raw)
		case tc.want != nil && (body.Temperature == nil || *body.Temperature != *tc.want):
			t.Errorf("%+v: temperature = %v, want %v (%s)", tc.req, body.Temperature, *tc.want, raw)
		}
	}
}
