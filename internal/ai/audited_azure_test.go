package ai

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// azurePricingServer answers the routes an audited Azure provider touches:
// chat completions (served by servedModel), embeddings, and the model listing
// (the configured deployments, all unpriced).
func azurePricingServer(t *testing.T, servedModel string, deployments ...string) *azureTestServer {
	t.Helper()
	return newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, body map[string]any) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			writeJSON(w, 200, chatCompletionJSON("hi", servedModel))
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			inputs, _ := body["input"].([]any)
			out := []map[string]any{}
			for range inputs {
				out = append(out, map[string]any{"embedding": []float32{0.1}})
			}
			writeJSON(w, 200, map[string]any{"data": out, "model": "text-embedding-3-small"})
		case strings.HasSuffix(r.URL.Path, "/models"):
			data := []map[string]any{}
			for _, d := range deployments {
				data = append(data, map[string]any{"id": d})
			}
			writeJSON(w, 200, map[string]any{"data": data})
		default:
			writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "NotFound"}})
		}
	})
}

// Finding #39 end to end: Embed through the real factory chain (tracing,
// retry and breaker wrappers hide EmbeddingModel) records the EMBEDDING
// deployment — not the chat deployment — priced by its catalogued name in
// API-key mode; an uncatalogued embedding deployment is not guessed: it is
// counted by the fallback metric and billed at the EMBEDDING ceiling
// (V-price-2: the chat ceiling overstated it 23-150×), so an unknown
// deployment cannot bill $0 and escape the daily budget.
func TestAuditedAzure_EmbedThroughFactoryChainPricedByDeploymentName(t *testing.T) {
	srv := azurePricingServer(t, "gpt-4o-2024-11-20", "chat", "text-embedding-3-small", "my-embed")
	small, _ := catalogEntryFor(azureFoundryProviderID, "text-embedding-3-small")
	texts := []string{"alpha beta gamma delta", "one two three four five six"}

	for _, tc := range []struct {
		deployment   string
		pricing      Pricing
		wantFallback bool
	}{
		{"text-embedding-3-small", small.Pricing, false},
		{"my-embed", AliasEmbeddingCeiling(azureFoundryProviderID), true},
	} {
		sink := newUsageSink()
		f := &ProviderFactory{recorder: sink.recorder()}
		raw := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) {
			c.ChatDeployment = "chat"
			c.EmbeddingDeployment = tc.deployment
		})
		tokens := 0
		for _, s := range texts {
			tokens += raw.EstimateTokens(s)
		}
		p := f.wrapChain(raw, "u1", azureFoundryProviderID)
		before := countPricingFallback(t, azureFoundryProviderID, "deployment")
		if _, err := p.Embed(context.Background(), texts); err != nil {
			t.Fatalf("%s: Embed: %v", tc.deployment, err)
		}
		m := sink.next(t)
		if m.Model != tc.deployment {
			t.Errorf("usage model = %q, want the embedding deployment %q", m.Model, tc.deployment)
		}
		assertCost(t, tc.deployment, m.EstimatedCost, costOf(tokens, 0, tc.pricing))
		fallbacks := countPricingFallback(t, azureFoundryProviderID, "deployment") - before
		if tc.wantFallback != (fallbacks == 1) {
			t.Errorf("%s: pricing-fallback metric moved by %v (fallback expected: %v)", tc.deployment, fallbacks, tc.wantFallback)
		}
	}
}

