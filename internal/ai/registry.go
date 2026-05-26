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
