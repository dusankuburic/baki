package ai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Code review #3 (W1): the Claude route — direct and on Azure AI Foundry.

// sseEvent formats one Anthropic SSE event.
func sseEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

// isClaudeKeepalive reports whether a chunk carries nothing but (optionally)
// the served model.
func isClaudeKeepalive(c Chunk) bool {
	return c.Text == "" && c.Reasoning == "" && c.TokensIn == 0 && c.TokensOut == 0 && !c.Done &&
		c.Error == nil && len(c.ToolCalls) == 0 && c.FinishReason == "" && c.ToolEvent == nil && !c.ServerStep
}

// R-10: an adaptive-thinking phase (thinking/signature deltas, pings, block
// start/stop) delivers one keepalive per event, each naming the served
// model, so the chat idle watchdog does not cancel a long thinking phase.
func TestParseClaudeSSE_ThinkingOnlyEventsYieldKeepalives(t *testing.T) {
	sse := sseEvent("message_start", `{"type":"message_start","message":{"model":"claude-opus-4-7","usage":{"input_tokens":10}}}`) +
		sseEvent("ping", `{"type":"ping"}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"more"}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sseEvent("ping", `{"type":"ping"}`) +
		sseEvent("some_future_event", `{"type":"some_future_event"}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)

	var chunks []Chunk
	if err := parseClaudeSSE(strings.NewReader(sse), func(c Chunk) { chunks = append(chunks, c) }); err != nil {
		t.Fatalf("parseClaudeSSE: %v", err)
	}
	keepalives := 0
	var text string
	var done Chunk
	for _, c := range chunks {
		if c.ServedModel != "claude-opus-4-7" {
			t.Errorf("every chunk after message_start carries the served model, got %+v", c)
		}
		if isClaudeKeepalive(c) {
			keepalives++
		}
		text += c.Text
		if c.Done {
			done = c
		}
	}
	// ping, block start, 2 thinking deltas, signature, block stop, ping,
	// unknown, text block start, message_stop.
	if keepalives != 10 {
		t.Errorf("keepalives = %d, want one per silent event (10); chunks %+v", keepalives, chunks)
	}
	if text != "answer" || !done.Done || done.TokensIn != 10 || done.TokensOut != 7 {
		t.Errorf("text %q done %+v", text, done)
	}
}

// A read error AFTER the terminal message_delta loses nothing: the stream is
// complete and the error is not reported.
func TestParseClaudeSSE_ReadErrorAfterDoneIsNil(t *testing.T) {
	sse := sseEvent("message_start", `{"type":"message_start","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":3}}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
	body := io.MultiReader(strings.NewReader(sse), errReader{errors.New("connection reset")})
	var done bool
	if err := parseClaudeSSE(body, func(c Chunk) { done = done || c.Done }); err != nil || !done {
		t.Errorf("err = %v, done = %v; want nil after a delivered Done", err, done)
	}
	// Before Done the same error is reported.
	early := io.MultiReader(strings.NewReader(sseEvent("message_start", `{"type":"message_start","message":{"model":"m"}}`)), errReader{errors.New("connection reset")})
	if err := parseClaudeSSE(early, func(Chunk) {}); err == nil {
		t.Error("a read error before Done must be reported")
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// stop_reason "refusal" is ErrContentFiltered on the stream (an error chunk
// with the usage and served model) and on Chat.
func TestClaude_RefusalIsContentFiltered(t *testing.T) {
	sse := sseEvent("message_start", `{"type":"message_start","message":{"model":"claude-fable-5-1","usage":{"input_tokens":20}}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I"}}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":2}}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
	var errChunk Chunk
	var sawDone bool
	if err := parseClaudeSSE(strings.NewReader(sse), func(c Chunk) {
		if c.Error != nil {
			errChunk = c
		}
		sawDone = sawDone || c.Done
	}); err != nil {
		t.Fatalf("parseClaudeSSE: %v", err)
	}
	if !errors.Is(errChunk.Error, ErrContentFiltered) || errChunk.TokensIn != 20 || errChunk.TokensOut != 2 || errChunk.ServedModel != "claude-fable-5-1" {
		t.Errorf("refusal chunk = %+v", errChunk)
	}
	if sawDone {
		t.Error("a refusal must not also deliver a Done chunk")
	}

	p, _ := newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
		m := claudeMessageJSON("", "claude-fable-5-1")
		m["stop_reason"] = "refusal"
		m["content"] = []map[string]any{}
		writeJSON(w, 200, m)
	})
	if _, err := p.Chat(context.Background(), Request{Model: "claude-fable-5-1", Messages: []Message{{Role: "user", Content: "x"}}}); !errors.Is(err, ErrContentFiltered) {
		t.Errorf("chat refusal: want ErrContentFiltered, got %v", err)
	}
}

