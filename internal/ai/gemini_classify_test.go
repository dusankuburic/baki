package ai

// Step 6, the Gemini group:
//   - R-provider-10: one status-first classifier for Chat, Stream and Embed, so a bodyless or HTML 429/5xx, a
//     401/403 and every embedding failure carry the shared sentinels retry and the breaker act on.
//   - R-provider-11: an empty MAX_TOKENS stop is ErrOutputLimit, Gemini thinking models get the reasoning floor,
//     and the reasoning effort reaches generationConfig.thinkingConfig.
//   - R-streaming-1: the SSE parser keeps the watchdog fed, bills a filtered finish exactly, classifies an error
//     event and carries the served model.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// serveGemini points the Gemini provider at handler for the test; hits counts the requests.
func serveGemini(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*GeminiProvider, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	prev := geminiBaseHost
	geminiBaseHost = srv.URL
	t.Cleanup(func() {
		srv.Close()
		geminiBaseHost = prev
	})
	return &GeminiProvider{apiKey: "key", client: srv.Client()}, &hits
}

func geminiCalls(p Provider) map[string]func() error {
	req := Request{Model: "gemini-2.5-pro", Messages: []Message{{Role: "user", Content: "hi"}}}
	return map[string]func() error{
		"Chat":   func() error { _, err := p.Chat(context.Background(), req); return err },
		"Stream": func() error { return p.Stream(context.Background(), req, func(Chunk) {}) },
		"Embed":  func() error { _, err := p.Embed(context.Background(), []string{"x"}); return err },
	}
}

// R-provider-10, done when: a 429 with an empty body is ErrRateLimited on all three calls, and the retry wrapper
// makes a second attempt.
func TestGeminiErrors_BodylessRateLimit(t *testing.T) {
	p, hits := serveGemini(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	for name, call := range geminiCalls(p) {
		if err := call(); !errors.Is(err, ErrRateLimited) {
			t.Errorf("%s: err = %v, want ErrRateLimited", name, err)
		}
	}
	hits.Store(0)
	rp := NewRetryingProviderWithConfig(p, 2, time.Millisecond)
	for name, call := range geminiCalls(rp) {
		hits.Store(0)
		_ = call()
		if n := hits.Load(); n != 2 {
			t.Errorf("%s through the retry wrapper: %d attempts, want 2", name, n)
		}
	}
}

// R-provider-10, done when: a 502 with an HTML body is ErrProviderDown; a 403 PERMISSION_DENIED is a permission
// refusal (and an auth failure) that is not retried; a 400 naming the API key is ErrApiKeyInvalid on Embed too.
func TestGeminiErrors_StatusFirst(t *testing.T) {
	p, _ := serveGemini(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	})
	for name, call := range geminiCalls(p) {
		if err := call(); !errors.Is(err, ErrProviderDown) {
			t.Errorf("502 HTML, %s: err = %v, want ErrProviderDown", name, err)
		}
	}

	p, hits := serveGemini(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Generative Language API has not been used in project 1"}}`))
	})
	rp := NewRetryingProviderWithConfig(p, 3, time.Millisecond)
	for name, call := range geminiCalls(rp) {
		hits.Store(0)
		err := call()
		if !errors.Is(err, ErrProviderPermissionDenied) || !errors.Is(err, ErrApiKeyInvalid) {
			t.Errorf("403, %s: err = %v, want a permission refusal that is also ErrApiKeyInvalid", name, err)
		}
		if !strings.Contains(err.Error(), "has not been used") {
			t.Errorf("403, %s: err = %q, want the vendor's message", name, err)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("403, %s: %d attempts, want 1", name, n)
		}
	}

	p, _ = serveGemini(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"API key not valid. Please pass a valid API key."}}`))
	})
	for name, call := range geminiCalls(p) {
		if err := call(); !errors.Is(err, ErrApiKeyInvalid) {
			t.Errorf("400 API key, %s: err = %v, want ErrApiKeyInvalid", name, err)
		}
	}
}

// A RESOURCE_EXHAUSTED body carries its retry delay in a RetryInfo detail when the header has none.
func TestGeminiErrors_RetryInfoDelay(t *testing.T) {
	p, _ := serveGemini(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"7s"}]}}`))
	})
	_, err := p.Embed(context.Background(), []string{"x"})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Errorf("err = %#v, want a RateLimitError with RetryAfter 7s", err)
	}
}

