package ai

import (
	"context"
	"testing"
)

// TestProviderCatalogConsistency locks the invariants whose violation produced
// the model-drift bugs (T1.2/T1.3): a provider's advertised default/free model
// must actually exist in its own catalog, and every catalogued model must be
// priced and sized. A default that isn't in Models() means the cost meter can't
// price it (silently $0 → the daily budget never trips); a zero ContextLimit
// breaks the request-clamping math. This test runs over every registered
// provider so a future catalog edit that breaks these invariants fails CI.
func TestProviderCatalogConsistency(t *testing.T) {
	ids := []string{"demo"}
	for _, meta := range AvailableProviders() {
		ids = append(ids, meta.ID)
	}

	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			p := GetMetadataProvider(id)
			if p == nil {
				t.Fatalf("GetMetadataProvider(%q) returned nil", id)
			}

			models, err := p.Models(context.Background())
			if err != nil {
				t.Fatalf("%s.Models() error: %v", id, err)
			}
			if len(models) == 0 {
				t.Fatalf("%s advertises no models", id)
			}

			catalog := make(map[string]bool, len(models))
			for _, m := range models {
				if m.ID == "" {
					t.Errorf("%s has a model with an empty ID", id)
				}
				if m.DisplayName == "" {
					t.Errorf("%s model %q has an empty DisplayName", id, m.ID)
				}
				if m.ContextLimit <= 0 {
					t.Errorf("%s model %q has non-positive ContextLimit %d", id, m.ID, m.ContextLimit)
				}
				if m.MaxOutputTokens < 0 {
					t.Errorf("%s model %q has negative MaxOutputTokens %d", id, m.ID, m.MaxOutputTokens)
				}
				if m.Pricing.InputCostPerM < 0 || m.Pricing.OutputCostPerM < 0 {
					t.Errorf("%s model %q has negative pricing %+v", id, m.ID, m.Pricing)
				}
				catalog[m.ID] = true
			}

			// The default the UI selects must be one the provider actually serves,
			// otherwise the cost meter can't find its price (records $0) and the
			// budget guard is defeated for that model.
			if def := p.DefaultModel(); !catalog[def] {
				t.Errorf("%s DefaultModel %q is not in its own catalog %v", id, def, keys(catalog))
			}
			// FreeModel is optional, but when set must also be a real catalog entry.
			if free := p.FreeModel(); free != "" && !catalog[free] {
				t.Errorf("%s FreeModel %q is not in its own catalog %v", id, free, keys(catalog))
			}
		})
	}
}

// TestCatalog_CoversAllProviders ensures every registered (non-demo) provider has
// a central catalog entry, so a newly added provider can't silently ship with an
// empty model list.
func TestCatalog_CoversAllProviders(t *testing.T) {
	for _, meta := range AvailableProviders() {
		if len(modelCatalog[meta.ID]) == 0 {
			t.Errorf("provider %q has no modelCatalog entry", meta.ID)
		}
	}
}

// TestCatalogModels_ReturnsCopy guards that callers can't mutate the shared
// catalog through the returned slice.
func TestCatalogModels_ReturnsCopy(t *testing.T) {
	got := catalogModels("claude")
	if len(got) == 0 {
		t.Fatal("expected claude models")
	}
	got[0].Pricing.InputCostPerM = 99999
	if catalogModels("claude")[0].Pricing.InputCostPerM == 99999 {
		t.Error("catalogModels returned a mutable view of the shared catalog")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
