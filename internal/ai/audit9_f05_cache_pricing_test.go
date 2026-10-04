package ai

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// costApprox compares two dollar amounts with a tolerance that survives
// float64 rounding in the per-million arithmetic.
func costApprox(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// TestUsageMetric_CacheTokensPricedAtCachedRates: the cached slice of
// TokensIn is priced at the cached rates instead of the full input rate
// (Opus 5.5 reads 0.05x, Fable/Mythos 5.1 0.025x, other Claude 0.1x, writes
// 1.25x). TokensIn itself stays the total.
func TestUsageMetric_CacheTokensPricedAtCachedRates(t *testing.T) {
	claude := &auditStub{
		models: []ModelInfo{
			{ID: "claude-opus-5", Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
			{ID: "claude-fable-5-1", Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
			{ID: "claude-opus-4-5", Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		},
	}
	ap := &auditedProvider{providerID: "claude", inner: claude}

	// Opus 5, 1000 uncached + 9000 cached read + 100 out:
	// (1000×5 + 9000×0.5 + 100×25)/1e6 = 0.0120.
	m := ap.usageMetric(context.Background(), "claude-opus-5", "claude-opus-5-20260101", "", usage{In: 10_000, CacheRead: 9000, Out: 100})
	if !costApprox(m.EstimatedCost, 0.0120) {
		t.Errorf("Opus 5 cached turn = %v, want 0.0120", m.EstimatedCost)
	}
	if m.PromptTokens != 10_000 {
		t.Errorf("PromptTokens = %d, want the TOTAL 10000 (the breakdown is priced, not subtracted)", m.PromptTokens)
	}

	// Fable 5.1 reads at 0.025x: (1000×10 + 9000×0.25 + 100×50)/1e6 = 0.01725.
	m = ap.usageMetric(context.Background(), "claude-fable-5-1", "", "", usage{In: 10_000, CacheRead: 9000, Out: 100})
	if !costApprox(m.EstimatedCost, 0.01725) {
		t.Errorf("Fable 5.1 cached turn = %v, want 0.01725 (reads at 0.025x)", m.EstimatedCost)
	}

	// A Claude model with no special rate reads at 0.1x:
	// (1000×5 + 9000×0.5)/1e6 = 0.0095.
	m = ap.usageMetric(context.Background(), "claude-opus-4-5", "", "", usage{In: 10_000, CacheRead: 9000})
	if !costApprox(m.EstimatedCost, 0.0095) {
		t.Errorf("Opus 4.5 cached turn = %v, want 0.0095 (reads at 0.1x)", m.EstimatedCost)
	}

	// A 5-minute cache write is billed 1.25x: (500×5 + 500×6.25)/1e6 = 0.005625.
	m = ap.usageMetric(context.Background(), "claude-opus-5", "", "", usage{In: 1000, CacheWrite: 500})
	if !costApprox(m.EstimatedCost, 0.005625) {
		t.Errorf("cache-write turn = %v, want 0.005625 (writes at 1.25x)", m.EstimatedCost)
	}

	// Non-Claude, non-Azure providers keep the full input rate on cached
	// tokens (no verified cached meter) — the safe overstatement. Azure
	// Foundry is unpriced (no cached-token discount table), so its cost is
	// zero whatever the cached breakdown.
	az := &auditStub{models: []ModelInfo{{ID: "gpt-4o", Pricing: Pricing{InputCostPerM: 2.5, OutputCostPerM: 10.0}}}}
	apAz := &auditedProvider{providerID: "openai", inner: az}
	m = apAz.usageMetric(context.Background(), "gpt-4o", "gpt-4o", "", usage{In: 1000, CacheRead: 900})
	if !costApprox(m.EstimatedCost, 2.5/1000) {
		t.Errorf("non-Claude cached turn = %v, want the full input rate 0.0025", m.EstimatedCost)
	}

	// A host that reports more cache tokens than input must not mint a
	// negative cost.
	m = ap.usageMetric(context.Background(), "claude-opus-5", "", "", usage{In: 100, CacheRead: 5000})
	if m.EstimatedCost < 0 {
		t.Errorf("oversized cache breakdown priced negative: %v", m.EstimatedCost)
	}
}

func TestCacheReadMultiplier_Families(t *testing.T) {
	for _, tc := range []struct {
		provider, model, served string
		want                    float64
	}{
		{"claude", "claude-opus-5", "", 0.1},
		{"claude", "claude-opus-5-5", "", 0.05},
		{"claude", "claude-fable-5-1", "", 0.025},
		{"claude", "claude-mythos-5-1-20260901", "", 0.025},
		{"claude", "claude-sonnet-4-5", "", 0.1},
		{"azure-foundry", "claude:mydep", "claude-opus-5-5", 0.05}, // served model decides
		{"azure-foundry", "claude:mydep", "", 0.1},                 // unknown Claude deployment
		{"azure-foundry", "mydep", "gpt-4o-2024-11-20", 1.0},       // Azure non-Claude: no discount table (unpriced)
		{"azure-foundry", "mydep", "gpt-5-pro", 1.0},
		{"openai", "gpt-4o", "", 1.0}, // non-Claude provider
	} {
		if got := cacheReadMultiplier(tc.provider, tc.model, tc.served); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("cacheReadMultiplier(%q, %q, %q) = %v, want %v", tc.provider, tc.model, tc.served, got, tc.want)
		}
	}
}

// TestParseOpenAISSE_CachedTokensBreakdown: the
// chat-completions parser carries usage.prompt_tokens_details.cached_tokens
// (Azure's prompt-cache hits; included in prompt_tokens) onto the Done chunk
// as the CacheReadTokens breakdown.
func TestParseOpenAISSE_CachedTokensBreakdown(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"model":"gpt-4o","choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":5000,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":4000}}}`,
		``,
		`data: [DONE]`,
		"",
	}, "\n")
	var got Chunk
	if err := parseOpenAISSE(strings.NewReader(sse), func(c Chunk) {
		if c.Done {
			got = c
		}
	}); err != nil {
		t.Fatalf("parseOpenAISSE: %v", err)
	}
	if !got.Done || got.TokensIn != 5000 || got.TokensOut != 9 {
		t.Fatalf("usage chunk = %+v, want 5000/9", got)
	}
	if got.CacheReadTokens != 4000 {
		t.Errorf("CacheReadTokens = %d, want 4000 (a slice of prompt_tokens)", got.CacheReadTokens)
	}
	if got.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0 (chat completions report reads only)", got.CacheWriteTokens)
	}
}

