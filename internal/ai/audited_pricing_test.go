package ai

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
)

// awaitAliasedMetric runs one Chat through an audited wrapper whose provider
// label is an alias provider (azure-foundry), so pricingFor walks the alias
// path. Recording is async; the metric arrives on the channel.
func awaitAliasedMetric(t *testing.T, stub *auditStub, req Request) *interfaces.UsageMetric {
	t.Helper()
	ch := make(chan *interfaces.UsageMetric, 1)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error { ch <- m; return nil }
	ap := NewAuditedProvider(stub, rec, "user-1", azureFoundryProviderID)
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

// Azure AI Foundry is unpriced (cost is read in the Azure portal): whatever
// the served model — uncatalogued, a current pro tier, or a router's pick —
// the usage row records the token counts, a $0 cost and the served model,
// and the pricing-fallback metric never moves for the alias provider.
func TestAudited_AzureTurnRecordsTokensAtZeroCost(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	for _, served := range []string{"uncatalogued-served-model", "gpt-5-pro-2025-10-06", "gpt-5.5-2026-04-24", ""} {
		stub := &auditStub{
			models: []ModelInfo{{ID: "my-deploy"}},
			resp:   &Response{TokensIn: 1_000_000, TokensOut: 500_000, ServedModel: served},
		}
		before := countPricingFallback(t, azureFoundryProviderID, "deployment")

		m := awaitAliasedMetric(t, stub, Request{Model: "my-deploy"})

		if m.PromptTokens != 1_000_000 || m.CompletionTokens != 500_000 {
			t.Errorf("served %q: tokens = %d/%d, want 1000000/500000", served, m.PromptTokens, m.CompletionTokens)
		}
		if m.EstimatedCost != 0 {
			t.Errorf("served %q: EstimatedCost = %v, want 0 (azure-foundry is unpriced)", served, m.EstimatedCost)
		}
		if served != "" && m.ServedModel != served {
			t.Errorf("served %q: recorded ServedModel = %q", served, m.ServedModel)
		}
		if after := countPricingFallback(t, azureFoundryProviderID, "deployment"); after != before {
			t.Errorf("served %q: pricing-fallback metric moved by %v, want 0", served, after-before)
		}
	}
}

// UsagePricing / ReservationPricing for an alias provider: a zero rate that
// counts as found (the done payload carries no price) and nothing is reserved.
func TestAudited_AzureUsageAndReservationPricingAreZero(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	stub := &auditStub{models: []ModelInfo{{ID: "my-deploy"}}}
	ap := NewAuditedProvider(stub, nil, "user-1", azureFoundryProviderID).(*auditedProvider)
	if got, ok := ap.UsagePricing(context.Background(), "my-deploy", "gpt-5.4-pro"); !ok || got != (Pricing{}) {
		t.Errorf("UsagePricing = %+v %v, want zero found", got, ok)
	}
	if got := ap.ReservationPricing(context.Background(), "my-deploy"); got != (Pricing{}) {
		t.Errorf("ReservationPricing = %+v, want zero", got)
	}
}

// A non-alias provider whose listing does not know the model falls back to
// its own provider-wide rate — $0 here — and records exactly that (its model
// ids are model names, so the fallback is the provider's own statement).
func TestAudited_ZeroFallbackNonAliasStaysZero(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "something-else"}},
		resp:   &Response{TokensIn: 1000, TokensOut: 1000, ServedModel: "whatever"},
	}
	m := awaitMetric(t, stub, Request{Model: "brand-new-model"})
	if m.EstimatedCost != 0 {
		t.Errorf("EstimatedCost = %v, want 0 (non-alias providers keep their own fallback)", m.EstimatedCost)
	}
}

// The pricing-fallback metric's model label stays a closed set: alias
// providers collapse to "deployment", non-alias providers keep a model's own
// name only while the catalogue knows it — the request model is
// client-chosen, and one unique string per fallback used to mint one permanent
// Prometheus series per name (the trap aiUsageCostTotal's comment describes).
func TestPricingMetricModel_BoundedLabels(t *testing.T) {
	for _, tc := range []struct {
		provider, model, want string
	}{
		{"azure-foundry", "some-user-deployment", "deployment"},
		{"azure-foundry", "gpt-4o", "deployment"}, // alias collapse wins
		{"openai", "gpt-4o", "gpt-4o"},            // catalogued: the useful signal
		{"openai", "gpt-4o-2024-11-20", "gpt-4o-2024-11-20"},
		{"openai", "client-invented-name", "uncatalogued"},
		{"stub", "anything-at-all", "uncatalogued"}, // provider without a catalogue
	} {
		if got := pricingMetricModel(tc.provider, tc.model); got != tc.want {
			t.Errorf("pricingMetricModel(%q, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
		}
	}
}

// TestAudited_PricingFallbackMetricLabelCollapsesUncatalogued drives the real
// recording path: a non-alias provider's uncatalogued request model must bump
// pad_ai_pricing_fallback_total under the collapsed label, never under the
// raw name.
func TestAudited_PricingFallbackMetricLabelCollapsesUncatalogued(t *testing.T) {
	stub := &auditStub{
		models: []ModelInfo{{ID: "known"}},
		resp:   &Response{TokensIn: 10, TokensOut: 10},
	}
	awaitMetric(t, stub, Request{Model: "client-invented-name"})

	families, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sawUncatalogued bool
	for _, fam := range families {
		if fam.GetName() != "pad_ai_pricing_fallback_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() != "model" {
					continue
				}
				switch lp.GetValue() {
				case "uncatalogued":
					sawUncatalogued = true
				case "client-invented-name":
					t.Errorf("raw client-chosen model name became a metric series: %q", lp.GetValue())
				}
			}
		}
	}
	if !sawUncatalogued {
		t.Error("the fallback was not counted under the collapsed \"uncatalogued\" label")
	}
}
