package ai

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
)

func TestCatalogEntryFor(t *testing.T) {
	cases := map[string]string{
		"gpt-4o-2024-11-20":     "gpt-4o",
		"GPT-5-mini-2025-08-07": "gpt-5-mini",
		"gpt-4.1-2025-04-14":    "gpt-4.1",
		"deepseek-v3-0324":      "DeepSeek-V3-0324",
		"gpt-4o":                "gpt-4o",
		"o4-mini-2025-04-16":    "o4-mini",
		"gpt-5-turbo":           "", // sibling model: never guessed
		"gpt-4o-minipro":        "",
		"":                      "",
	}
	for served, want := range cases {
		entry, ok := catalogEntryFor("azure-foundry", served)
		if (want == "") != !ok {
			t.Errorf("catalogEntryFor(%q) ok=%v, want match=%v", served, ok, want != "")
			continue
		}
		if ok && entry.ID != want {
			t.Errorf("catalogEntryFor(%q) = %q, want %q", served, entry.ID, want)
		}
	}
}

func TestServedModelMemo_ScopedAndEnriching(t *testing.T) {
	resetServedModels()
	recordServedModel("u1", "azure-foundry", "my-gpt4o", "gpt-4o-2024-11-20")
	recordServedModel("u1", "openai", "gpt-4o", "gpt-4o-2024-11-20") // not an alias provider: ignored
	recordServedModel("u1", "azure-foundry", "same", "same")         // alias == model: ignored

	if _, ok := lookupServedModel("u2", "azure-foundry", "my-gpt4o"); ok {
		t.Error("another user's deployment name must not resolve")
	}
	if _, ok := lookupServedModel("u1", "openai", "gpt-4o"); ok {
		t.Error("non-alias providers are not memoised")
	}
	if _, ok := lookupServedModel("u1", "azure-foundry", "same"); ok {
		t.Error("an alias equal to its model is not memoised")
	}

	models := enrichServedModels("u1", "azure-foundry", []ModelInfo{
		{ID: "my-gpt4o", DisplayName: "my-gpt4o", ContextLimit: 128000},
		{ID: "unknown", DisplayName: "unknown", ContextLimit: 128000},
		{ID: "priced", DisplayName: "priced", ContextLimit: 1, Pricing: Pricing{InputCostPerM: 1}},
	})
	got := models[0]
	if got.ServedModel != "gpt-4o-2024-11-20" || got.DisplayName != "my-gpt4o · gpt-4o-2024-11-20" {
		t.Errorf("enriched entry = %+v", got)
	}
	if got.Pricing.InputCostPerM != 2.5 || got.MaxOutputTokens != 16384 {
		t.Errorf("pricing/limits not taken from the catalogue: %+v", got)
	}
	if models[1].ServedModel != "" || models[2].DisplayName != "priced" {
		t.Errorf("unknown or already-priced entries must be untouched: %+v %+v", models[1], models[2])
	}
	if out := enrichServedModels("u1", "openai", []ModelInfo{{ID: "gpt-4o", DisplayName: "GPT-4o"}}); out[0].DisplayName != "GPT-4o" {
		t.Error("non-alias providers are never enriched")
	}
}

// servedFakeProvider answers with a served model that differs from the
// request model, the Azure deployment situation.
type servedFakeProvider struct {
	id     string
	served string
	models []ModelInfo
}

func (f *servedFakeProvider) ID() string                     { return f.id }
func (f *servedFakeProvider) Name() string                   { return f.id }
func (f *servedFakeProvider) DefaultModel() string           { return "dep" }
func (f *servedFakeProvider) FreeModel() string              { return "" }
func (f *servedFakeProvider) ContextLimit() int              { return 128000 }
func (f *servedFakeProvider) PricePerMillionTokens() Pricing { return Pricing{} }
func (f *servedFakeProvider) SupportsTools() bool            { return true }
func (f *servedFakeProvider) EstimateTokens(s string) int    { return len(s) }
func (f *servedFakeProvider) Embed(context.Context, []string) ([][]float32, error) {
	return nil, nil
}
func (f *servedFakeProvider) Models(context.Context) ([]ModelInfo, error) { return f.models, nil }
func (f *servedFakeProvider) Chat(_ context.Context, req Request) (*Response, error) {
	return &Response{Content: "hi", TokensIn: 100, TokensOut: 50, ServedModel: f.served}, nil
}
func (f *servedFakeProvider) Stream(_ context.Context, req Request, onChunk func(Chunk)) error {
	onChunk(Chunk{Text: "hi"})
	onChunk(Chunk{Done: true, TokensIn: 100, TokensOut: 50, ServedModel: f.served})
	return nil
}

