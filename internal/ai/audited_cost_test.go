package ai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
)

// gatherCounter reads one counter series out of the real registry.
func gatherCounter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	metric:
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if want, ok := labels[l.GetName()]; ok && want != l.GetValue() {
					continue metric
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// TestAuditedAzure_CustomEmbeddingDeploymentRecordsZeroCost: a custom-named
// Azure embedding deployment in API-key mode records the
// embedding deployment, its token count and $0 — and no pricing-fallback
// metric moves.
func TestAuditedAzure_CustomEmbeddingDeploymentRecordsItsTokens(t *testing.T) {
	az := azureFoundryProviderID
	srv := azurePricingServer(t, "gpt-4o-2024-11-20", "chat", "kb-embed")
	sink := newUsageSink()
	raw := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) {
		c.ChatDeployment = "chat"
		c.EmbeddingDeployment = "kb-embed"
	})
	texts := []string{"alpha beta gamma delta", "one two three four five six"}
	tokens := 0
	for _, s := range texts {
		tokens += raw.EstimateTokens(s)
	}
	ap := newAuditedProvider(raw, sink.recorder(), "u1", az)
	ap.adoptRawHints(raw)
	if _, err := ap.Embed(context.Background(), texts); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	m := sink.next(t)
	if m.Model != "kb-embed" {
		t.Errorf("usage model = %q, want the embedding deployment", m.Model)
	}
	if m.PromptTokens != tokens {
		t.Errorf("recorded %d tokens, want %d", m.PromptTokens, tokens)
	}
}

// TestAuditedStream_CountsHostedToolSteps: each completed hosted
// agent tool step raises pad_ai_hosted_tool_steps_total{provider,tool}; the
// start notifications do not.
func TestAuditedStream_CountsHostedToolSteps(t *testing.T) {
	labels := map[string]string{"provider": azureFoundryProviderID, "tool": "web_search_call"}
	before := gatherCounter(t, "pad_ai_hosted_tool_steps_total", labels)
	stub := &auditStub{streamChunks: []Chunk{
		{ToolEvent: &ToolEvent{Name: "web_search_call", Label: "Agent · web search"}},
		{ToolEvent: &ToolEvent{Name: "web_search_call", Label: "Agent · web search", Done: true, OK: true}},
		{ToolEvent: &ToolEvent{Name: "web_search_call", Label: "Agent · web search"}},
		{ToolEvent: &ToolEvent{Name: "web_search_call", Label: "Agent · web search", Done: true, OK: true}},
		{Text: "answer"},
		{Done: true, TokensIn: 5, TokensOut: 2},
	}}
	ap := NewAuditedProvider(stub, nil, "u1", azureFoundryProviderID)
	if err := ap.Stream(context.Background(), Request{Model: "agent:triage"}, func(Chunk) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := gatherCounter(t, "pad_ai_hosted_tool_steps_total", labels) - before; got != 2 {
		t.Errorf("hosted tool steps counted %v, want 2", got)
	}
}

// TestAuditedStream_CancelledBeforeContentStillBillsThePrompt (D-44 /
// V-price-1): a keepalive proves a 200 body was being parsed, so a Stop
// during a reasoning deployment's silent thinking phase records the prompt
// estimate (output 0) instead of nothing.
func TestAuditedStream_CancelledBeforeContentStillBillsThePrompt(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m"}},
		streamChunks: []Chunk{{}}, // Azure's role delta / prompt-filter annotation
		streamErr:    context.Canceled,
	}
	sink := newUsageSink()
	ap := NewAuditedProvider(stub, sink.recorder(), "user-1", "stub")
	req := Request{Model: "m", Messages: []Message{{Role: "user", Content: "a prompt long enough to count"}}}
	if err := ap.Stream(context.Background(), req, func(Chunk) {}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want the cancellation to propagate, got %v", err)
	}
	m := sink.next(t)
	if m.PromptTokens == 0 || m.CompletionTokens != 0 {
		t.Errorf("recorded %d/%d tokens, want the prompt estimate and no output", m.PromptTokens, m.CompletionTokens)
	}
}

