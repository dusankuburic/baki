package ai

import "fmt"

// ProviderFactory creates Provider instances using a caller-supplied key-fetching function.
// This keeps the ai package free of storage dependencies.
//
// getKey takes a scope (owner namespace; "" = legacy/local) and a provider key
// name. Manual per-user API keys are resolved under the caller's scope; OAuth
// device-flow tokens (github-models, copilot OAuth) are resolved under the
// global scope ("") — they are not yet per-user.
type ProviderFactory struct {
	getKey      func(scope, provider string) (string, error)
	copilotAuth *CopilotAuth
}

// NewProviderFactory returns a factory that resolves API keys via getKey.
// Typical usage: ai.NewProviderFactory(storage.GetApiKeyScoped, copilotAuth)
func NewProviderFactory(getKey func(scope, provider string) (string, error), copilotAuth *CopilotAuth) *ProviderFactory {
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

// For returns an initialised Provider for the given providerID, resolving its
// key within scope (the caller's owner namespace; "" = legacy/local).
func (f *ProviderFactory) For(scope, providerID string) (Provider, error) {
	if providerID == "demo" {
		return NewTracedProvider(NewRetryingProvider(NewCircuitBreakerProvider(NewDemoProvider()))), nil
	}
	if providerID == "copilot" {
		p, err := f.forCopilot(scope)
		if err != nil {
			return nil, err
		}
		return NewTracedProvider(NewRetryingProvider(NewCircuitBreakerProvider(p))), nil
	}
	ctor, ok := providerCtors[providerID]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerID)
	}
	// github-models is authenticated via the GitHub device flow, whose token is
	// not yet per-user; resolve it in the global scope. All other providers use
	// the caller's manual, per-user key.
	keyScope := scope
	if providerID == "github-models" {
		keyScope = ""
	}
	key, err := f.getKey(keyScope, storageKey(providerID))
	if err != nil {
		return nil, fmt.Errorf("get %s key: %w", providerID, err)
	}
	if key == "" {
		return nil, fmt.Errorf("%s: %w", providerID, ErrKeyNotConfigured)
	}
	return NewTracedProvider(NewRetryingProvider(NewCircuitBreakerProvider(ctor(key)))), nil
}

// GetMetadataProvider returns a MetadataProvider suitable for reading provider
// information (models, pricing, context limits) without requiring a valid API key.
// The returned value intentionally omits Chat and Stream — use ProviderFactory.For
// to obtain a fully functional Provider.
func GetMetadataProvider(providerID string) MetadataProvider {
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
// The OAuth token comes from the device flow and is not yet per-user (global
// scope ""); the manual PAT is a per-user key resolved under the caller's scope.
func (f *ProviderFactory) forCopilot(scope string) (Provider, error) {
	// Try OAuth token first (stored by the device flow; global scope).
	oauthToken, oauthErr := f.getKey("", "copilot-oauth-token")
	if oauthErr == nil && oauthToken != "" && f.copilotAuth != nil {
		return NewCopilotProviderWithAuth(f.copilotAuth, oauthToken), nil
	}

	// Fall back to manual PAT (per-user).
	pat, patErr := f.getKey(scope, "copilot")
	if patErr == nil && pat != "" {
		return NewCopilotProvider(pat), nil
	}

	return nil, fmt.Errorf("copilot not configured: no OAuth token or PAT found")
}
