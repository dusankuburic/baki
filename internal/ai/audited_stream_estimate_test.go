package ai

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// countingEstimateStub is an auditStub whose EstimateTokens counts its calls.
type countingEstimateStub struct {
	auditStub
	estimates atomic.Int64
}

func (s *countingEstimateStub) EstimateTokens(t string) int {
	s.estimates.Add(1)
	return len(t) / 4
}

func streamDeltas(n int) []Chunk {
	chunks := make([]Chunk, 0, n)
	for i := 0; i < n; i++ {
		chunks = append(chunks, Chunk{Text: "delta number " + strings.Repeat("x", i%7)})
	}
	return chunks
}

func runAuditedStream(t *testing.T, stub *countingEstimateStub, req Request) *interfaces.UsageMetric {
	t.Helper()
	ch := make(chan *interfaces.UsageMetric, 1)
	ap := NewAuditedProvider(stub, func(_ context.Context, m *interfaces.UsageMetric) error { ch <- m; return nil }, "user-1", "stub")
	if err := ap.Stream(context.Background(), req, func(Chunk) {}); err != nil && stub.streamErr == nil {
		t.Fatalf("Stream: %v", err)
	}
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("usage metric was not recorded")
		return nil
	}
}

// The audited wrapper tokenised every delta as it arrived (~2.75 µs and 16
// allocations each) for an estimate needed only when the provider reports no
// usage. A stream whose provider reports usage must not tokenise at all.
func TestAuditedStream_NoPerDeltaTokenizeWhenUsageReported(t *testing.T) {
	stub := &countingEstimateStub{}
	stub.models = []ModelInfo{{ID: "m"}}
	stub.streamChunks = append(streamDeltas(200), Chunk{Done: true, TokensIn: 11, TokensOut: 22})
	m := runAuditedStream(t, stub, Request{Model: "m", Messages: []Message{{Role: "user", Content: "q"}}})
	if m.PromptTokens != 11 || m.CompletionTokens != 22 {
		t.Errorf("usage = %d/%d, want the reported 11/22", m.PromptTokens, m.CompletionTokens)
	}
	if n := stub.estimates.Load(); n != 0 {
		t.Errorf("EstimateTokens ran %d times on a stream with reported usage, want 0", n)
	}
}

// Without reported usage the estimate is unchanged: the sum of each delta's
// own count (text and reasoning), for both the Done(0,0) and the no-Done
// fallbacks.
func TestAuditedStream_EstimateIsPerDeltaSum(t *testing.T) {
	deltas := streamDeltas(50)
	deltas = append(deltas, Chunk{Reasoning: "thinking about it"})
	want := 0
	for _, c := range deltas {
		want += len(c.Text)/4 + len(c.Reasoning)/4
	}
	req := Request{Model: "m", Messages: []Message{{Role: "user", Content: "q"}}}

	done := &countingEstimateStub{}
	done.models = []ModelInfo{{ID: "m"}}
	done.streamChunks = append(append([]Chunk{}, deltas...), Chunk{Done: true})
	if m := runAuditedStream(t, done, req); m.CompletionTokens != want {
		t.Errorf("Done(0,0): CompletionTokens = %d, want the per-delta sum %d", m.CompletionTokens, want)
	}

	cut := &countingEstimateStub{}
	cut.models = []ModelInfo{{ID: "m"}}
	cut.streamChunks = deltas // no Done: truncated
	if m := runAuditedStream(t, cut, req); m.CompletionTokens != want {
		t.Errorf("no Done: CompletionTokens = %d, want the per-delta sum %d", m.CompletionTokens, want)
	}
}

// A very long stream is folded into the sum as it goes (bounded memory) and
// still yields the same per-delta sum.
func TestDeltaEstimate_FoldsAndKeepsSum(t *testing.T) {
	calls := 0
	d := deltaEstimate{estimate: func(s string) int { calls++; return len(s) }}
	piece := strings.Repeat("y", 4096)
	n := deltaEstimateFoldBytes/len(piece) + 10
	for i := 0; i < n; i++ {
		d.add(piece)
	}
	if calls == 0 || len(d.pending) >= n {
		t.Errorf("no fold past %d bytes (calls=%d, pending=%d)", deltaEstimateFoldBytes, calls, len(d.pending))
	}
	if got := d.total(); got != n*len(piece) {
		t.Errorf("total = %d, want %d", got, n*len(piece))
	}
}
