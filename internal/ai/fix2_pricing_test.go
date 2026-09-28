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

// TestAuditedReservationPricing_RouterReservesAtCeiling (D-13): the
// in-flight reservation rate follows pricingFor's rule — the listing price
// only for a stable alias; a router (by name, or observed varying) reserves at
// the ceiling even though its listing still DISPLAYS the last routed model's
// price. Non-alias providers keep their provider-wide rate.
func TestAuditedReservationPricing_RouterReservesAtCeiling(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	az := azureFoundryProviderID
	nano, _ := catalogEntryFor(az, "gpt-5-nano")
	ceiling := AliasRouterCeiling(az) // routers may route to Claude

	recordServedModel("u1", az, "model-router", "gpt-5-nano-2025-08-07")
	recordServedModel("u1", az, "smart", "gpt-5-nano-2025-08-07")
	recordServedModel("u1", az, "smart", "gpt-4.1-2025-04-14") // varied
	recordServedModel("u1", az, "stable", "gpt-5-nano-2025-08-07")
	inner := &servedFakeProvider{id: az, models: []ModelInfo{
		{ID: "model-router", DisplayName: "model-router", ContextLimit: 128000},
		{ID: "smart", DisplayName: "smart", ContextLimit: 128000},
		{ID: "stable", DisplayName: "stable", ContextLimit: 128000},
	}}
	ap := NewAuditedProvider(inner, nil, "u1", az)
	rp, ok := ap.(ReservationPricer)
	if !ok {
		t.Fatal("the audited wrapper must implement ReservationPricer")
	}
	ctx := context.Background()
	models, _ := ap.Models(ctx)
	if models[0].Pricing != nano.Pricing {
		t.Errorf("the router's listing must keep the memo's price for display: %+v", models[0])
	}
	if got := rp.ReservationPricing(ctx, "model-router"); got != ceiling {
		t.Errorf("model-router reserves at %+v, want the ceiling %+v", got, ceiling)
	}
	if got := rp.ReservationPricing(ctx, "smart"); got != ceiling {
		t.Errorf("a varied alias reserves at %+v, want the ceiling %+v", got, ceiling)
	}
	if got := rp.ReservationPricing(ctx, "stable"); got != nano.Pricing {
		t.Errorf("a stable alias reserves at %+v, want its listing price %+v", got, nano.Pricing)
	}
	if got := rp.ReservationPricing(ctx, ""); got != AliasReservationCeiling(az) {
		t.Errorf("an unknown plain default deployment reserves at %+v, want the Azure (non-router) ceiling", got)
	}

	other := NewAuditedProvider(&auditStub{price: Pricing{InputCostPerM: 1, OutputCostPerM: 2}}, nil, "u1", "stub").(ReservationPricer)
	if got := other.ReservationPricing(ctx, "anything"); got != (Pricing{InputCostPerM: 1, OutputCostPerM: 2}) {
		t.Errorf("non-alias provider reserves at %+v, want its provider rate", got)
	}
}

// TestAuditedUsagePricing_ResolvesLikeTheUsageRow (D-38): UsagePricing is the
// rate the turn was billed at — the served model first, the ceiling for an
// unknown model (the embedding ceiling for an embedding deployment), a free
// listing still "found" at $0, and false only when nothing prices the call.
func TestAuditedUsagePricing_ResolvesLikeTheUsageRow(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	ctx := context.Background()
	az := azureFoundryProviderID
	gpt4o, _ := catalogEntryFor(az, "gpt-4o")

	inner := &servedFakeProvider{id: az, models: []ModelInfo{{ID: "dep"}, {ID: "vectors", Embedding: true}}}
	up, ok := NewAuditedProvider(inner, nil, "u1", az).(UsagePricer)
	if !ok {
		t.Fatal("the audited wrapper must implement UsagePricer")
	}
	if got, ok := up.UsagePricing(ctx, "dep", "gpt-4o-2024-11-20"); !ok || got != gpt4o.Pricing {
		t.Errorf("served gpt-4o: got %+v %v, want the catalogue price", got, ok)
	}
	if got, ok := up.UsagePricing(ctx, "dep", "brand-new-model"); !ok || got != AliasReservationCeiling(az) {
		t.Errorf("unknown served model: got %+v %v, want the chat ceiling", got, ok)
	}
	if got, ok := up.UsagePricing(ctx, "vectors", ""); !ok || got != AliasEmbeddingCeiling(az) {
		t.Errorf("embedding deployment flagged by the listing: got %+v %v, want the embedding ceiling", got, ok)
	}

	gh := NewAuditedProvider(&auditStub{models: []ModelInfo{{ID: "gpt-4o"}}}, nil, "u1", "github-models").(UsagePricer)
	if got, ok := gh.UsagePricing(ctx, "gpt-4o", "gpt-4o-2024-11-20"); !ok || got != (Pricing{}) {
		t.Errorf("a free listing is found at $0: got %+v %v", got, ok)
	}
	none := NewAuditedProvider(&auditStub{}, nil, "u1", "stub").(UsagePricer)
	if _, ok := none.UsagePricing(ctx, "x", ""); ok {
		t.Error("nothing prices an unlisted model of a zero-rate non-alias provider")
	}
}

