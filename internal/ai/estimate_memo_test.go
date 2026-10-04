package ai

import (
	"encoding/json"
	"fmt"
	"testing"
)

// estimateCountingStub answers a constant token count and records how often
// it was asked: the audited wrapper's inner estimator seam is the Provider
// interface, and embedding it (nil) leaves exactly the method under test.
type estimateCountingStub struct {
	Provider
	calls int
}

func (s *estimateCountingStub) EstimateTokens(string) int {
	s.calls++
	return 3
}

// The estimate memo must absorb the repeats: the same request estimated twice
// consults the inner estimator once, and a request the estimator reads
// differently starts a fresh count instead of hitting the earlier entry.
func TestEstimateRequestTokensMemoizesRepeatRequests(t *testing.T) {
	stub := &estimateCountingStub{}
	ap := &auditedProvider{inner: stub, providerID: "azure-foundry"}
	req := Request{
		SystemPrompt: "perf-round5 memo probe: system",
		Messages: []Message{
			{Role: "user", Content: "perf-round5 memo probe: question"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Name: "probe", Input: json.RawMessage(`{"q":"perf-round5"}`)}},
				ProviderItems: json.RawMessage(`[{"type":"text","text":"perf-round5"}]`)},
		},
	}
	first := ap.estimateRequestTokens(req)
	counted := stub.calls
	second := ap.estimateRequestTokens(req)
	if second != first {
		t.Fatalf("second estimate = %d, first = %d; the memo must repeat the answer", second, first)
	}
	if stub.calls != counted {
		t.Fatalf("identical request re-estimated: estimator calls went %d -> %d", counted, stub.calls)
	}
	changed := req
	changed.Messages = append([]Message(nil), req.Messages...)
	changed.Messages[0].Content += " and more"
	// The stub answers a constant, so only the call count can tell a fresh
	// estimate from a memo hit: reading different bytes must re-estimate.
	ap.estimateRequestTokens(changed)
	if stub.calls == counted {
		t.Fatal("a request the estimator reads differently hit the earlier entry")
	}
}

// The memo must stay bounded no matter how many distinct requests pass
// through it; the victim of an eviction is arbitrary by design.
func TestEstimateRequestTokensMemoStaysBounded(t *testing.T) {
	for i := 0; i < estimateRequestTokensMemoSize*3; i++ {
		requestTokenEstimates.put(uint64(i), i)
	}
	requestTokenEstimates.mu.Lock()
	n := len(requestTokenEstimates.m)
	requestTokenEstimates.mu.Unlock()
	if n > estimateRequestTokensMemoSize {
		t.Fatalf("memo holds %d entries, cap %d", n, estimateRequestTokensMemoSize)
	}
}

// The served-model memo's resource index keeps resource lookups exact across
// inserts and past the memo's cap: every surviving key stays indexed, no
// evicted key is left behind (a stale hit), and a post-eviction insert still
// resolves.
func TestServedModelResourceIndexSurvivesEviction(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)

	const az = "azure-foundry"
	const res = "https://res.services.ai.azure.com"
	recordServedModel("u1\x00"+res, az, "claude:prod", "claude-opus-4-6")
	recordServedModel("u2\x00"+res, az, "claude:prod", "claude-opus-4-6")
	if got, ok := lookupServedModelForResource(res, az, "claude:prod"); !ok || got != "claude-opus-4-6" {
		t.Fatalf("resource lookup = %q, %v; want claude-opus-4-6, true", got, ok)
	}
	// Another resource's observation must not leak into this one's lookup.
	recordServedModel("u1\x00https://other.services.ai.azure.com", az, "claude:prod", "claude-sonnet-4-6")
	if got, _ := lookupServedModelForResource(res, az, "claude:prod"); got != "claude-opus-4-6" {
		t.Fatalf("resource lookup after a foreign insert = %q, want claude-opus-4-6", got)
	}

	// Push the memo past its cap: eviction must unindex every key it drops,
	// and the index must track exactly the surviving memo.
	for i := 0; i < servedMemoMax+64; i++ {
		recordServedModel(fmt.Sprintf("u%d\x00https://r%d.services.ai.azure.com", i, i), az,
			fmt.Sprintf("dep%d", i%8), "gpt-4.1-2025-04-14")
	}
	servedMu.Lock()
	memoLen := len(servedMemo)
	indexed := 0
	for _, bucket := range servedByResource {
		indexed += len(bucket)
		for k := range bucket {
			if _, ok := servedMemo[k]; !ok {
				servedMu.Unlock()
				t.Fatalf("index lists evicted key %q", k)
			}
		}
	}
	servedMu.Unlock()
	if memoLen > servedMemoMax {
		t.Fatalf("memo holds %d entries, cap %d", memoLen, servedMemoMax)
	}
	if indexed != memoLen {
		t.Fatalf("index tracks %d keys, memo holds %d", indexed, memoLen)
	}

	// A post-eviction insert must still resolve through the index.
	const late = "https://late.services.ai.azure.com"
	recordServedModel("late-user\x00"+late, az, "claude:late", "claude-opus-4-6")
	if got, ok := lookupServedModelForResource(late, az, "claude:late"); !ok || got != "claude-opus-4-6" {
		t.Fatalf("post-eviction resource lookup = %q, %v; want claude-opus-4-6, true", got, ok)
	}
}