// A mid-stream error event carries the served model and the input tokens.
func TestParseClaudeSSE_ErrorChunkCarriesUsage(t *testing.T) {
	sse := sseEvent("message_start", `{"type":"message_start","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":42}}}`) +
		sseEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	var errChunk Chunk
	_ = parseClaudeSSE(strings.NewReader(sse), func(c Chunk) {
		if c.Error != nil {
			errChunk = c
		}
	})
	if !errors.Is(errChunk.Error, ErrProviderDown) || errChunk.TokensIn != 42 || errChunk.ServedModel != "claude-sonnet-4-6" {
		t.Errorf("error chunk = %+v", errChunk)
	}
}

// newPlainClaude is the direct Claude provider against a local server.
func newPlainClaude(t *testing.T, h http.HandlerFunc) (*ClaudeProvider, *int) {
	t.Helper()
	resetLearnedRepairs()
	calls := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	orig := claudeAPIURL
	t.Cleanup(func() { claudeAPIURL = orig })
	claudeAPIURL = srv.URL + "/v1/messages"
	p := NewClaudeProvider("k")
	p.client = srv.Client()
	return p, calls
}

func claudeCallBoth(p *ClaudeProvider, req Request) map[string]error {
	_, chatErr := p.Chat(context.Background(), req)
	return map[string]error{"chat": chatErr, "stream": p.Stream(context.Background(), req, func(Chunk) {})}
}

// R-11: status first — an HTML 503 from a gateway is ErrProviderDown, an
// empty 429 is rate-limited with its retry-after-ms, a 529 overloaded is
// ErrProviderDown; the body only enriches, and residual errors carry the
// request id.
func TestClaude_ErrorsAreClassifiedByStatusFirst(t *testing.T) {
	req := Request{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "x"}}}

	p, _ := newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "<html><body>503 Service Unavailable</body></html>")
	})
	for what, err := range claudeCallBoth(p, req) {
		if !errors.Is(err, ErrProviderDown) || !isRetryable(err) {
			t.Errorf("HTML 503 %s: want retryable ErrProviderDown, got %v", what, err)
		}
	}

	p, _ = newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("retry-after-ms", "1500")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	for what, err := range claudeCallBoth(p, req) {
		var rl *RateLimitError
		if !errors.As(err, &rl) || rl.RetryAfter != 1500*time.Millisecond {
			t.Errorf("empty 429 %s: want RateLimitError(1.5s), got %v", what, err)
		}
	}

	p, _ = newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_123")
		writeJSON(w, 529, map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "Overloaded"}})
	})
	for what, err := range claudeCallBoth(p, req) {
		if !errors.Is(err, ErrProviderDown) || !strings.Contains(err.Error(), "Overloaded") || !strings.Contains(err.Error(), "req_123") {
			t.Errorf("529 %s: want ErrProviderDown with the message and request id, got %v", what, err)
		}
	}

	p, _ = newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("apim-request-id", "apim-9")
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "messages: bad"}})
	})
	for what, err := range claudeCallBoth(p, req) {
		if err == nil || !strings.Contains(err.Error(), "messages: bad") || !strings.Contains(err.Error(), "apim-9") || isRetryable(err) {
			t.Errorf("400 %s: want a permanent error with the message and request id, got %v", what, err)
		}
	}

	p, _ = newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error",
			"message": "prompt is too long: 250000 tokens > 200000 maximum"}})
	})
	for what, err := range claudeCallBoth(p, req) {
		var cle *ContextLimitError
		if !errors.As(err, &cle) || cle.Limit != 200000 {
			t.Errorf("prompt too long %s: want ContextLimitError{200000}, got %v", what, err)
		}
	}
}

