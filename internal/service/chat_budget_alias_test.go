package service

import (
	"context"
	"math"
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

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// Finding #8: Azure's provider-wide price is $0 on purpose, so pricing the
// in-flight reservation by PricePerMillionTokens reserved nothing and the
// concurrent-stream / tool-loop overspend guards were off for azure-foundry.
func TestInFlightPricing_AliasProviderReservesDeploymentPriceOrCeiling(t *testing.T) {
	ctx := context.Background()
	azure := &aliasPricedProvider{
		FakeProvider: &testutil.FakeProvider{},
		id:           ai.AzureFoundryProviderID,
		listing: []ai.ModelInfo{
			{ID: "dep", Pricing: ai.Pricing{InputCostPerM: 2, OutputCostPerM: 8}},
			{ID: "unpriced"},
		},
	}

	// The chosen deployment's listing price.
	if got := inFlightPricing(ctx, azure, "dep"); got != (ai.Pricing{InputCostPerM: 2, OutputCostPerM: 8}) {
		t.Errorf("priced deployment reserves at %+v, want its listing price", got)
	}
	if got, want := inFlightCost(inFlightPricing(ctx, azure, "dep"), 0, 4096), 4096*8/1e6; !approxEqual(got, want) {
		t.Errorf("reservation = %v, want %v", got, want)
	}
	// An empty request model means the provider default deployment.
	if got := inFlightPricing(ctx, azure, ""); got.OutputCostPerM != 8 {
		t.Errorf("default deployment reserves at %+v, want the listing price of %q", got, azure.DefaultModel())
	}

	// Unknown price (unpriced or unlisted deployment): the catalogue ceiling.
	ceiling := ai.AliasReservationCeiling(ai.AzureFoundryProviderID)
	if ceiling.InputCostPerM <= 0 || ceiling.OutputCostPerM <= 0 {
		t.Fatalf("ceiling must be a real rate, got %+v", ceiling)
	}
	for _, model := range []string{"unpriced", "missing"} {
		if got := inFlightPricing(ctx, azure, model); got != ceiling {
			t.Errorf("%s reserves at %+v, want the ceiling %+v", model, got, ceiling)
		}
	}
	if inFlightCost(inFlightPricing(ctx, azure, "unpriced"), 0, 4096) <= 0 {
		t.Error("an Azure stream must reserve a non-zero amount")
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
	if ai.AliasReservationCeiling("openai") != (ai.Pricing{}) {
		t.Error("the ceiling applies to alias providers only")
	}
}

// End to end through the tool loop: an Azure stream publishes a non-zero
// reservation on the concurrent-stream ledger, so another org member's turn
// near the cap is refused while the stream itself may continue.
func TestRunToolLoop_AzureStreamReservesInFlightBudget(t *testing.T) {
	svc := &ChatService{
		analysisCache: &AnalysisService{},
		backend:       &testutil.FakeBackend{DailyUsage: 0.95},
		settings:      &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 1.0}}},
	}
	ctl := &streamCtl{ownerID: "user-1", orgID: "org-1"}
	svc.activeStreams.Store("azure", ctl)
	ctx := context.Background()

	var seenInflight float64
	var otherErr error
	azure := &aliasPricedProvider{
		FakeProvider: &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "done", TokensIn: 10, TokensOut: 5}}},
		id:           ai.AzureFoundryProviderID,
		listing:      []ai.ModelInfo{{ID: "chat1"}}, // model not known yet
	}
	azure.onStream = func() {
		seenInflight = ctl.inflight()
		otherErr = svc.enforceBudget(ctx, "user-2", "org-1", 0, nil)
	}
	emit, events := captureEvents()

	svc.runToolLoop(ctx, azure, ai.Request{Model: "chat1", OrgID: "org-1", Messages: []ai.Message{{Role: "user", Content: "hi"}}},
		"user-1", &ai.ToolContext{Ctx: ctx, Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	want := inFlightCost(ai.AliasReservationCeiling(ai.AzureFoundryProviderID), 0, maxOutputTokens(ai.Request{}))
	if want <= 0.05 {
		t.Fatalf("fixture rot: ceiling reservation %v no longer crosses the $1 cap from $0.95", want)
	}
	if !approxEqual(seenInflight, want) {
		t.Errorf("published reservation = %v, want %v (catalogue ceiling × one max response)", seenInflight, want)
	}
	if otherErr == nil {
		t.Error("another org member near the cap must be refused while the Azure stream's reservation is in flight")
	}
	for _, ev := range events() {
		if ev.typ == "error" {
			t.Errorf("the reserving stream itself must not be refused: %+v", ev.data)
		}
	}
}

// Through a real Azure AI Foundry provider (unresolvable test host, so the
// listing degrades to the configured deployment): the unpriced configured
// deployment reserves at the ceiling, never $0.
func TestInFlightPricing_RealAzureProviderNeverReservesZero(t *testing.T) {
	configureAzureForTests(t)
	p := ai.NewAzureFoundryProvider(azureTestDoc)
	if got, want := inFlightPricing(context.Background(), p, "chat1"), ai.AliasReservationCeiling(ai.AzureFoundryProviderID); got != want {
		t.Errorf("reservation price = %+v, want the ceiling %+v", got, want)
	}
}