// TestClaudeChat_CacheTokensCarried: the non-streaming Chat
// path fills the cache breakdown on the Response, included in TokensIn.
func TestClaudeChat_CacheTokensCarried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","stop_reason":"end_turn",
		"content":[{"type":"text","text":"hi"}],
		"usage":{"input_tokens":100,"output_tokens":7,"cache_read_input_tokens":2000,"cache_creation_input_tokens":500}}`))
	}))
	defer srv.Close()
	p := NewClaudeProvider("k")
	p.messagesURL = srv.URL
	resp, err := p.Chat(context.Background(), Request{Model: "claude-opus-5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.TokensIn != 2600 || resp.CacheReadTokens != 2000 || resp.CacheWriteTokens != 500 {
		t.Errorf("usage = in %d (read %d / write %d), want 2600 total with 2000/500 breakdown", resp.TokensIn, resp.CacheReadTokens, resp.CacheWriteTokens)
	}
}

// TestAudited_StreamCacheTokensPricedAtCachedRates: end to end, a Claude
// stream whose usage chunks carry the cache breakdown records
// the cached rate, not the full input rate, on the usage row.
func TestAudited_StreamCacheTokensPricedAtCachedRates(t *testing.T) {
	sink := newUsageSink()
	stub := &auditStub{
		models: []ModelInfo{{ID: "claude-opus-5", Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}}},
		streamChunks: []Chunk{
			{TokensIn: 10_000, CacheReadTokens: 9000, ServedModel: "claude-opus-5"},
			{Text: "hi"},
			{Done: true, TokensIn: 10_000, CacheReadTokens: 9000, TokensOut: 100, ServedModel: "claude-opus-5"},
		},
	}
	ap := NewAuditedProvider(stub, sink.recorder(), "user-1", "claude")
	if err := ap.Stream(context.Background(), Request{Model: "claude-opus-5"}, func(Chunk) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	m := sink.next(t)
	if !costApprox(m.EstimatedCost, 0.0120) {
		t.Errorf("EstimatedCost = %v, want 0.0120 (1000×5 + 9000×0.5 + 100×25 per 1e6)", m.EstimatedCost)
	}
	if m.PromptTokens != 10_000 {
		t.Errorf("PromptTokens = %d, want the total 10000", m.PromptTokens)
	}
}

// TestRetry_HeldUsageKeepsCacheCounts: the withheld
// pre-content error chunk's cache breakdown rides the error (heldUsageError)
// so a failed run's usage is still priced at the cached rates.
func TestRetry_HeldUsageKeepsCacheCounts(t *testing.T) {
	inner := &auditStub{streamChunks: []Chunk{
		{Error: ErrProviderDown, TokensIn: 10_000, CacheReadTokens: 9000, CacheWriteTokens: 500, ServedModel: "claude-opus-5"},
	}}
	inner.streamErr = nil
	rp := NewRetryingProviderWithConfig(inner, 1, time.Millisecond)
	err := rp.Stream(context.Background(), Request{Model: "claude-opus-5"}, func(Chunk) {})
	if err == nil {
		t.Fatal("expected the withheld error to surface")
	}
	if in, out, served, ok := usageOf(err); !ok || in != 10_000 || out != 0 || served != "claude-opus-5" {
		t.Fatalf("usageOf = %d/%d/%q %v, want the held usage", in, out, served, ok)
	}
	if read, write := cacheUsageOf(err); read != 9000 || write != 500 {
		t.Errorf("cacheUsageOf = %d/%d, want 9000/500 (held with the usage)", read, write)
	}
}
