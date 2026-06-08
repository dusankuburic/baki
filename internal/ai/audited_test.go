package ai

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// auditStub is a minimal Provider for exercising auditedProvider.record cost
// math: it returns a fixed response and a configurable catalog + default price.
type auditStub struct {
	Provider
	models       []ModelInfo
	price        Pricing
	resp         *Response
	streamChunks []Chunk
}

func (s *auditStub) Chat(_ context.Context, _ Request) (*Response, error) { return s.resp, nil }
func (s *auditStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	for _, c := range s.streamChunks {
		onChunk(c)
	}
	return nil
}
func (s *auditStub) Models() []ModelInfo            { return s.models }
func (s *auditStub) PricePerMillionTokens() Pricing { return s.price }
func (s *auditStub) ID() string                     { return "stub" }

// awaitMetric runs a Chat through the audited wrapper and returns the recorded
// metric (recording is async). Fails the test if nothing is recorded.
func awaitMetric(t *testing.T, stub *auditStub, req Request) *interfaces.UsageMetric {
	t.Helper()
	ch := make(chan *interfaces.UsageMetric, 1)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error {
		ch <- m
		return nil
	}
	ap := NewAuditedProvider(stub, rec, "user-1", "stub")
	if _, err := ap.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("usage metric was not recorded")
		return nil
	}
}

// TestAudited_NilRecorderDoesNotPanic guards the local/desktop-mode wiring:
// when there is no storage backend the factory passes a nil recorder, and the
// audited provider must skip recording rather than calling through it. Chat and
// Stream both have to be safe (record() runs in a goroutine on the Stream path).
func TestAudited_NilRecorderDoesNotPanic(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
		resp:   &Response{Content: "ok", TokensIn: 100, TokensOut: 100},
	}
	ap := NewAuditedProvider(stub, nil, "user-1", "stub")
	if _, err := ap.Chat(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatalf("Chat with nil recorder: %v", err)
	}
	// Stream records in a spawned goroutine on the Done chunk — drive it too.
	stub.streamChunks = []Chunk{{Text: "ok"}, {Done: true, TokensIn: 100, TokensOut: 100}}
	if err := ap.Stream(context.Background(), Request{Model: "m"}, func(Chunk) {}); err != nil {
		t.Fatalf("Stream with nil recorder: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // let the record goroutine run (it must no-op)
}

func TestAudited_PricesFromCatalogWhenModelKnown(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "known", Pricing: Pricing{InputCostPerM: 3, OutputCostPerM: 15}}},
		price:  Pricing{InputCostPerM: 99, OutputCostPerM: 99}, // should NOT be used
		resp:   &Response{TokensIn: 1_000_000, TokensOut: 1_000_000},
	}
	m := awaitMetric(t, stub, Request{Model: "known"})
	if got, want := m.EstimatedCost, 18.0; got != want {
		t.Errorf("EstimatedCost = %v, want %v (catalog price)", got, want)
	}
}

// TestAudited_RecordsOrgID is the T4.2 guarantee: usage carries the request's
// OrgID, so org-scoped flows can be summed for an org-wide daily budget.
func TestAudited_RecordsOrgID(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "known", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
		resp:   &Response{TokensIn: 1000, TokensOut: 1000},
	}
	m := awaitMetric(t, stub, Request{Model: "known", OrgID: "org-42"})
	if m.OrgID != "org-42" {
		t.Errorf("OrgID = %q, want %q", m.OrgID, "org-42")
	}
	if m.UserID != "user-1" {
		t.Errorf("UserID = %q, want %q (per-user attribution preserved)", m.UserID, "user-1")
	}
}

// TestAudited_FallsBackToProviderPriceForUnknownModel is the T1.3 guarantee:
// an out-of-catalog model is priced from PricePerMillionTokens instead of being
// silently recorded as $0 (which would let the daily budget never trip).
func TestAudited_FallsBackToProviderPriceForUnknownModel(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "something-else", Pricing: Pricing{InputCostPerM: 3, OutputCostPerM: 15}}},
		price:  Pricing{InputCostPerM: 2, OutputCostPerM: 10},
		resp:   &Response{TokensIn: 1_000_000, TokensOut: 1_000_000},
	}
	m := awaitMetric(t, stub, Request{Model: "brand-new-model"})
	if got, want := m.EstimatedCost, 12.0; got != want {
		t.Errorf("EstimatedCost = %v, want %v (provider fallback price), not $0", got, want)
	}
}
