package ai

// ProviderMetadata is one row of AvailableProviders.
type ProviderMetadata struct {
	ID       string
	Name     string
	AuthType string
}

// AvailableProviders lists the registered providers (Specs) in display order.
func AvailableProviders() []ProviderMetadata {
	specs := Specs()
	out := make([]ProviderMetadata, 0, len(specs))
	for _, s := range specs {
		out = append(out, ProviderMetadata{ID: s.ID, Name: s.Name, AuthType: string(s.AuthType)})
	}
	return out
}

// SupportsEmbeddings reports whether the provider can serve Embed (CapEmbeddings) — answerable WITHOUT a key or
// a live call: the narrow MetadataProvider interface omits Embed, and probing a keyed provider is a billed
// request. Unknown ids report false.
func SupportsEmbeddings(providerID string) bool {
	s, ok := lookupSpec(providerID)
	return ok && s.Caps.Has(CapEmbeddings)
}

// EmbeddingFallbackOrder returns the embedding-capable provider ids in display order: the deterministic
// priority callers scan for an alternate embedding provider when the configured one has no key (iterating
// the settings' provider map — Go map order is random — would pick a different fallback per call).
func EmbeddingFallbackOrder() []string {
	var out []string
	for _, s := range Specs() {
		if s.Caps.Has(CapEmbeddings) {
			out = append(out, s.ID)
		}
	}
	return out
}