// R-provider-11, done when: Chat on a candidate with no parts and finishReason MAX_TOKENS is ErrOutputLimit.
func TestGeminiChat_EmptyMaxTokensIsOutputLimit(t *testing.T) {
	p, _ := serveGemini(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model"},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":5,"thoughtsTokenCount":16}}`))
	})
	_, err := p.Chat(context.Background(), Request{Model: "gemini-2.5-pro", MaxTokens: 16})
	if !errors.Is(err, ErrOutputLimit) {
		t.Errorf("err = %v, want ErrOutputLimit", err)
	}
}

// R-provider-11, done when: the Gemini thinking models get the reasoning floor; the older ones do not.
func TestReasoningFloor_GeminiThinkingModels(t *testing.T) {
	for model, want := range map[string]int{
		"gemini-2.5-pro":                reasoningOutputFloor,
		"gemini-2.5-flash":              reasoningOutputFloor,
		"gemini-3-pro-preview":          reasoningOutputFloor,
		"gemini-2.0-flash":              0,
		"gemini-1.5-pro":                0,
		"gemini-2.0-flash-thinking-exp": reasoningOutputFloor,
	} {
		if got := ReasoningOutputFloor(model); got != want {
			t.Errorf("ReasoningOutputFloor(%q) = %d, want %d", model, got, want)
		}
	}
}

// R-provider-11, done when: a reasoning effort reaches generationConfig.thinkingConfig (a budget on 2.5, a level
// on 3) and "" omits it.
func TestGeminiRequest_ThinkingConfig(t *testing.T) {
	var bodies []map[string]any
	p, _ := serveGemini(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	})
	thinking := func(i int) map[string]any {
		gc, _ := bodies[i]["generationConfig"].(map[string]any)
		tc, _ := gc["thinkingConfig"].(map[string]any)
		return tc
	}
	for _, tc := range []struct {
		model, effort string
		check         func(map[string]any) bool
	}{
		{"gemini-2.5-pro", "low", func(c map[string]any) bool {
			return c != nil && c["thinkingBudget"] != nil && c["thinkingLevel"] == nil
		}},
		{"gemini-2.5-flash", "high", func(c map[string]any) bool { return c != nil && c["thinkingBudget"].(float64) > 8192 }},
		{"gemini-3-pro-preview", "low", func(c map[string]any) bool { return c != nil && c["thinkingLevel"] == "low" }},
		{"gemini-2.5-pro", "", func(c map[string]any) bool { return c == nil }},
	} {
		bodies = nil
		err := p.Stream(context.Background(), Request{Model: tc.model, ReasoningEffort: tc.effort, MaxTokens: 32768, Messages: []Message{{Role: "user", Content: "hi"}}}, func(Chunk) {})
		if err != nil {
			t.Fatalf("%s %q: %v", tc.model, tc.effort, err)
		}
		if got := thinking(0); !tc.check(got) {
			t.Errorf("%s effort %q: thinkingConfig = %v", tc.model, tc.effort, got)
		}
	}
	if spec, _ := SpecOf("gemini"); !spec.Caps.Has(CapReasoning) {
		t.Error("the Gemini spec does not declare CapReasoning: the settings do not offer the effort control")
	}
}

func parseGeminiLines(t *testing.T, lines ...string) ([]Chunk, error) {
	t.Helper()
	var chunks []Chunk
	err := parseGeminiSSE(strings.NewReader(strings.Join(lines, "\n\n")+"\n\n"), func(c Chunk) { chunks = append(chunks, c) })
	return chunks, err
}

// R-streaming-1, done when: events that deliver nothing still feed the watchdog with a keepalive each.
func TestParseGeminiSSE_KeepalivesForEmptyEvents(t *testing.T) {
	chunks, err := parseGeminiLines(t,
		`data: {"usageMetadata":{"promptTokenCount":3},"modelVersion":"gemini-2.5-pro-001"}`,
		`data: {"candidates":[{"content":{"parts":[]},"safetyRatings":[]}],"modelVersion":"gemini-2.5-pro-001"}`,
		`data: {"candidates":[{"content":{"parts":[]}}],"modelVersion":"gemini-2.5-pro-001"}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-pro-001"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 5 {
		t.Fatalf("chunks = %+v, want three keepalives, the text and Done", chunks)
	}
	for i := range 3 {
		c := chunks[i]
		if c.Text != "" || c.Done || c.Error != nil || len(c.ToolCalls) > 0 {
			t.Errorf("chunk %d = %+v, want a field-less keepalive", i, c)
		}
	}
	if chunks[3].Text != "hi" {
		t.Errorf("chunk 3 = %+v, want the text", chunks[3])
	}
	last := chunks[len(chunks)-1]
	if !last.Done || last.ServedModel != "gemini-2.5-pro-001" {
		t.Errorf("Done chunk = %+v, want ServedModel from modelVersion", last)
	}
}

