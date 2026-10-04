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
		// Configured with an endpoint + credential document rather than a bare key (AzureFoundryConfig); AuthType
		// stays "api_key" because the UI's auth-type union has no third value and the document is user-entered.
		{ID: "azure-foundry", Name: "Azure AI Foundry", AuthType: "api_key"},
		// A locally hosted OpenAI-compatible server (Foundry Local, Ollama, LM Studio): loopback base URL
		// (LocalConfig), desktop only, listed only once configured; "api_key" for the same reason as azure-foundry.
		{ID: LocalProviderID, Name: "Local model (OpenAI-compatible)", AuthType: "api_key"},
	}
}

// embeddingCapable records which providers can serve Embed, so capability is answerable WITHOUT a key or a live
// call — the narrow MetadataProvider interface omits Embed, and probing a keyed provider is a billed request. Sync
// it when adding/removing embedding support (TestEmbeddingCapability_ConsistentWithRegistry guards the IDs).
var embeddingCapable = map[string]bool{
	"openai":        true,
	"gemini":        true,
	"glm":           true,
	"github-models": true,
	"azure-foundry": true,
}

// embeddingFallbackOrder is the deterministic priority callers scan for an
// alternate embedding provider when the configured one has no key; iterating
// the settings' provider map (Go map order is random) would pick a different
// fallback per call.
var embeddingFallbackOrder = []string{"openai", "gemini", "glm", "github-models", "azure-foundry"}

// SupportsEmbeddings reports whether the provider ID can serve embedding
// requests. Unknown IDs report false.
func SupportsEmbeddings(providerID string) bool {
	return embeddingCapable[providerID]
}

// EmbeddingFallbackOrder returns the embedding-capable provider IDs in deterministic fallback priority
// (a fresh copy — callers can't mutate the package-level slice).
func EmbeddingFallbackOrder() []string {
	out := make([]string, len(embeddingFallbackOrder))
	copy(out, embeddingFallbackOrder)
	return out
}
