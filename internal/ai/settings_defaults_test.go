package ai

import (
	"context"
	"testing"

	"pad-core/models"
)

// TestDefaultSettings_ProvidersMatchRegistryAndCatalog pins core's default AI settings (core cannot import ai,
// so the map is a literal copy) to the registry and the catalogue: every key is a registered provider, every
// provider has a row, and a row's DefaultModel is one the provider serves. The gemini default once read
// "gemini-2-5-pro" (dashes) against a catalogue of "gemini-2.5-pro".
func TestDefaultSettings_ProvidersMatchRegistryAndCatalog(t *testing.T) {
	defaults := models.DefaultSettings().AI
	known := map[string]bool{}
	for _, meta := range AvailableProviders() {
		known[meta.ID] = true
		if _, ok := defaults.Providers[meta.ID]; !ok {
			t.Errorf("DefaultSettings().AI.Providers has no row for registered provider %q", meta.ID)
		}
	}
	if !known[defaults.ActiveProvider] {
		t.Errorf("ActiveProvider %q is not a registered provider", defaults.ActiveProvider)
	}
	if !SupportsEmbeddings(defaults.EmbeddingProvider) {
		t.Errorf("EmbeddingProvider %q cannot embed", defaults.EmbeddingProvider)
	}
	for id, row := range defaults.Providers {
		if !known[id] {
			t.Errorf("DefaultSettings().AI.Providers[%q] names an unregistered provider", id)
			continue
		}
		if row.DefaultModel == "" {
			continue // alias and live-listing providers: the server picks
		}
		p := GetMetadataProvider(id)
		if p == nil {
			t.Fatalf("GetMetadataProvider(%q) = nil", id)
		}
		served, err := p.Models(context.Background())
		if err != nil {
			t.Fatalf("%s.Models(): %v", id, err)
		}
		found := false
		for _, m := range served {
			if m.ID == row.DefaultModel {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultSettings().AI.Providers[%q].DefaultModel = %q is not a model %s serves", id, row.DefaultModel, id)
		}
	}
}

// TestProviderNames_MatchRegistry pins each provider's Name() and ID() to the registry entry the listing serves,
// so a client sees one name for a provider wherever it comes from (the local provider once had three).
func TestProviderNames_MatchRegistry(t *testing.T) {
	for _, meta := range AvailableProviders() {
		p := GetMetadataProvider(meta.ID)
		if p == nil {
			t.Fatalf("GetMetadataProvider(%q) = nil", meta.ID)
		}
		if p.ID() != meta.ID {
			t.Errorf("%s: ID() = %q, want the registry id", meta.ID, p.ID())
		}
		if p.Name() != meta.Name {
			t.Errorf("%s: Name() = %q, registry says %q", meta.ID, p.Name(), meta.Name)
		}
	}
}
