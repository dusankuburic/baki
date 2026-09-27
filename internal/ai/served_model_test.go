package ai

import (
	"context"
	"sync"
	"testing"

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
