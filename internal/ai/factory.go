package ai

import "fmt"

// ProviderFactory creates Provider instances using a caller-supplied key-fetching function.
// This keeps the ai package free of storage dependencies.
type ProviderFactory struct {
	getKey func(string) (string, error)
}

// NewProviderFactory returns a factory that resolves API keys via getKey.
// Typical usage: ai.NewProviderFactory(storage.GetApiKey)
func NewProviderFactory(getKey func(string) (string, error)) *ProviderFactory {
	return &ProviderFactory{getKey: getKey}
}

// providerCtors maps provider IDs to their constructors.
// Adding a new provider requires only a single entry here.
var providerCtors = map[string]func(key string) Provider{
	"claude":        func(k string) Provider { return NewClaudeProvider(k) },
	"openai":        func(k string) Provider { return NewOpenAIProvider(k) },
	"gemini":        func(k string) Provider { return NewGeminiProvider(k) },
	"xai":           func(k string) Provider { return NewXAIProvider(k) },
	"glm":           func(k string) Provider { return NewGLMProvider(k) },
	"github-models": func(k string) Provider { return NewGitHubModelsProvider(k) },
	"copilot":       func(k string) Provider { return NewCopilotProvider(k) },
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
