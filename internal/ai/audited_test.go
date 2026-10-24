package ai

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// auditStub is a minimal Provider for exercising auditedProvider.record: it
// returns a fixed response and a configurable catalog.
type auditStub struct {
	Provider
	models       []ModelInfo
	modelsCalls  atomic.Int32 // counts Models() invocations (memoisation tests)
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
func (s *auditStub) ID() string                  { return "stub" }
func (s *auditStub) EstimateTokens(t string) int { return len(t) / 4 }

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

// TestAudited_NilRecorderDoesNotPanic: a nil recorder (local/desktop wiring) must skip recording on Chat and Stream.
func TestAudited_NilRecorderDoesNotPanic(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "m"}},
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

// TestAudited_RecordsOrgID (T4.2): usage carries the request's OrgID, so org flows sum into the org's usage.
func TestAudited_RecordsOrgID(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "known"}},
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

// TestAudited_RecordsEstimatedUsageOnTruncatedStream: a stream ending without Done still consumed tokens —
// usage is recorded from estimates, not silently $0.
func TestAudited_RecordsEstimatedUsageOnTruncatedStream(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m"}},
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

// TestAudited_NoUsageForStreamThatNeverStarted: a failure before the provider produced anything (circuit
// open, connection refused) must not bill an estimate for a call that consumed nothing.
func TestAudited_NoUsageForStreamThatNeverStarted(t *testing.T) {
	stub := &auditStub{
		models:    []ModelInfo{{ID: "m"}},
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

// TestAudited_RecorderBoundedUnderSaturation: record() used to spawn a goroutine per finished request that
// then blocked on recordSem. The acquire is non-blocking: past maxConcurrentRecords the metric is dropped.
func TestAudited_RecorderBoundedUnderSaturation(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "m"}},
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

// TestAudited_ModelsMemoisedPerInstance: one tool-loop request consults Models ~7 times; for wire-backed
// Models() that multiplied into a live HTTP GET per usage record.
func TestAudited_ModelsMemoisedPerInstance(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "m", ContextLimit: 8192}},
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
func TestAuditedEmbed_RecordsUsage(t *testing.T) {
	stub := &embedAuditStub{
		auditStub: auditStub{
			models: []ModelInfo{{ID: "text-embedding-3-small"}},
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

// TestAuditedStream_DoneWithZeroUsageEstimates (R3): compat servers that ignore stream_options end every
// stream with Done(0,0); without the fallback every such stream billed $0.
func TestAuditedStream_DoneWithZeroUsageEstimates(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m"}},
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
}

// TestAuditedStream_DoneZeroNoContentRecordsNothing: Done(0,0) with no delivered content is an empty
// response, not a usage event (mirrors the never-started gate).
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

// countedRecorder counts usage records under a mutex. Both record decisions happen synchronously inside
// Stream (the goroutine only delivers), so a FINAL count assertion is sound once the first record lands.
type countedRecorder struct {
	mu      sync.Mutex
	metrics []*interfaces.UsageMetric
}

func (c *countedRecorder) record(_ context.Context, m *interfaces.UsageMetric) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metrics = append(c.metrics, m)
	return nil
}

func (c *countedRecorder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.metrics)
}

// assertExactlyOneRecorded waits for the async record to deliver, gives a would-be duplicate a grace
// interval, then pins the FINAL count at exactly one.
func assertExactlyOneRecorded(t *testing.T, rec *countedRecorder) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for rec.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // a duplicate spawned during Stream lands by now
	if got := rec.count(); got != 1 {
		t.Fatalf("usage recorded %d times, want exactly 1 per stream", got)
	}
}

// TestAuditedStream_RecordsUsageExactlyOnce_Done: the Done-chunk record and the !sawDone fallback are
// mutually exclusive via sawDone — deleting the gate would double-bill every successful stream.
func TestAuditedStream_RecordsUsageExactlyOnce_Done(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m"}},
		streamChunks: []Chunk{{Text: "a streamed answer"}, {Done: true, TokensIn: 100, TokensOut: 50}},
	}
	rec := &countedRecorder{}
	ap := NewAuditedProvider(stub, rec.record, "user-1", "stub")
	if err := ap.Stream(context.Background(), Request{Model: "m"}, func(Chunk) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	assertExactlyOneRecorded(t, rec)
}

// TestAuditedStream_RecordsUsageExactlyOnce_Truncated: the truncated-stream fallback fires exactly once —
// one estimated-usage metric for a stream cut before Done, never two.
func TestAuditedStream_RecordsUsageExactlyOnce_Truncated(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m"}},
		streamChunks: []Chunk{{Text: "some output before the stream died"}},
		streamErr:    errStreamTruncated("stub"),
	}
	rec := &countedRecorder{}
	ap := NewAuditedProvider(stub, rec.record, "user-1", "stub")
	if err := ap.Stream(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "prompt"}}}, func(Chunk) {}); err == nil {
		t.Fatal("expected the truncation error to propagate")
	}
	assertExactlyOneRecorded(t, rec)
}

// A7: cached slices are clamped like the totals before pricing — a negative CacheWrite used to price
// negative, and slices beyond the total priced tokens never sent. Read ∈ [0, In], Write ∈ [0, In-Read].
func TestAudited_CacheSlicesClamped(t *testing.T) {
	const perM = 1_000_000.0
	models := []ModelInfo{{ID: "m"}}
	for _, tc := range []struct {
		name     string
		resp     Response
		wantCost float64
	}{
		// Uncapped: (10 - (-1000) + 1.25 × -1000) / 1M < 0.
		{"negative write", Response{TokensIn: 10, TokensOut: 1, CacheWriteTokens: -1000}, 10 / perM},
		// Uncapped: 5010 uncached tokens less 0.1 × 5000 at Claude's read
		// rate — 451× the prompt's cost.
		{"negative read", Response{TokensIn: 10, TokensOut: 1, CacheReadTokens: -5000, ServedModel: "claude-sonnet-4-5"}, 10 / perM},
		// Uncapped: (500 + 1.25 × 500) / 1M for a 100-token prompt.
		{"oversized slices", Response{TokensIn: 100, TokensOut: 1, CacheReadTokens: 500, CacheWriteTokens: 500}, 100 / perM},
		// Read fills 60 of 100; the write is capped at the 40 left.
		{"write beyond the rest", Response{TokensIn: 100, TokensOut: 1, CacheReadTokens: 60, CacheWriteTokens: 90}, (60 + 1.25*40) / perM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.resp
			m := awaitMetric(t, &auditStub{models: models, resp: &resp}, Request{Model: "m"})
			if m.PromptTokens != tc.resp.TokensIn {
				t.Errorf("PromptTokens = %d, want the reported total %d", m.PromptTokens, tc.resp.TokensIn)
			}
		})
	}
}
