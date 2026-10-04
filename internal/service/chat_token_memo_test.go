package service

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/ai"
)

// r4CountingProvider is tokenCountingProvider under its own provider id, so
// the process-wide memo it exercises is not shared with other tests' texts.
type r4CountingProvider struct {
	ai.Provider
	id    string
	calls atomic.Int32
}

func (c *r4CountingProvider) ID() string { return c.id }
func (c *r4CountingProvider) EstimateTokens(s string) int {
	c.calls.Add(1)
	return len(s) / 4
}

// The stored history comes back on every turn of a thread: once the prompt
// is past the byte fast path, every turn's truncation re-estimated every
// stored message (a tiktoken pass each). With the process-wide memo the
// second turn estimates only what is new — the one appended message.
func TestTruncateForContextWindow_HistoryEstimatesMemoisedAcrossTurns(t *testing.T) {
	p := &r4CountingProvider{id: "counting-r4-" + t.Name()}
	msgs := []ai.Message{}
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, ai.Message{Role: role, Content: fmt.Sprintf("%s turn %d: %s", t.Name(), i, strings.Repeat("history text ", 60))})
	}
	const ctxLimit = 10000 // 12 × ~800 bytes + the 4000 reserve: past the byte bound, inside the estimate
	build := func(question string) ai.Request {
		m := append([]ai.Message{}, msgs...)
		m = append(m, ai.Message{Role: "user", Content: question})
		return ai.Request{SystemPrompt: t.Name() + ": you are a flow assistant", Messages: m}
	}

	req := build("first question " + t.Name())
	if _, err := truncateForContextWindow(context.Background(), p, &req, ctxLimit, len(req.Messages)-1); err != nil {
		t.Fatal(err)
	}
	first := p.calls.Load()
	if first != 14 { // the system prompt, 12 stored messages, the question
		t.Fatalf("first turn estimated %d texts, want 14 (resize the test if the fast path admitted it)", first)
	}
	if len(req.Messages) != 13 {
		t.Fatalf("first turn kept %d messages, want 13 (the window must admit the whole history)", len(req.Messages))
	}

	req = build("second question " + t.Name())
	if _, err := truncateForContextWindow(context.Background(), p, &req, ctxLimit, len(req.Messages)-1); err != nil {
		t.Fatal(err)
	}
	if n := p.calls.Load() - first; n != 1 {
		t.Errorf("second turn estimated %d texts, want 1 (only the new question; the history is memoised)", n)
	}
	if len(req.Messages) != 13 {
		t.Errorf("second turn kept %d messages, want 13", len(req.Messages))
	}

	// Through a per-loop memo the global memo is consulted the same way.
	req = build("third question " + t.Name())
	before := p.calls.Load()
	if _, err := truncateForContextWindowMemo(context.Background(), p, &req, ctxLimit, len(req.Messages)-1, newTokenMemo()); err != nil {
		t.Fatal(err)
	}
	if n := p.calls.Load() - before; n != 1 {
		t.Errorf("tool-loop turn estimated %d texts, want 1", n)
	}
}

// Counts are keyed by provider id and text — a different provider's
// tokenizer is not served another's count — and the memo is bounded, least
// recently used first.
func TestTokenCountMemo_KeyedByProviderAndBounded(t *testing.T) {
	memo := newTokenCountMemo(3)
	a := &r4CountingProvider{id: "r4-a-" + t.Name()}
	b := &r4CountingProvider{id: "r4-b-" + t.Name()}
	text := "shared text " + t.Name()
	first := memo.estimate(a, text)
	if second := memo.estimate(a, text); first != len(text)/4 || second != first {
		t.Fatal("estimate changed between calls")
	}
	if a.calls.Load() != 1 {
		t.Fatalf("provider a estimated %d times, want 1", a.calls.Load())
	}
	memo.estimate(b, text)
	if b.calls.Load() != 1 {
		t.Fatal("a count keyed by another provider's id was served")
	}
	memo.estimate(a, text+" 2")
	memo.estimate(a, text+" 3") // the fourth entry evicts (a, text)
	if memo.len() != 3 {
		t.Fatalf("memo holds %d entries, want 3", memo.len())
	}
	memo.estimate(a, text)
	if a.calls.Load() != 4 {
		t.Fatalf("the evicted text was served from the memo (%d calls, want 4)", a.calls.Load())
	}
}