// Anthropic's permission_error is the permission-denied path (no token
// replay, the explanation kept); authentication_error stays an invalid
// credential.
func TestClaude_PermissionErrorIsPermissionDenied(t *testing.T) {
	req := Request{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "x"}}}
	permission := map[string]any{"type": "error", "error": map[string]any{"type": "permission_error", "message": "Your identity may not use this model."}}

	p, _ := newPlainClaude(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 403, permission) })
	for what, err := range claudeCallBoth(p, req) {
		if !errors.Is(err, ErrProviderPermissionDenied) || !strings.Contains(err.Error(), "may not use this model") {
			t.Errorf("direct %s: want ErrProviderPermissionDenied, got %v", what, err)
		}
	}

	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) { writeJSON(w, 403, permission) })
	entra := newAzureClaudeEntraProvider(t, srv)
	creq := Request{Model: "claude:my-claude", Messages: []Message{{Role: "user", Content: "x"}}}
	if _, err := entra.Chat(context.Background(), creq); !errors.Is(err, ErrProviderPermissionDenied) || errors.Is(err, ErrEntraTokenRejected) {
		t.Errorf("entra: want ErrProviderPermissionDenied, got %v", err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("entra: %d requests, want 1 — a fresh token cannot grant a missing role", n)
	}
	key := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	if err := key.Stream(context.Background(), creq, func(Chunk) {}); !errors.Is(err, ErrProviderPermissionDenied) {
		t.Errorf("api key: want ErrProviderPermissionDenied, got %v", err)
	}

	auth := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 401, map[string]any{"type": "error", "error": map[string]any{"type": "authentication_error", "message": "invalid token"}})
	})
	entra2 := newAzureClaudeEntraProvider(t, auth)
	if _, err := entra2.Chat(context.Background(), creq); !errors.Is(err, ErrEntraTokenRejected) || errors.Is(err, ErrProviderPermissionDenied) {
		t.Errorf("authentication_error: want the rejected-token path, got %v", err)
	}
}

// R-12: the sampling policy reads the model name case-insensitively and
// covers every model that rejects temperature.
func TestClaudeRemovesSampling_Families(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-opus-4-7":            true,
		"Claude-Opus-4-8-20260901":   true,
		"claude-opus-5":              true,
		"claude-opus-5-5":            true,
		"claude-sonnet-5":            true,
		"claude-sonnet-5-5":          true,
		"claude-fable-5-1":           true,
		"claude-mythos-5":            true,
		"claude-opus-4-6":            false,
		"claude-opus-4-5-20251101":   false,
		"claude-opus-4-20250514":     false,
		"claude-sonnet-4-6":          false,
		"claude-sonnet-4-5-20250929": false,
		"claude-haiku-4-5":           false,
		"claude-3-5-sonnet-20241022": false,
		"prod-claude":                false,
		"":                           false,
	} {
		if got := claudeRemovesSampling(model); got != want {
			t.Errorf("claudeRemovesSampling(%q) = %v, want %v", model, got, want)
		}
	}
}

// R-12: a custom-named Foundry deployment is policed by the model it serves
// (served-model memo): an Opus 4.7 behind "prod-claude" gets no temperature
// and adaptive thinking.
func TestAzureClaude_SamplingPolicyFromServedModel(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 200, claudeMessageJSON("ok", "claude-opus-4-7"))
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"prod-claude"} })
	recordServedModel("some-user\x00"+p.servedMemoResource(), azureFoundryProviderID, "claude:prod-claude", "claude-opus-4-7")

	if _, err := p.Chat(context.Background(), Request{Model: "claude:prod-claude", Temperature: 0.3,
		Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	body := srv.requests()[0].Body
	if _, has := body["temperature"]; has {
		t.Errorf("temperature sent to an Opus 4.7 deployment: %v", body)
	}
	if th, _ := body["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("thinking = %v, want adaptive", body["thinking"])
	}
	// Another resource's memo does not leak into this decision.
	resetServedModels()
	recordServedModel("u\x00https://other.services.ai.azure.com", azureFoundryProviderID, "claude:prod-claude", "claude-opus-4-7")
	if got := p.claudeServedModel("prod-claude"); got != "" {
		t.Errorf("claudeServedModel = %q, want nothing from another resource", got)
	}
}

// R-12: a 400 naming temperature is repaired once and remembered.
func TestAzureClaude_TemperatureRejectionRepairedOnce(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, body map[string]any) {
		if _, has := body["temperature"]; has {
			writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error",
				"message": "`temperature` is deprecated for this model."}})
			return
		}
		writeJSON(w, 200, claudeMessageJSON("ok", "claude-next"))
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"mystery"} })
	req := Request{Model: "claude:mystery", Temperature: 0.3, Messages: []Message{{Role: "user", Content: "x"}}}
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("first chat: %v", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Fatalf("first chat: %d requests, want the rejection and one repaired retry", n)
	}
	if err := p.Stream(context.Background(), req, func(Chunk) {}); err == nil {
		// the stub answers JSON, not SSE: a truncation error is expected;
		// what matters is the request that went out.
		t.Log("stream parsed a JSON body")
	}
	reqs := srv.requests()
	if len(reqs) != 3 {
		t.Fatalf("second call: %d requests total, want 3 (memoised repair, no rejection)", len(reqs))
	}
	if _, has := reqs[2].Body["temperature"]; has {
		t.Error("the remembered repair must pre-apply")
	}

	// A rejection of something else is not retried.
	other := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "messages: roles must alternate"}})
	})
	p2 := newAzureKeyProvider(t, other, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"mystery"} })
	if _, err := p2.Chat(context.Background(), req); err == nil || len(other.requests()) != 1 {
		t.Errorf("unrelated 400: err %v after %d requests, want one request", err, len(other.requests()))
	}
}