// TestAudited_NegativeUsageClampedToZero (AZSEC-3): a negative token count
// from an allow-listed non-Microsoft host must not record a negative cost
// that lowers the caller's daily spend.
func TestAudited_NegativeUsageClampedToZero(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "m"}},
		resp:   &Response{TokensIn: -5, TokensOut: 7},
	}
	m := awaitMetric(t, stub, Request{Model: "m"})
	if m.PromptTokens != 0 || m.CompletionTokens != 7 {
		t.Errorf("recorded %+v, want prompt clamped to 0", m)
	}

	// Both negative: nothing to record.
	stub.resp = &Response{TokensIn: -5, TokensOut: -7}
	ap := NewAuditedProvider(stub, func(_ context.Context, m *interfaces.UsageMetric) error {
		t.Errorf("negative usage recorded: %+v", m)
		return nil
	}, "user-1", "stub").(*auditedProvider)
	if _, err := ap.Chat(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	ap.record(context.Background(), "m", "", "", usage{In: -3})
	time.Sleep(30 * time.Millisecond)
}

// TestAudited_StreamEstimateDoesNotRetainText: the fallback
// output estimate is counted per delta; the stream is no longer buffered in
// memory for an end-of-stream tokenizer pass.
func TestAudited_StreamEstimateDoesNotRetainText(t *testing.T) {
	const chunk = 4 << 10
	const chunks = 1024 // 4 MB streamed
	delta := strings.Repeat("r", chunk)
	stub := &lazyStreamStub{delta: delta, n: chunks}
	sink := newUsageSink()
	ap := NewAuditedProvider(stub, sink.recorder(), "user-1", "stub")

	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := ap.Stream(context.Background(), Request{Model: "m"}, func(Chunk) {}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	runtime.ReadMemStats(&after)
	if grew := after.HeapAlloc - before.HeapAlloc; grew > 1<<20 {
		t.Errorf("heap grew by %d bytes during a 4 MB stream — the text is being retained", grew)
	}
	m := sink.next(t)
	if want := chunks * (chunk / 4); m.CompletionTokens != want {
		t.Errorf("CompletionTokens = %d, want the per-delta sum %d", m.CompletionTokens, want)
	}
}

// lazyStreamStub emits the same reasoning delta n times (no per-chunk
// allocation) and ends with Done(0,0).
type lazyStreamStub struct {
	auditStub
	delta string
	n     int
}

func (s *lazyStreamStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	for range s.n {
		onChunk(Chunk{Reasoning: s.delta})
	}
	onChunk(Chunk{Done: true})
	return nil
}

func (s *lazyStreamStub) Models(context.Context) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m"}}, nil
}

// TestAuditedAzure_TruncatedRouterStreamRecordsRoutedModel: the served model
// rides on every content chunk, so a stream cut before Done (Stop,
// watchdog, truncation) still teaches the memo and records the routed model
// with the estimated tokens; Azure is unpriced, so the cost is $0.
func TestAuditedAzure_TruncatedRouterStreamRecordsRoutedModel(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"x","model":"gpt-5-nano-2025-08-07","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n"))
		// The connection ends here: no finish_reason, no [DONE].
	})
	raw := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ChatDeployment = "model-router" })
	sink := newUsageSink()
	ap := newAuditedProvider(raw, sink.recorder(), "u1", azureFoundryProviderID)
	ap.adoptRawHints(raw)

	err := ap.Stream(context.Background(), Request{Model: "model-router", Messages: []Message{{Role: "user", Content: "hi"}}}, func(Chunk) {})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want the truncation error, got %v", err)
	}
	m := sink.next(t)
	if m.PromptTokens == 0 || m.CompletionTokens == 0 {
		t.Errorf("recorded %d/%d tokens, want the estimates", m.PromptTokens, m.CompletionTokens)
	}
	if m.ServedModel != "gpt-5-nano-2025-08-07" {
		t.Errorf("recorded served model = %q, want the routed model", m.ServedModel)
	}
	if served, ok := lookupServedModel(ap.memoScope, azureFoundryProviderID, "model-router"); !ok || served != "gpt-5-nano-2025-08-07" {
		t.Errorf("memo after the truncated stream = %q %v, want the routed model", served, ok)
	}
}
