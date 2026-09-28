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

// An alias provider's own PricePerMillionTokens is deliberately zero (the
// deployment name says nothing about the model behind it), so a served model
// the catalogue doesn't know used to be billed $0 — escaping the daily budget
// except for the in-flight reservation. The zero fallback must be replaced by
// the AliasReservationCeiling rates: overstating is the safe direction for a
// spend cap, and the recorded cost matches what the reservation held.
func TestAudited_UncataloguedServedModelPricedAtReservationCeiling(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	stub := &auditStub{
		models: []ModelInfo{{ID: "my-deploy"}}, // listing entry, no price
		resp:   &Response{TokensIn: 1_000_000, TokensOut: 1_000_000, ServedModel: "uncatalogued-served-model"},
	}

	m := awaitAliasedMetric(t, stub, Request{Model: "my-deploy"})

	ceiling := AliasReservationCeiling(azureFoundryProviderID)
	if ceiling == (Pricing{}) {
		t.Fatal("the azure-foundry catalogue must carry priced models for this test")
	}
	want := ceiling.InputCostPerM + ceiling.OutputCostPerM // 1M in + 1M out
	if m.EstimatedCost != want {
		t.Errorf("EstimatedCost = %v, want %v (the AliasReservationCeiling rates)", m.EstimatedCost, want)
	}
	if m.EstimatedCost == 0 {
		t.Error("uncatalogued served model billed $0 — it escapes the daily budget")
	}
}

// The ceiling fallback is alias-only: a provider whose fallback price is
// legitimately unknown still records $0 exactly as before (its model ids are
// model names, so the fallback is the provider's own statement).
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
