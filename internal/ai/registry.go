package ai

type ProviderMetadata struct {
	ID       string
	Name     string
	AuthType string
}

func AvailableProviders() []ProviderMetadata {
	return []ProviderMetadata{
		{ID: "claude", Name: "Claude", AuthType: "api_key"},
		{ID: "openai", Name: "OpenAI", AuthType: "api_key"},
		{ID: "gemini", Name: "Gemini", AuthType: "api_key"},
		{ID: "xai", Name: "xAI (Grok)", AuthType: "api_key"},
		{ID: "glm", Name: "GLM (z.ai)", AuthType: "api_key"},
		{ID: "github-models", Name: "GitHub Models", AuthType: "oauth"},
		{ID: "copilot", Name: "GitHub Copilot", AuthType: "oauth"},
	}
}

// embeddingCapable records which providers can serve Embed requests. Kept
// beside the provider registry (mirroring the model catalog) so capability is
// answerable WITHOUT a key or a live call — the narrow MetadataProvider
// interface intentionally omits Embed, and probing a keyed provider would mean
// a billed request. Sync it when adding/removing embedding support on a
// provider (TestEmbeddingCapability_ConsistentWithRegistry guards the IDs).
var embeddingCapable = map[string]bool{
	"openai":        true,
	"gemini":        true,
	"glm":           true,
	"github-models": true,
}

// embeddingFallbackOrder is the deterministic priority in which callers scan
// for an alternate embedding provider when the configured one has no key.
// Determinism matters: iterating the settings' provider map (Go map order is
// random) would pick a different fallback per call.
var embeddingFallbackOrder = []string{"openai", "gemini", "glm", "github-models"}

// SupportsEmbeddings reports whether the provider ID can serve embedding
// requests. Unknown IDs report false.
func SupportsEmbeddings(providerID string) bool {
	return embeddingCapable[providerID]
}

// EmbeddingFallbackOrder returns the embedding-capable provider IDs in
// deterministic fallback priority order (a fresh copy — callers can't mutate
// the package-level slice).
func EmbeddingFallbackOrder() []string {
	out := make([]string, len(embeddingFallbackOrder))
	copy(out, embeddingFallbackOrder)
	return out
}