func TestAuditedProvider_PricesDeploymentByServedModel(t *testing.T) {
	resetServedModels()
	var mu sync.Mutex
	var metrics []interfaces.UsageMetric
	done := make(chan struct{}, 4)
	recorder := func(_ context.Context, m *interfaces.UsageMetric) error {
		mu.Lock()
		metrics = append(metrics, *m)
		mu.Unlock()
		done <- struct{}{}
		return nil
	}
	inner := &servedFakeProvider{id: "azure-foundry", served: "gpt-4o-2024-11-20", models: []ModelInfo{{ID: "dep", DisplayName: "dep", ContextLimit: 128000}}}
	p := NewAuditedProvider(inner, recorder, "u1", "azure-foundry")

	if _, err := p.Chat(context.Background(), Request{Model: "dep"}); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := p.Stream(context.Background(), Request{Model: "dep"}, func(Chunk) {}); err != nil {
		t.Fatal(err)
	}
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(metrics) != 2 {
		t.Fatalf("got %d usage metrics, want 2", len(metrics))
	}
	want := 100.0/1e6*2.5 + 50.0/1e6*10.0 // gpt-4o list price
	for _, m := range metrics {
		if m.Model != "dep" {
			t.Errorf("usage model = %q, want the request model", m.Model)
		}
		if diff := m.EstimatedCost - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("cost = %v, want %v (priced by the served model)", m.EstimatedCost, want)
		}
	}

	// The memo learned the mapping, so a fresh listing shows it.
	fresh := NewAuditedProvider(inner, recorder, "u1", "azure-foundry")
	models, _ := fresh.Models(context.Background())
	if models[0].DisplayName != "dep · gpt-4o-2024-11-20" || models[0].Pricing.InputCostPerM != 2.5 {
		t.Errorf("listing not enriched from the memo: %+v", models[0])
	}
}

// usageSink collects the usage rows an audited provider records (recording
// is asynchronous).
type usageSink struct {
	ch chan interfaces.UsageMetric
}

func newUsageSink() *usageSink { return &usageSink{ch: make(chan interfaces.UsageMetric, 16)} }

func (s *usageSink) recorder() UsageRecorder {
	return func(_ context.Context, m *interfaces.UsageMetric) error {
		s.ch <- *m
		return nil
	}
}

func (s *usageSink) next(t *testing.T) interfaces.UsageMetric {
	t.Helper()
	select {
	case m := <-s.ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("usage metric was not recorded")
		return interfaces.UsageMetric{}
	}
}

// countPricingFallback reads pad_ai_pricing_fallback_total{provider,model}
// out of the real registry.
func countPricingFallback(t *testing.T, provider, model string) float64 {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "pad_ai_pricing_fallback_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var prov, mdl string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "provider":
					prov = l.GetValue()
				case "model":
					mdl = l.GetValue()
				}
			}
			if prov == provider && mdl == model {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func costOf(tokensIn, tokensOut int, p Pricing) float64 {
	return float64(tokensIn)/1e6*p.InputCostPerM + float64(tokensOut)/1e6*p.OutputCostPerM
}

func assertCost(t *testing.T, label string, got, want float64) {
	t.Helper()
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("%s: EstimatedCost = %v, want %v", label, got, want)
	}
}