// projectWithClaude answers a project listing in which "foo" (named by the
// document as a chat deployment) is Anthropic-published and "c1" / "c2" are
// Claude deployments detected by model name and by the document.
func projectWithClaude(t *testing.T) *azureTestServer {
	return newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			writeJSON(w, 200, map[string]any{"value": []map[string]any{
				{"name": "foo", "modelName": "claude-sonnet-4-6", "modelPublisher": "Anthropic"},
				{"name": "c1", "modelName": "Claude-Opus-4-7"},
				{"name": "c2", "modelName": "house-model"},
				{"name": "gpt", "modelName": "gpt-4o", "modelPublisher": "OpenAI"},
			}})
		case "/api/projects/p1/agents":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"name": "helper", "definition": map[string]any{"kind": "prompt", "model": "c1"}}}})
		case "/anthropic/v1/messages":
			writeJSON(w, 200, claudeMessageJSON("from claude", "claude-sonnet-4-6"))
		case "/anthropic/v1/messages/count_tokens":
			writeJSON(w, 200, map[string]any{"input_tokens": 77})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
}

// R-14 + publisher detection + the 200k clamp for agents.
func TestAzureClaude_RelistedDefaultRoutesToAnthropic(t *testing.T) {
	srv := projectWithClaude(t)
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "foo"
	p.cfg.ClaudeDeployments = []string{"C2"}

	if def := p.DefaultModel(); def != "foo" {
		t.Errorf("DefaultModel before any listing = %q", def)
	}
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[strings.ToLower(m.ID)] = m
	}
	for _, id := range []string{"claude:foo", "claude:c1", "claude:c2"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("%s missing from the listing %v", id, models)
		}
	}
	if _, ok := byID["foo"]; ok {
		t.Error("foo must be listed only as a Claude deployment")
	}
	if agent := byID["agent:helper"]; agent.ContextLimit != 1000000 || agent.ServedModel != "Claude-Opus-4-7" {
		t.Errorf("agent on a Claude deployment = %+v, want Opus 4.7's 1M Foundry window", agent)
	}

	if def := p.DefaultModel(); def != "claude:foo" {
		t.Errorf("DefaultModel = %q, want the re-listed claude:foo", def)
	}
	resp, err := p.Chat(context.Background(), Request{Model: "foo", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil || resp.Content != "from claude" {
		t.Fatalf("Chat(foo) = %+v, %v", resp, err)
	}
	var sent azureSeenRequest
	for _, r := range srv.requests() {
		if r.Path == "/anthropic/v1/messages" {
			sent = r
		}
	}
	if sent.Body["model"] != "foo" {
		t.Errorf("Anthropic request = %+v, want model foo", sent)
	}
	// The Claude policy came from the listing: c1 serves Opus 4.7.
	if got := p.claudeServedModel("c1"); got != "Claude-Opus-4-7" {
		t.Errorf("claudeServedModel(c1) = %q", got)
	}
}

// Claude on Foundry uses the catalogue window (Anthropic's Foundry page: 1M
// for Opus/Sonnet 4.6+ and 5.x, 200k for Sonnet 4.5 and Haiku 4.5).
func TestAnnotateServedModel_ClaudeOnFoundryUsesCatalogueWindow(t *testing.T) {
	small := ModelInfo{ID: "old", DisplayName: "old", ContextLimit: 128000}
	applyServedModel(azureFoundryProviderID, &small, "claude-sonnet-4-5")
	if small.ContextLimit != 200000 {
		t.Errorf("bare deployment serving Sonnet 4.5 = %+v, want its 200k window", small)
	}
	m := ModelInfo{ID: "bare", DisplayName: "bare", ContextLimit: 128000}
	applyServedModel(azureFoundryProviderID, &m, "claude-opus-4-6")
	if m.ContextLimit != 1000000 || m.MaxOutputTokens != 128000 || m.Pricing != (Pricing{}) {
		t.Errorf("bare deployment serving Opus 4.6 = %+v", m)
	}
	g := ModelInfo{ID: "g", DisplayName: "g", ContextLimit: 128000}
	applyServedModel(azureFoundryProviderID, &g, "gpt-4.1")
	if g.ContextLimit != 1047576 {
		t.Errorf("non-Claude entry = %+v, want the catalogue window", g)
	}
}

// Low: the per-call token — a token another call fetched meanwhile is not the
// one invalidated.
func TestAzureFoundry_TokenRetryInvalidatesTheCallsOwnToken(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {})
	p := newAzureClaudeEntraProvider(t, srv)
	var sent []string
	calls := 0
	err := p.withTokenRetry(context.Background(), func(ctx context.Context) error {
		calls++
		tok, err := p.entraToken(ctx)
		if err != nil {
			return err
		}
		sent = append(sent, tok)
		if calls == 1 {
			// A concurrent call replaced the cached token meanwhile (its own
			// retry) and fetched a fresh one on its own context.
			p.tokens.Invalidate(tok)
			if _, err := p.entraToken(context.Background()); err != nil {
				return err
			}
			return ErrApiKeyInvalid
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The retry reuses the concurrent call's still-valid tok-2; the old
	// provider-wide lastToken invalidated tok-2 and minted tok-3.
	if strings.Join(sent, ",") != "tok-1,tok-2" {
		t.Errorf("tokens sent = %v, want tok-1 then the other call's tok-2", sent)
	}
}

// The catalogue carries the Foundry-sold Claude models: the direct Claude
// provider keeps their published prices, while the same models on Azure
// Foundry resolve with the window/output cap and no price (Azure is unpriced).
func TestClaudeCatalogue_FoundryModelsAreUnpricedOnAzure(t *testing.T) {
	for id, want := range map[string]Pricing{
		"claude-opus-4-5":       {InputCostPerM: 5, OutputCostPerM: 25},
		"claude-opus-4-6":       {InputCostPerM: 5, OutputCostPerM: 25},
		"claude-opus-4-7":       {InputCostPerM: 5, OutputCostPerM: 25},
		"claude-sonnet-4-5":     {InputCostPerM: 3, OutputCostPerM: 15},
		"claude-opus-5-5":       {InputCostPerM: 4, OutputCostPerM: 20},
		"claude-sonnet-5":       {InputCostPerM: 2, OutputCostPerM: 10},
		"claude-mythos-5-1":     {InputCostPerM: 10, OutputCostPerM: 50},
		"claude-mythos-preview": {InputCostPerM: 10, OutputCostPerM: 50},
	} {
		direct, ok := catalogEntryFor("claude", id)
		if !ok || direct.Pricing != want {
			t.Errorf("direct %s: %+v %v, want %+v", id, direct.Pricing, ok, want)
		}
		entry, ok := catalogEntryFor(azureFoundryProviderID, id)
		if !ok || entry.Pricing != (Pricing{}) || entry.ContextLimit != direct.ContextLimit || entry.MaxOutputTokens != direct.MaxOutputTokens {
			t.Errorf("azure %s: %+v %v, want the direct window/cap %d/%d with no price", id, entry, ok, direct.ContextLimit, direct.MaxOutputTokens)
		}
		if dep := claudeDeploymentEntry(id); dep.Pricing != (Pricing{}) {
			t.Errorf("claudeDeploymentEntry(%s) carries pricing %+v", id, dep.Pricing)
		}
	}
	if e, ok := catalogEntryFor("claude", "claude-opus-5-5-20261001"); !ok || e.ID != "claude-opus-5-5" {
		t.Errorf("a dated Opus 5.5 must resolve to Opus 5.5, not Opus 5: %+v %v", e, ok)
	}
	// A portal-default mythos-preview deployment is catalogued at its real
	// 1M/128K limits and stays out of the direct provider's picker.
	if e, ok := catalogEntryFor("claude", "claude-mythos-preview"); !ok || e.ContextLimit != 1000000 || e.MaxOutputTokens != 128000 {
		t.Errorf("claude-mythos-preview entry = %+v %v, want 1M / 128K", e, ok)
	}
	if dep := claudeDeploymentEntry("claude-mythos-preview"); dep.ContextLimit != 1000000 || dep.MaxOutputTokens != 128000 {
		t.Errorf("claude-mythos-preview deployment entry = %+v, want 1M / 128K", dep)
	}
	if !claudeInviteOnly["claude-mythos-preview"] {
		t.Error("claude-mythos-preview must be invite-only (hidden from the direct picker)")
	}
}