// TestAuditedAzure_CustomEmbeddingDeploymentFallsBackToEmbeddingCeiling
// (D-12 / V-price-2): a custom-named Azure embedding deployment in API-key
// mode is billed at the EMBEDDING ceiling ($0.13/M), not the chat ceiling
// ($3/M) that let one knowledge-base index exhaust the daily budget.
func TestAuditedAzure_CustomEmbeddingDeploymentFallsBackToEmbeddingCeiling(t *testing.T) {
	az := azureFoundryProviderID
	ceiling := AliasEmbeddingCeiling(az)
	if ceiling.InputCostPerM != 0.13 || ceiling.OutputCostPerM != 0 {
		t.Fatalf("AliasEmbeddingCeiling(azure) = %+v, want text-embedding-3-large's 0.13 in / 0 out", ceiling)
	}
	if AliasEmbeddingCeiling("openai") != (Pricing{}) {
		t.Error("the embedding ceiling is alias-only")
	}

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
	assertCost(t, "custom embedding deployment", m.EstimatedCost, costOf(tokens, 0, ceiling))
	if chat := costOf(tokens, 0, Pricing{InputCostPerM: 3.0}); m.EstimatedCost >= chat {
		t.Errorf("EstimatedCost %v is not below the chat ceiling's %v", m.EstimatedCost, chat)
	}
}

// TestAuditedStream_CountsHostedToolSteps (D-41): each completed hosted
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
		models:       []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
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
		models: []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
		resp:   &Response{TokensIn: -5, TokensOut: 7},
	}
	m := awaitMetric(t, stub, Request{Model: "m"})
	if m.PromptTokens != 0 || m.CompletionTokens != 7 || m.EstimatedCost <= 0 {
		t.Errorf("recorded %+v, want prompt clamped to 0 and a positive cost", m)
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
	ap.recordDetached(context.Background(), "m", "", "", -3, 0)
	time.Sleep(30 * time.Millisecond)
}

// TestAudited_StreamEstimateDoesNotRetainText (D-18 / L-ai-4): the fallback
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
	return []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}}, nil
}

// TestAuditedAzure_TruncatedRouterStreamPricedByRoutedModel (D-40): the served
// model rides on every content chunk, so a stream cut before Done (Stop,
// watchdog, truncation) is priced by the model that answered — not by the
// ceiling — and still teaches the memo.
func TestAuditedAzure_TruncatedRouterStreamPricedByRoutedModel(t *testing.T) {
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
	nano, _ := catalogEntryFor(azureFoundryProviderID, "gpt-5-nano")
	assertCost(t, "truncated router stream", m.EstimatedCost, costOf(m.PromptTokens, m.CompletionTokens, nano.Pricing))
	if ceil := costOf(m.PromptTokens, m.CompletionTokens, AliasReservationCeiling(azureFoundryProviderID)); m.EstimatedCost == ceil {
		t.Errorf("priced at the ceiling %v instead of the routed model", ceil)
	}
	if served, ok := lookupServedModel(ap.memoScope, azureFoundryProviderID, "model-router"); !ok || served != "gpt-5-nano-2025-08-07" {
		t.Errorf("memo after the truncated stream = %q %v, want the routed model", served, ok)
	}
}
