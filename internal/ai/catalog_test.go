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

// TestProviderFallbackPricing_MatchesDefaultModelCatalog locks the invariant
// that keeps the audited-provider pricing fallback honest: for a paid provider,
// the provider-wide PricePerMillionTokens must equal the catalog pricing of its
// DefaultModel. audited.record falls back to the provider-wide price when a
// model ID isn't in the catalog; if that fallback under-reports (GLM shipped
// 0.01/M against a 1.4/M default — ~140x under-billing), usage for new models
// is recorded at the wrong cost and the daily budget silently never trips.
//
// Free providers (github-models, copilot, demo) report a zero provider-wide
// price by design — their access is billed at $0 regardless of the catalog's
// reference prices — so they're exempt.
func TestProviderFallbackPricing_MatchesDefaultModelCatalog(t *testing.T) {
	for _, meta := range AvailableProviders() {
		t.Run(meta.ID, func(t *testing.T) {
			p := GetMetadataProvider(meta.ID)
			if p == nil {
				t.Fatalf("GetMetadataProvider(%q) returned nil", meta.ID)
			}
			fallback := p.PricePerMillionTokens()
			if fallback.InputCostPerM == 0 && fallback.OutputCostPerM == 0 {
				return // free provider — provider-wide $0 is intentional
			}

			models, err := p.Models(context.Background())
			if err != nil {
				t.Fatalf("%s.Models() error: %v", meta.ID, err)
			}
			def := p.DefaultModel()
			for _, m := range models {
				if m.ID != def {
					continue
				}
				if m.Pricing != fallback {
					t.Errorf("%s provider-wide pricing %+v != DefaultModel(%q) catalog pricing %+v — the out-of-catalog fallback would meter %s at the wrong cost",
						meta.ID, fallback, def, m.Pricing, def)
				}
				return
			}
			t.Errorf("%s DefaultModel %q not found in Models() (checked by TestProviderCatalogConsistency)", meta.ID, def)
		})
	}
}

// TestEmbeddingCapability_ConsistentWithRegistry guards the static capability
// map in registry.go: every ID it lists must be a constructible provider (a
// stale entry would make SupportsEmbeddings lie), the fallback order must be
// exactly the capable set (no capable provider missing from the scan, no
// incapable one included), and the known chat-only providers must report
// false (returning e.g. claude from a fallback scan produces a provider that
// fails at Embed time with an opaque error).
func TestEmbeddingCapability_ConsistentWithRegistry(t *testing.T) {
	capable := EmbeddingFallbackOrder()
	if len(capable) == 0 {
		t.Fatal("embedding fallback order is empty")
	}
	seen := make(map[string]bool, len(capable))
	for _, id := range capable {
		if seen[id] {
			t.Errorf("duplicate %q in embedding fallback order", id)
		}
		seen[id] = true
		if _, ok := providerCtors[id]; !ok {
			t.Errorf("embedding-capable list includes %q which has no constructor", id)
		}
		if !SupportsEmbeddings(id) {
			t.Errorf("fallback order includes %q but SupportsEmbeddings(%q) is false", id, id)
		}
	}
	for _, meta := range AvailableProviders() {
		if SupportsEmbeddings(meta.ID) && !seen[meta.ID] {
			t.Errorf("provider %q is embedding-capable but missing from EmbeddingFallbackOrder", meta.ID)
		}
	}
	for _, chatOnly := range []string{"claude", "xai", "copilot", "demo"} {
		if SupportsEmbeddings(chatOnly) {
			t.Errorf("SupportsEmbeddings(%q) must be false — provider has no Embed implementation", chatOnly)
		}
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
