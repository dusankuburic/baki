package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// aliasPricedProvider is an alias provider (Azure AI Foundry) as the chat
// service sees it: no provider-wide price, per-deployment prices only in its
// listing. onStream runs before each streamed turn.
type aliasPricedProvider struct {
	*testutil.FakeProvider
	id       string
	listing  []ai.ModelInfo
	price    ai.Pricing
	onStream func()
}

func (p *aliasPricedProvider) ID() string                        { return p.id }
func (p *aliasPricedProvider) DefaultModel() string              { return "dep" }
func (p *aliasPricedProvider) PricePerMillionTokens() ai.Pricing { return p.price }
func (p *aliasPricedProvider) Models(context.Context) ([]ai.ModelInfo, error) {
	return p.listing, nil
}
func (p *aliasPricedProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	if p.onStream != nil {
		p.onStream()
	}
	return p.FakeProvider.Stream(ctx, req, onChunk)
}

// Azure AI Foundry is not priced by baki (its cost is read in the Azure
// portal and the daily budget does not count it), so an alias provider
// reserves $0 whatever its listing carries.
func TestInFlightPricing_AliasProviderReservesNothing(t *testing.T) {
	ctx := context.Background()
	azure := &aliasPricedProvider{
		FakeProvider: &testutil.FakeProvider{},
		id:           ai.AzureFoundryProviderID,
		listing: []ai.ModelInfo{
			{ID: "dep", Pricing: ai.Pricing{InputCostPerM: 2, OutputCostPerM: 8}},
			{ID: "unpriced"},
		},
	}
	for _, model := range []string{"dep", "unpriced", "missing", ""} {
		if got := inFlightPricing(ctx, azure, model); got != (ai.Pricing{}) {
			t.Errorf("%q reserves at %+v, want no price", model, got)
		}
		if got := inFlightCost(inFlightPricing(ctx, azure, model), 1000, 4096); got != 0 {
			t.Errorf("%q reservation = %v, want 0", model, got)
		}
	}
}

// Every provider whose model ids are model names keeps its provider-wide
// rate — also a free one whose listing happens to carry a price.
func TestInFlightPricing_OtherProvidersKeepProviderRate(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		id    string
		price ai.Pricing
	}{
		{"openai", ai.Pricing{InputCostPerM: 2.5, OutputCostPerM: 10}},
		{"github-models", ai.Pricing{}},
		{"copilot", ai.Pricing{}},
	} {
		p := &aliasPricedProvider{
			FakeProvider: &testutil.FakeProvider{},
			id:           tc.id,
			price:        tc.price,
			listing:      []ai.ModelInfo{{ID: "dep", Pricing: ai.Pricing{InputCostPerM: 7, OutputCostPerM: 70}}},
		}
		if got := inFlightPricing(ctx, p, "dep"); got != tc.price {
			t.Errorf("%s reserves at %+v, want its provider-wide %+v", tc.id, got, tc.price)
		}
	}
}

// End to end through the tool loop: an Azure stream reserves nothing on the
// concurrent-stream ledger, so another org member's turn near the cap is not
// refused on its account, and the stream itself is not refused either.
func TestRunToolLoop_AzureStreamReservesNothing(t *testing.T) {
	svc := &ChatService{
		analysisCache: &AnalysisService{},
		backend:       &testutil.FakeBackend{DailyUsage: 0.95},
		settings:      &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 1.0}}},
	}
	ctl := &streamCtl{ownerID: "user-1", orgID: "org-1"}
	svc.activeStreams.Store("azure", ctl)
	svc.aiBudget().hold(ctl)
	ctx := context.Background()

	var seenInflight float64
	var otherErr error
	azure := &aliasPricedProvider{
		FakeProvider: &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "done", TokensIn: 10, TokensOut: 5}}},
		id:           ai.AzureFoundryProviderID,
		listing:      []ai.ModelInfo{{ID: "chat1"}},
	}
	azure.onStream = func() {
		seenInflight = ctl.inflight()
		otherErr = svc.enforceBudget(ctx, "user-2", "org-1", 0, nil)
	}
	emit, events := captureEvents()

	svc.runToolLoop(ctx, azure, ai.Request{Model: "chat1", OrgID: "org-1", Messages: []ai.Message{{Role: "user", Content: "hi"}}},
		"user-1", &ai.ToolContext{Ctx: ctx, Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	if seenInflight != 0 {
		t.Errorf("published reservation = %v, want 0 (Azure is unpriced)", seenInflight)
	}
	if otherErr != nil {
		t.Errorf("an unpriced Azure stream must not push another member over the cap: %v", otherErr)
	}
	for _, ev := range events() {
		if ev.typ == "error" {
			t.Errorf("the stream itself must not be refused: %+v", ev.data)
		}
	}
}

// Through a real Azure AI Foundry provider (unresolvable test host, so the
// listing degrades to the configured deployment): it reserves nothing.
func TestInFlightPricing_RealAzureProviderReservesNothing(t *testing.T) {
	configureAzureForTests(t)
	p := ai.NewAzureFoundryProvider(azureTestDoc)
	if got := inFlightPricing(context.Background(), p, "chat1"); got != (ai.Pricing{}) {
		t.Errorf("reservation price = %+v, want none", got)
	}
}
