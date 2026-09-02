package ai

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// auditStub is a minimal Provider for exercising auditedProvider.record cost
// math: it returns a fixed response and a configurable catalog + default price.
type auditStub struct {
	Provider
	models       []ModelInfo
	modelsCalls  atomic.Int32 // counts Models() invocations (memoisation tests)
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
func (s *auditStub) Models(_ context.Context) ([]ModelInfo, error) {
	s.modelsCalls.Add(1)
	return s.models, nil
}
func (s *auditStub) PricePerMillionTokens() Pricing { return s.price }
func (s *auditStub) ID() string                     { return "stub" }
func (s *auditStub) EstimateTokens(t string) int    { return len(t) / 4 }

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

// TestAudited_ModelsMemoisedPerInstance proves the audited wrapper consults
// the inner provider's Models() at most once per request chain: a single
// tool-loop request records up to ~7 metrics (each pricing via Models) and
// also calls ContextLimitFor/ModelMaxOutputTokens — for wire-backed Models()
// implementations (GitHub Models pre-cache) that multiplied into a live HTTP
// GET per usage record.
func TestAudited_ModelsMemoisedPerInstance(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "m", ContextLimit: 8192, Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
		resp:   &Response{TokensIn: 100, TokensOut: 100},
	}
	ap := NewAuditedProvider(stub, func(_ context.Context, _ *interfaces.UsageMetric) error { return nil }, "user-1", "stub")

	// Simulate one request's access pattern: clamp lookups + several records.
	_ = ContextLimitFor(context.Background(), ap, "m")
	_ = ModelMaxOutputTokens(context.Background(), ap, "m")
	for i := 0; i < 5; i++ {
		if _, err := ap.Chat(context.Background(), Request{Model: "m"}); err != nil {
			t.Fatalf("Chat %d: %v", i, err)
		}
	}
	if n := stub.modelsCalls.Load(); n != 1 {
		t.Errorf("inner Models() called %d times for one audited instance, want exactly 1", n)
	}
}

// embedAuditStub extends auditStub with Embed + the embedding-model accessor,
// mirroring openaiBase/Gemini providers on the audited chain.
type embedAuditStub struct {
	auditStub
	embedModel string
	embedTexts [][]string
}

func (s *embedAuditStub) Embed(_ context.Context, text []string) ([][]float32, error) {
	s.embedTexts = append(s.embedTexts, text)
	out := make([][]float32, len(text))
	for i := range out {
		out[i] = []float32{0.1}
	}
	return out, nil
}
func (s *embedAuditStub) EmbeddingModel() string { return s.embedModel }

// awaitEmbedMetric runs one Embed through the audited wrapper and returns the
// recorded metric (recording is async).
func awaitEmbedMetric(t *testing.T, stub *embedAuditStub, texts []string) *interfaces.UsageMetric {
	t.Helper()
	ch := make(chan *interfaces.UsageMetric, 1)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error {
		ch <- m
		return nil
	}
	ap := NewAuditedProvider(stub, rec, "user-1", "stub")
	if _, err := ap.Embed(context.Background(), texts); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("embedding usage metric was not recorded")
		return nil
	}
}

// TestAuditedEmbed_RecordsUsage pins R2: embedding spend (RAG indexing +
// per-turn query embeddings) must hit usage_metrics and the daily budget.
// Before this, auditedProvider.Embed was a bare delegation — every embedding
// call was unbilled.
func TestAuditedEmbed_RecordsUsage(t *testing.T) {
	stub := &embedAuditStub{
		auditStub: auditStub{
			models: []ModelInfo{{ID: "text-embedding-3-small", Pricing: Pricing{InputCostPerM: 0.02}}},
		},
		embedModel: "text-embedding-3-small",
	}
	texts := []string{"alpha beta gamma delta", "one two three four five six"}
	m := awaitEmbedMetric(t, stub, texts)

	if m.CompletionTokens != 0 {
		t.Errorf("CompletionTokens = %d, want 0 (embeddings bill input only)", m.CompletionTokens)
	}
	// EstimateTokens is len/4 on the stub: "alpha beta gamma delta"=22 → 5,
	// "one two three four five six"=27 → 6. 11 total.
	if m.PromptTokens != 11 {
		t.Errorf("PromptTokens = %d, want 11 (sum of per-text estimates)", m.PromptTokens)
	}
	if m.Model != "text-embedding-3-small" {
		t.Errorf("Model = %q, want the embedding model name (catalog pricing)", m.Model)
	}
	if want := 11.0 / 1_000_000 * 0.02; m.EstimatedCost != want {
		t.Errorf("EstimatedCost = %v, want %v", m.EstimatedCost, want)
	}
	if m.OrgID != "" {
		t.Errorf("OrgID = %q, want empty (personal attribution — Embed carries no org context)", m.OrgID)
	}
	if len(stub.embedTexts) != 1 || len(stub.embedTexts[0]) != 2 {
		t.Errorf("inner Embed received wrong batch: %+v", stub.embedTexts)
	}
}