// Finding #2: the served-model price lookup ran for EVERY provider, so a free
// GitHub Models call answered as "gpt-4o-2024-11-20" was billed at the
// catalogue's gpt-4o list price. Only alias providers consult the served
// model; everyone else prices from their own listing exactly as before.
func TestAuditedProvider_ServedModelIgnoredForNonAliasProviders(t *testing.T) {
	resetServedModels()

	// GitHub Models: zero-priced live listing, catalogued served model → $0.
	sink := newUsageSink()
	gh := &servedFakeProvider{id: "github-models", served: "gpt-4o-2024-11-20", models: []ModelInfo{{ID: "gpt-4o", ContextLimit: 8192}}}
	ap := NewAuditedProvider(gh, sink.recorder(), "u1", "github-models")
	if _, err := ap.Chat(context.Background(), Request{Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "github-models Chat", sink.next(t).EstimatedCost, 0)
	if err := ap.Stream(context.Background(), Request{Model: "gpt-4o"}, func(Chunk) {}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "github-models Stream", sink.next(t).EstimatedCost, 0)

	// OpenAI: the listing price wins over the catalogue entry of the served model.
	listing := Pricing{InputCostPerM: 1, OutputCostPerM: 2}
	oa := &servedFakeProvider{id: "openai", served: "gpt-4o-2024-11-20", models: []ModelInfo{{ID: "gpt-4o", Pricing: listing}}}
	ap = NewAuditedProvider(oa, sink.recorder(), "u1", "openai")
	if _, err := ap.Chat(context.Background(), Request{Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "openai Chat", sink.next(t).EstimatedCost, costOf(100, 50, listing))
	if err := ap.Stream(context.Background(), Request{Model: "gpt-4o"}, func(Chunk) {}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "openai Stream", sink.next(t).EstimatedCost, costOf(100, 50, listing))
}

// Finding #9: for alias providers a zero-priced listing entry is not "found"
// (the counted fallback runs), and a model-router deployment is priced by the
// model that answered THIS call, never by the memo of an earlier one. Every
// fallback case is billed at the AliasReservationCeiling rates, not $0 — the
// fallback must not let an unknown model escape the daily budget.
func TestAuditedProvider_AliasPricingNeverUsesAStaleOrZeroListingPrice(t *testing.T) {
	resetServedModels()
	nano, _ := catalogEntryFor("azure-foundry", "gpt-5-nano")
	ceiling := AliasReservationCeiling("azure-foundry")

	// A router alias: turn 1 was routed to gpt-5-nano; turn 2 to a model the
	// catalogue does not know.
	recordServedModel("u1", "azure-foundry", "router", "gpt-5-nano-2025-08-07")
	recordServedModel("u1", "azure-foundry", "router", "gpt-4.1-nano-2025-04-14") // varied from now on
	sink := newUsageSink()
	inner := &servedFakeProvider{id: "azure-foundry", served: "claude-opus-4-1", models: []ModelInfo{{ID: "router", ContextLimit: 128000}}}
	ap := NewAuditedProvider(inner, sink.recorder(), "u1", "azure-foundry")
	if models, _ := ap.Models(context.Background()); models[0].Pricing == (Pricing{}) {
		t.Fatalf("fixture rot: the listing should carry the memo's price for display: %+v", models[0])
	}
	before := countPricingFallback(t, "azure-foundry", "deployment")
	if _, err := ap.Chat(context.Background(), Request{Model: "router"}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "router served by an uncatalogued model", sink.next(t).EstimatedCost, costOf(100, 50, ceiling))
	if after := countPricingFallback(t, "azure-foundry", "deployment"); after != before+1 {
		t.Errorf("pricing-fallback metric %v → %v, want +1", before, after)
	}

	// The same router when the response names no model (a stream cut before
	// Done): still not the previous call's price.
	inner.served = ""
	ap = NewAuditedProvider(inner, sink.recorder(), "u1", "azure-foundry")
	_, _ = ap.Models(context.Background())
	if _, err := ap.Chat(context.Background(), Request{Model: "router"}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "varied alias without a served model", sink.next(t).EstimatedCost, costOf(100, 50, ceiling))

	// Named "model-router": treated as a router from the first call.
	recordServedModel("u1", "azure-foundry", "model-router", "gpt-5-nano-2025-08-07")
	inner.models = []ModelInfo{{ID: "model-router", ContextLimit: 128000}}
	ap = NewAuditedProvider(inner, sink.recorder(), "u1", "azure-foundry")
	_, _ = ap.Models(context.Background())
	if _, err := ap.Chat(context.Background(), Request{Model: "model-router"}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "model-router without a served model", sink.next(t).EstimatedCost, costOf(100, 50, ceiling))

	// A zero-priced deployment whose served model is unknown: priced at the
	// reservation ceiling by the counted fallback, never $0.
	inner.served = "brand-new-model"
	inner.models = []ModelInfo{{ID: "dep", ContextLimit: 128000}}
	ap = NewAuditedProvider(inner, sink.recorder(), "u1", "azure-foundry")
	before = countPricingFallback(t, "azure-foundry", "deployment")
	if err := ap.Stream(context.Background(), Request{Model: "dep"}, func(Chunk) {}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "unknown served model", sink.next(t).EstimatedCost, costOf(100, 50, ceiling))
	if after := countPricingFallback(t, "azure-foundry", "deployment"); after != before+1 {
		t.Errorf("zero-priced listing entry must reach the counted fallback: metric %v → %v", before, after)
	}

	// A stable alias whose response names no model keeps the memo's price
	// (same model as before), and a served model always wins when catalogued.
	recordServedModel("u1", "azure-foundry", "stable", "gpt-5-nano-2025-08-07")
	inner.served = ""
	inner.models = []ModelInfo{{ID: "stable", ContextLimit: 128000}}
	ap = NewAuditedProvider(inner, sink.recorder(), "u1", "azure-foundry")
	if _, err := ap.Chat(context.Background(), Request{Model: "stable"}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "stable alias without a served model", sink.next(t).EstimatedCost, costOf(100, 50, nano.Pricing))
}

// Finding #39: an alias deployment named after a catalogued model (Azure's
// portal default) is priced by that name when nothing else prices it — the
// embedding deployment in API-key mode — while an uncatalogued name is not
// guessed: it reaches the counted fallback, billed at the reservation
// ceiling.
func TestAuditedProvider_AliasDeploymentPricedByItsCataloguedName(t *testing.T) {
	resetServedModels()
	small, _ := catalogEntryFor("azure-foundry", "text-embedding-3-small")
	sink := newUsageSink()
	inner := &servedFakeProvider{id: "azure-foundry", models: []ModelInfo{{ID: "text-embedding-3-small"}, {ID: "gpt-4o"}, {ID: "dep"}}}

	ap := NewAuditedProvider(inner, sink.recorder(), "u1", "azure-foundry").(*auditedProvider)
	ap.record(context.Background(), "text-embedding-3-small", "", "", 1000, 0)
	assertCost(t, "embedding deployment", sink.next(t).EstimatedCost, costOf(1000, 0, small.Pricing))

	gpt4o, _ := catalogEntryFor("azure-foundry", "gpt-4o")
	if _, err := ap.Chat(context.Background(), Request{Model: "gpt-4o"}); err != nil { // no served model reported
		t.Fatal(err)
	}
	assertCost(t, "chat deployment named after its model", sink.next(t).EstimatedCost, costOf(100, 50, gpt4o.Pricing))

	if _, err := ap.Chat(context.Background(), Request{Model: "dep"}); err != nil {
		t.Fatal(err)
	}
	assertCost(t, "uncatalogued deployment name", sink.next(t).EstimatedCost, costOf(100, 50, AliasReservationCeiling("azure-foundry")))
}

// Finding #10: a model router (by name, or an alias seen served by more than
// one model) must not take the last routed model's context window or output
// cap — the next call may be routed to a smaller-window model. Price and
// display still follow the memo.
func TestEnrichServedModels_RouterKeepsListingLimits(t *testing.T) {
	resetServedModels()
	recordServedModel("u1", "azure-foundry", "model-router", "gpt-4.1-nano-2025-04-14")
	recordServedModel("u1", "azure-foundry", "smart", "gpt-4.1-nano-2025-04-14")
	recordServedModel("u1", "azure-foundry", "my-gpt41", "gpt-4.1-2025-04-14")
	recordServedModel("u1", "azure-foundry", "my-gpt41", "gpt-4.1") // same model, dated vs undated: not varied

	list := func() []ModelInfo {
		return enrichServedModels("u1", "azure-foundry", []ModelInfo{
			{ID: "model-router", DisplayName: "model-router", ContextLimit: 128000},
			{ID: "smart", DisplayName: "smart", ContextLimit: 128000},
			{ID: "my-gpt41", DisplayName: "my-gpt41", ContextLimit: 128000},
		})
	}
	nano, _ := catalogEntryFor("azure-foundry", "gpt-4.1-nano")

	models := list()
	router := models[0]
	if router.ContextLimit != 128000 || router.MaxOutputTokens != 0 {
		t.Errorf("model-router took the routed model's limits: %+v", router)
	}
	if router.ServedModel != "gpt-4.1-nano-2025-04-14" || router.Pricing != nano.Pricing || !strings.Contains(router.DisplayName, " · ") {
		t.Errorf("model-router must still show the routed model and its price: %+v", router)
	}
	if models[2].ContextLimit != 1047576 || models[2].MaxOutputTokens != 32768 {
		t.Errorf("a stable alias keeps its model's window: %+v", models[2])
	}

	// A renamed router: stable after one observation, varied after a second model.
	if models[1].ContextLimit != 1047576 {
		t.Fatalf("fixture rot: one observation is not yet a router: %+v", models[1])
	}
	recordServedModel("u1", "azure-foundry", "smart", "o4-mini-2025-04-16")
	models = list()
	if models[1].ContextLimit != 128000 || models[1].MaxOutputTokens != 0 {
		t.Errorf("alias served by two models kept a routed model's limits: %+v", models[1])
	}
	recordServedModel("u1", "azure-foundry", "smart", "gpt-4.1-nano-2025-04-14")
	if models = list(); models[1].ContextLimit != 128000 {
		t.Errorf("varied is sticky within the TTL: %+v", models[1])
	}

	// Through the audited wrapper, the chat path's context window stays put.
	inner := &servedFakeProvider{id: "azure-foundry", served: "gpt-4.1-nano-2025-04-14", models: []ModelInfo{{ID: "model-router", ContextLimit: 128000}}}
	ap := NewAuditedProvider(inner, nil, "u1", "azure-foundry")
	if _, err := ap.Chat(context.Background(), Request{Model: "model-router"}); err != nil {
		t.Fatal(err)
	}
	fresh := NewAuditedProvider(inner, nil, "u1", "azure-foundry")
	if got := ContextLimitFor(context.Background(), fresh, "model-router"); got != 128000 {
		t.Errorf("ContextLimitFor(model-router) = %d, want 128000", got)
	}
	if got := ModelMaxOutputTokens(context.Background(), fresh, "model-router"); got != 0 {
		t.Errorf("ModelMaxOutputTokens(model-router) = %d, want 0 (provider default)", got)
	}
}
