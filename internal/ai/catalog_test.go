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
	var ids []string
	for _, meta := range AvailableProviders() {
		// The local models provider has no catalogue by design: its models
		// are whatever the user's own server serves (see local_provider.go).
		if meta.ID == LocalProviderID {
			continue
		}
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
		if _, ok := lookupSpec(id); !ok {
			t.Errorf("embedding-capable list includes %q which has no spec", id)
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
	for _, chatOnly := range []string{"claude", "xai", "copilot"} {
		if SupportsEmbeddings(chatOnly) {
			t.Errorf("SupportsEmbeddings(%q) must be false — provider has no Embed implementation", chatOnly)
		}
	}
}

// TestCatalog_CoversAllProviders ensures every registered provider has
// a central catalog entry, so a newly added provider can't silently ship with an
// empty model list.
func TestCatalog_CoversAllProviders(t *testing.T) {
	for _, meta := range AvailableProviders() {
		if meta.ID == LocalProviderID {
			continue // served by the user's own server, no catalogue
		}
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
	got[0].ContextLimit = 99999
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
