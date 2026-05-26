package ai

import "fmt"

// ProviderFactory creates Provider instances using a caller-supplied key-fetching function.
// This keeps the ai package free of storage dependencies.
type ProviderFactory struct {
	getKey      func(string) (string, error)
	copilotAuth *CopilotAuth
}

// NewProviderFactory returns a factory that resolves API keys via getKey.
// Typical usage: ai.NewProviderFactory(storage.GetApiKey, copilotAuth)
func NewProviderFactory(getKey func(string) (string, error), copilotAuth *CopilotAuth) *ProviderFactory {
	return &ProviderFactory{getKey: getKey, copilotAuth: copilotAuth}
}

// providerCtors maps provider IDs to their constructors.
// Adding a new provider requires only a single entry here.
// Note: "copilot" is handled specially in For() for dual-auth support.
var providerCtors = map[string]func(key string) Provider{
	"claude":        func(k string) Provider { return NewClaudeProvider(k) },
	"openai":        func(k string) Provider { return NewOpenAIProvider(k) },
	"gemini":        func(k string) Provider { return NewGeminiProvider(k) },
	"xai":           func(k string) Provider { return NewXAIProvider(k) },
	"glm":           func(k string) Provider { return NewGLMProvider(k) },
	"github-models": func(k string) Provider { return NewGitHubModelsProvider(k) },
}

// storageKey returns the keyring key name for a provider ID.
// Most providers use their own ID; github-models uses a different token name.
func storageKey(providerID string) string {
	if providerID == "github-models" {
		return "github-models-token"
	}
	return providerID
}

// For returns an initialised Provider for the given providerID.
func (f *ProviderFactory) For(providerID string) (Provider, error) {
	if providerID == "demo" {
		return NewDemoProvider(), nil
	}
	if providerID == "copilot" {
		return f.forCopilot()
	}
	ctor, ok := providerCtors[providerID]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerID)
	}
	key, err := f.getKey(storageKey(providerID))
	if err != nil {
		return nil, fmt.Errorf("get %s key: %w", providerID, err)
	}
	return ctor(key), nil
}

// GetMetadataProvider returns a provider instance with an empty key,
// suitable for retrieving metadata (like Models() or Name()) without storage access.
func GetMetadataProvider(providerID string) Provider {
	if providerID == "demo" {
		return NewDemoProvider()
	}
	if providerID == "copilot" {
		return NewCopilotProvider("")
	}
	if ctor, ok := providerCtors[providerID]; ok {
		return ctor("")
	}
	return nil
}

// forCopilot implements dual-auth: try OAuth token first, fall back to manual PAT.
func (f *ProviderFactory) forCopilot() (Provider, error) {
	// Try OAuth token first (stored by the device flow)
	oauthToken, oauthErr := f.getKey("copilot-oauth-token")
	if oauthErr == nil && oauthToken != "" && f.copilotAuth != nil {
		return NewCopilotProviderWithAuth(f.copilotAuth, oauthToken), nil
	}

	// Fall back to manual PAT
	pat, patErr := f.getKey("copilot")
	if patErr == nil && pat != "" {
		return NewCopilotProvider(pat), nil
	}

	return nil, fmt.Errorf("copilot not configured: no OAuth token or PAT found")
}