// TestAuditedEmbed_FailedEmbedRecordsNothing: the error path records no usage
// (the provider rejected the call — nothing was spent).
func TestAuditedEmbed_FailedEmbedRecordsNothing(t *testing.T) {
	stub := &failingEmbedStub{auditStub: auditStub{}}
	ap := NewAuditedProvider(stub, func(context.Context, *interfaces.UsageMetric) error {
		t.Error("no usage must be recorded for a failed Embed")
		return nil
	}, "user-1", "stub")
	if _, err := ap.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("want error propagated")
	}
	time.Sleep(20 * time.Millisecond) // any (wrong) async record would fire by now
}

type failingEmbedStub struct{ auditStub }

func (f *failingEmbedStub) Embed(context.Context, []string) ([][]float32, error) {
	return nil, fmt.Errorf("embed rejected")
}

// awaitStreamMetric drives one Stream through the audited wrapper and returns
// the recorded metric.
func awaitStreamMetric(t *testing.T, stub *auditStub, req Request) *interfaces.UsageMetric {
	t.Helper()
	ch := make(chan *interfaces.UsageMetric, 1)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error {
		ch <- m
		return nil
	}
	ap := NewAuditedProvider(stub, rec, "user-1", "stub")
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

// TestAuditedStream_DoneWithZeroUsageEstimates pins R3: OpenAI-COMPAT servers
// that ignore stream_options (older vLLM/Ollama/proxies; base URLs are
// env-overridable) end every stream with Done(0,0). Without the fallback,
// every such stream billed $0 — no metric, budget never trips.
func TestAuditedStream_DoneWithZeroUsageEstimates(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 2}}},
		streamChunks: []Chunk{{Text: "hello world, this is output"}, {Done: true}}, // Done(0,0)
	}
	req := Request{
		Model:        "m",
		SystemPrompt: "you are a stub",
		Messages:     []Message{{Role: "user", Content: "a question worth asking"}, {Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"credentials"}`)}}}},
	}
	m := awaitStreamMetric(t, stub, req)

	// Stub EstimateTokens = len/4.
	wantIn := len(req.SystemPrompt)/4 + len(req.Messages[0].Content)/4 + len(`{"query":"credentials"}`)/4
	wantOut := len("hello world, this is output") / 4
	if m.PromptTokens != wantIn {
		t.Errorf("PromptTokens = %d, want %d (system+content+tool_use args)", m.PromptTokens, wantIn)
	}
	if m.CompletionTokens != wantOut {
		t.Errorf("CompletionTokens = %d, want %d (estimated from streamed text)", m.CompletionTokens, wantOut)
	}
	if m.EstimatedCost == 0 {
		t.Error("EstimatedCost = 0 — stream billed nothing")
	}
}

// TestAuditedStream_DoneZeroNoContentRecordsNothing: Done(0,0) with NO
// delivered content is an empty response, not a usage event — nothing billed
// (mirrors the never-started gate on the no-Done path).
func TestAuditedStream_DoneZeroNoContentRecordsNothing(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m"}},
		streamChunks: []Chunk{{Done: true}}, // empty response, no usage
	}
	ap := NewAuditedProvider(stub, func(context.Context, *interfaces.UsageMetric) error {
		t.Error("no usage must be recorded for an empty Done(0,0) stream")
		return nil
	}, "user-1", "stub")
	if err := ap.Stream(context.Background(), Request{Model: "m"}, func(Chunk) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
}