// R-streaming-1, done when: a filtered finish carries the usage Gemini reported.
func TestParseGeminiSSE_FilteredFinishKeepsUsage(t *testing.T) {
	chunks, err := parseGeminiLines(t,
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":30}}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	last := chunks[len(chunks)-1]
	if last.Error == nil || last.TokensIn != 100 || last.TokensOut != 50 {
		t.Errorf("filtered finish = %+v, want an error chunk with TokensIn 100 and TokensOut 50", last)
	}
}

// R-streaming-1, done when: an error event after the 200 ends the stream with a classified error chunk and a
// nil return, which the retry wrapper retries before any content.
func TestParseGeminiSSE_ErrorEvent(t *testing.T) {
	chunks, err := parseGeminiLines(t,
		`data: {"usageMetadata":{"promptTokenCount":4}}`,
		`data: {"error":{"code":503,"status":"UNAVAILABLE","message":"The model is overloaded."}}`,
	)
	if err != nil {
		t.Fatalf("return = %v, want nil (the error is a chunk)", err)
	}
	last := chunks[len(chunks)-1]
	if !errors.Is(last.Error, ErrProviderDown) || !strings.Contains(last.Error.Error(), "overloaded") {
		t.Errorf("error chunk = %+v, want ErrProviderDown with the message", last)
	}

	chunks, _ = parseGeminiLines(t, `data: {"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`)
	if !errors.Is(chunks[len(chunks)-1].Error, ErrRateLimited) {
		t.Errorf("429 event = %+v, want ErrRateLimited", chunks[len(chunks)-1])
	}

	var hits atomic.Int32
	p, _ := serveGemini(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if hits.Add(1) == 1 {
			_, _ = w.Write([]byte("data: {\"error\":{\"code\":503,\"status\":\"UNAVAILABLE\",\"message\":\"x\"}}\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"modelVersion\":\"gemini-2.5-pro-002\"}\n\n"))
	})
	rp := NewRetryingProviderWithConfig(p, 2, time.Millisecond)
	var got []Chunk
	err = rp.Stream(context.Background(), Request{Model: "gemini-2.5-pro", Messages: []Message{{Role: "user", Content: "hi"}}}, func(c Chunk) { got = append(got, c) })
	if err != nil || hits.Load() != 2 {
		t.Fatalf("retried stream: err %v after %d attempts, want success on the second", err, hits.Load())
	}
	if last := got[len(got)-1]; !last.Done || last.ServedModel != "gemini-2.5-pro-002" {
		t.Errorf("Done = %+v, want the served model", last)
	}
}

// Review: a Gemini 403 is the provider refusing access (vendor-neutral copy), and one that names the API key (a
// leaked or restricted key) is an invalid key; a daily quota is not a rate limit to wait out; a filtered finish
// is ErrContentFiltered.
func TestGeminiErrors_ReviewRound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"403 API disabled", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Generative Language API has not been used in project 1 before or it is disabled."}}`,
			func(err error) bool { return errors.Is(err, ErrProviderAccessDenied) }},
		{"403 leaked key", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Your API key was reported as leaked. Please use another API key."}}`,
			func(err error) bool {
				return errors.Is(err, ErrApiKeyInvalid) && !errors.Is(err, ErrProviderAccessDenied)
			}},
		{"daily quota", 429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"You exceeded your current quota","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier"}]}]}}`,
			func(err error) bool {
				return errors.Is(err, ErrInsufficientBalance) && !errors.Is(err, ErrRateLimited) && strings.Contains(err.Error(), "exceeded your current quota")
			}},
		{"per-minute quota", 429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"You exceeded your current quota","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel"}]}]}}`,
			func(err error) bool { return errors.Is(err, ErrRateLimited) }},
	} {
		p, _ := serveGemini(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		if _, err := p.Chat(context.Background(), Request{Model: "gemini-2.5-pro"}); !tc.check(err) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	chunks, _ := parseGeminiLines(t, `data: {"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`)
	if !errors.Is(chunks[len(chunks)-1].Error, ErrContentFiltered) {
		t.Errorf("SAFETY finish = %v, want ErrContentFiltered", chunks[len(chunks)-1].Error)
	}
}

// Review: the thinking budget leaves room for the answer under the turn's output cap, and the lever reaches only
// the models it suits.
func TestGeminiThinking_ReviewRound(t *testing.T) {
	if c := geminiThinking("gemini-2.5-pro", "high", 16384); c == nil || *c.ThinkingBudget > 16384-geminiAnswerReserve {
		t.Errorf("high on a 16384 cap = %+v, want a budget that leaves %d for the answer", c, geminiAnswerReserve)
	}
	if c := geminiThinking("gemini-2.5-pro", "medium", 32768); c == nil || *c.ThinkingBudget != 8192 {
		t.Errorf("medium on a 32768 cap = %+v, want 8192", c)
	}
	for model, effort := range map[string]string{
		"gemini-2.0-flash-thinking-exp": "high", // 2.0 takes no thinking budget
		"gemini-2.5-flash-image":        "high", // no thinking
		"gemini-2.5-flash-lite":         "low",  // off by default: "low" must not turn it on
	} {
		if c := geminiThinking(model, effort, 16384); c != nil {
			t.Errorf("geminiThinking(%q, %q) = %+v, want none", model, effort, c)
		}
	}
	if c := geminiThinking("gemini-2.5-flash-lite", "high", 16384); c == nil {
		t.Error("flash-lite with high effort: want a budget")
	}
}

// Review: the audited wrapper records the exact usage of a Gemini filtered finish, not an estimate.
func TestAudited_GeminiFilteredFinishRecordsExactUsage(t *testing.T) {
	p, _ := serveGemini(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":30}}` + "\n\n"))
	})
	got := make(chan [2]int, 1)
	ap := newAuditedProvider(p, func(_ context.Context, m *interfaces.UsageMetric) error {
		got <- [2]int{m.PromptTokens, m.CompletionTokens}
		return nil
	}, "u1", "gemini")
	_ = ap.Stream(context.Background(), Request{Model: "gemini-2.5-pro", Messages: []Message{{Role: "user", Content: "x"}}}, func(Chunk) {})
	select {
	case u := <-got:
		if u != [2]int{100, 50} {
			t.Errorf("recorded usage = %v, want [100 50]", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no usage recorded")
	}
}

// Review: a 503 on Embed counts toward the embedding circuit.
func TestGeminiEmbed_503CountsForTheBreaker(t *testing.T) {
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)
	p, _ := serveGemini(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	cb := NewCircuitBreakerProvider(p)
	for range cbFailureThreshold {
		if _, err := cb.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrProviderDown) {
			t.Fatalf("Embed err = %v, want ErrProviderDown", err)
		}
	}
	if _, err := cb.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("after %d 503s: %v, want ErrCircuitOpen", cbFailureThreshold, err)
	}
}