// Finding #45: the served-model memo is keyed by resource. A new resource's
// deployment with the same name must not inherit the old resource's model
// (window, output cap, price, label); usage rows stay attributed to the user.
func TestAuditedAzure_ServedModelMemoScopedToResource(t *testing.T) {
	r1 := azurePricingServer(t, "gpt-4.1-2025-04-14", "chat")
	r2 := azurePricingServer(t, "gpt-4o-2024-11-20", "chat")
	sink := newUsageSink()
	f := &ProviderFactory{recorder: sink.recorder()}
	build := func(srv *azureTestServer) Provider {
		cfg := azureKeyConfig(srv.URL)
		cfg.ChatDeployment = "chat"
		return f.wrapChain(newAzureFoundryProvider(cfg, nil, srv.Client()), "u1", azureFoundryProviderID)
	}
	ResetAzureFoundryCaches()

	if _, err := build(r1).Chat(context.Background(), Request{Model: "chat"}); err != nil {
		t.Fatalf("R1 chat: %v", err)
	}
	if m := sink.next(t); m.UserID != "u1" {
		t.Errorf("usage UserID = %q, want the user scope", m.UserID)
	}

	onR1 := build(r1)
	if got := ContextLimitFor(context.Background(), onR1, "chat"); got != 1047576 {
		t.Errorf("R1 chat window = %d, want gpt-4.1's 1047576 from the memo", got)
	}
	onR2 := build(r2)
	models, err := onR2.Models(context.Background())
	if err != nil {
		t.Fatalf("R2 models: %v", err)
	}
	for _, m := range models {
		if m.ID != "chat" {
			continue
		}
		if m.ContextLimit != 128000 || m.ServedModel != "" || strings.Contains(m.DisplayName, " · ") || m.Pricing != (Pricing{}) {
			t.Errorf("R2's chat inherited R1's mapping: %+v", m)
		}
	}
}

// slowListingEmbedStub embeds instantly but lists models slowly, the Azure
// cache-miss situation (several serial requests).
type slowListingEmbedStub struct {
	embedAuditStub
	release     chan struct{}
	listCtxDead atomic.Bool
}

func (s *slowListingEmbedStub) ID() string { return azureFoundryProviderID }
func (s *slowListingEmbedStub) Models(ctx context.Context) ([]ModelInfo, error) {
	select {
	case <-s.release:
	case <-time.After(3 * time.Second): // a synchronous caller would wait here
	}
	s.listCtxDead.Store(ctx.Err() != nil)
	return s.models, nil
}

// Finding #21: pricing an embedding may need the live model listing. The
// per-turn RAG query embeds under an 800 ms budget, so Embed returns as soon
// as the vectors arrive and prices/records on a detached, bounded context —
// the caller's cancellation neither delays the call nor cuts the listing.
func TestAuditedEmbed_PricesOnDetachedContext(t *testing.T) {
	resetServedModels()
	stub := &slowListingEmbedStub{
		embedAuditStub: embedAuditStub{
			auditStub:  auditStub{models: []ModelInfo{{ID: "emb", Pricing: Pricing{InputCostPerM: 0.5}}}},
			embedModel: "emb",
		},
		release: make(chan struct{}),
	}
	sink := newUsageSink()
	ap := NewAuditedProvider(stub, sink.recorder(), "u1", azureFoundryProviderID)

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	start := time.Now()
	if _, err := ap.Embed(ctx, []string{"abcdefgh"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("Embed blocked %v on the model listing", elapsed)
	}
	if ctx.Err() != nil {
		t.Error("the caller's budget was spent inside Embed")
	}
	cancel() // the RAG turn moves on (or ends) before pricing finishes
	close(stub.release)

	m := sink.next(t)
	assertCost(t, "detached embedding record", m.EstimatedCost, costOf(2, 0, Pricing{InputCostPerM: 0.5}))
	if stub.listCtxDead.Load() {
		t.Error("the listing ran on the caller's cancelled context")
	}
}

// The detached Embed record still honours the recorder slots: a panicking
// recorder frees its slot and never takes the process down.
func TestAuditedEmbed_DetachedRecordRecoversAndFreesSlot(t *testing.T) {
	stub := &embedAuditStub{
		auditStub:  auditStub{models: []ModelInfo{{ID: "emb", Pricing: Pricing{InputCostPerM: 1}}}},
		embedModel: "emb",
	}
	var calls atomic.Int32
	done := make(chan struct{}, 2)
	rec := func(context.Context, *interfaces.UsageMetric) error {
		defer func() { done <- struct{}{} }()
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return nil
	}
	ap := NewAuditedProvider(stub, rec, "u1", "stub").(*auditedProvider)
	for i := 0; i < 2; i++ {
		if _, err := ap.Embed(context.Background(), []string{"abcd"}); err != nil {
			t.Fatalf("Embed: %v", err)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("embedding usage was not recorded")
		}
	}
	deadline := time.Now().Add(time.Second)
	for len(ap.recordSem) != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := len(ap.recordSem); n != 0 {
		t.Errorf("%d recorder slots still held after the goroutines finished", n)
	}
}
