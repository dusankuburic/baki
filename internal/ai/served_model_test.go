package ai

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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
		"o3-deep-research-0626": "o3-deep-research",
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
		{ID: "priced", DisplayName: "priced", ContextLimit: 1},
	})
	got := models[0]
	if got.ServedModel != "gpt-4o-2024-11-20" || got.DisplayName != "my-gpt4o · gpt-4o-2024-11-20" {
		t.Errorf("enriched entry = %+v", got)
	}
	if got.MaxOutputTokens != 16384 {
		t.Errorf("limits not taken from the catalogue (or a price appeared for the unpriced azure-foundry): %+v", got)
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

func (f *servedFakeProvider) ID() string                  { return f.id }
func (f *servedFakeProvider) Name() string                { return f.id }
func (f *servedFakeProvider) DefaultModel() string        { return "dep" }
func (f *servedFakeProvider) FreeModel() string           { return "" }
func (f *servedFakeProvider) ContextLimit() int           { return 128000 }
func (f *servedFakeProvider) SupportsTools() bool         { return true }
func (f *servedFakeProvider) EstimateTokens(s string) int { return len(s) }
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

func TestAuditedProvider_AzureDeploymentRecordsServedModelAtZeroCost(t *testing.T) {
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
	for _, m := range metrics {
		if m.Model != "dep" {
			t.Errorf("usage model = %q, want the request model", m.Model)
		}
		if m.ServedModel != "gpt-4o-2024-11-20" {
			t.Errorf("recorded served model = %q, want the model that answered", m.ServedModel)
		}
		if m.PromptTokens != 100 || m.CompletionTokens != 50 {
			t.Errorf("tokens = %d/%d, want 100/50", m.PromptTokens, m.CompletionTokens)
		}
	}

	// The memo learned the mapping, so a fresh listing shows it.
	fresh := NewAuditedProvider(inner, recorder, "u1", "azure-foundry")
	models, _ := fresh.Models(context.Background())
	if models[0].DisplayName != "dep · gpt-4o-2024-11-20" {
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

// a model router (by name, or an alias seen served by more than
// one model) must not take the last routed model's context window or output
// cap — the next call may be routed to a smaller-window model. The served
// model and display still follow the memo (Azure carries no price).
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

	models := list()
	router := models[0]
	if router.ContextLimit != 128000 || router.MaxOutputTokens != 0 {
		t.Errorf("model-router took the routed model's limits: %+v", router)
	}
	if router.ServedModel != "gpt-4.1-nano-2025-04-14" || !strings.Contains(router.DisplayName, " · ") {
		t.Errorf("model-router must still show the routed model, without a price: %+v", router)
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
