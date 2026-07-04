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
	streamErr    error // returned after replaying streamChunks
}

func (s *auditStub) Chat(_ context.Context, _ Request) (*Response, error) { return s.resp, nil }
func (s *auditStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	for _, c := range s.streamChunks {
		onChunk(c)
	}
	return s.streamErr
}
func (s *auditStub) Models(_ context.Context) ([]ModelInfo, error) { return s.models, nil }
func (s *auditStub) PricePerMillionTokens() Pricing                { return s.price }
func (s *auditStub) ID() string                                    { return "stub" }
func (s *auditStub) EstimateTokens(t string) int                   { return len(t) / 4 }

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

// TestAudited_PricesFromCatalogWhenModelKnown guards the catalog-price path.
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

// TestAudited_RecordsEstimatedUsageOnTruncatedStream: a stream that ends
// without a Done chunk (truncation, mid-stream error) still consumed provider
// tokens — usage must be recorded from estimates instead of silently counting
// as $0 against the daily budget.
func TestAudited_RecordsEstimatedUsageOnTruncatedStream(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
		streamChunks: []Chunk{{Text: "some partial output before the stream died"}},
		streamErr:    errStreamTruncated("stub"),
	}
	ch := make(chan *interfaces.UsageMetric, 1)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error {
		ch <- m
		return nil
	}
	ap := NewAuditedProvider(stub, rec, "user-1", "stub")

	req := Request{Model: "m", Messages: []Message{{Role: "user", Content: "a reasonably sized user prompt"}}}
	if err := ap.Stream(context.Background(), req, func(Chunk) {}); err == nil {
		t.Fatal("expected the truncation error to propagate")
	}

	select {
	case m := <-ch:
		if m.CompletionTokens == 0 {
			t.Error("CompletionTokens = 0, want an estimate from the streamed text")
		}
		if m.PromptTokens == 0 {
			t.Error("PromptTokens = 0, want an estimate from the request")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no usage metric recorded for the truncated stream")
	}
}

// TestAudited_NoUsageForStreamThatNeverStarted: a failure before the provider
// produced anything (circuit open, connection refused) must not bill an
// input-token estimate for a call that consumed nothing.
func TestAudited_NoUsageForStreamThatNeverStarted(t *testing.T) {
	stub := &auditStub{
		models:    []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
		streamErr: ErrCircuitOpen,
	}
	recorded := make(chan *interfaces.UsageMetric, 1)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error {
		recorded <- m
		return nil
	}
	ap := NewAuditedProvider(stub, rec, "user-1", "stub")

	if err := ap.Stream(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "prompt"}}}, func(Chunk) {}); err == nil {
		t.Fatal("expected the stream error to propagate")
	}
	select {
	case m := <-recorded:
		t.Fatalf("usage recorded for a stream that never started: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestAudited_RecorderBoundedUnderSaturation is the regression test for the
// unbounded-goroutine bug: record() used to spawn a goroutine per finished AI
// request that then blocked on recordSem (size 16). A slow recorder under load
// piled up goroutines without limit. After the fix the semaphore acquire is
// non-blocking — at most maxConcurrentRecords recorders run at once, and the
// rest of the metrics are dropped (not parked as goroutines).
func TestAudited_RecorderBoundedUnderSaturation(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
		resp:   &Response{TokensIn: 10, TokensOut: 10},
	}

	const oversubscribe = maxConcurrentRecords * 8 // well beyond the semaphore cap

	started := make(chan struct{}, oversubscribe)
	release := make(chan struct{})
	rec := func(_ context.Context, _ *interfaces.UsageMetric) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // block until the test lets all recorders finish
		return nil
	}

	ap := NewAuditedProvider(stub, rec, "user-1", "stub")

	// Fire many Chats; each attempts an async record. Chat returns immediately.
	for i := 0; i < oversubscribe; i++ {
		if _, err := ap.Chat(context.Background(), Request{Model: "m"}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
	}

	// Wait for the recorder-started count to reach the cap, then confirm it
	// plateaus there (the remainder must be dropped, not parked as goroutines).
	deadline := time.Now().Add(2 * time.Second)
	for len(started) < maxConcurrentRecords && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := len(started); got != maxConcurrentRecords {
		t.Fatalf("recorder started %d times, want exactly %d (the semaphore cap)", got, maxConcurrentRecords)
	}

	// Give the dropped paths a moment to run; the started count must NOT grow
	// past the cap — that would mean extra recorders were parked, not dropped.
	time.Sleep(30 * time.Millisecond)
	if got := len(started); got > maxConcurrentRecords {
		t.Errorf("recorder started %d times, exceeded cap %d (drops not working)", got, maxConcurrentRecords)
	}

	// Release the in-flight recorders so the goroutines exit cleanly.
	close(release)
}
