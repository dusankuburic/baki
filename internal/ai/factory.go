package ai

import (
	"fmt"
	"time"

	"pad-analyzer/internal/config"
)

// ProviderFactory creates Provider instances using a caller-supplied key-fetching function.
// This keeps the ai package free of storage dependencies.
//
// getKey takes a scope (owner namespace; "" = legacy/local) and a provider key
// name. All keys — manual API keys and OAuth device-flow tokens (github-models,
// copilot OAuth) alike — are resolved under the caller's scope, so each user's
// connected providers are isolated.
type ProviderFactory struct {
	getKey      func(scope, provider string) (string, error)
	copilotAuth *CopilotAuth
	recorder    UsageRecorder
	rtCfg       *config.RuntimeConfig
}

func NewProviderFactory(getKey func(scope, provider string) (string, error), copilotAuth *CopilotAuth, recorder UsageRecorder, rtCfg *config.RuntimeConfig) *ProviderFactory {
	return &ProviderFactory{getKey: getKey, copilotAuth: copilotAuth, recorder: recorder, rtCfg: rtCfg}
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

func (f *ProviderFactory) wrapChain(p Provider, scope, providerID string) Provider {
	var cb Provider
	var rp Provider

	if f.rtCfg != nil {
		threshold := f.rtCfg.CircuitBreakerFailures
		if threshold <= 0 {
			threshold = cbFailureThreshold
		}
		openDur := cbOpenDuration
		if d, err := time.ParseDuration(f.rtCfg.CircuitBreakerOpenDur); err == nil && d > 0 {
			openDur = d
		}
		cb = NewCircuitBreakerProviderWithConfig(p, threshold, openDur)

		maxAttempts := f.rtCfg.RetryMaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = retryMaxAttempts
		}
		baseDelay := retryBaseDelay
		if d, err := time.ParseDuration(f.rtCfg.RetryBaseDelay); err == nil && d > 0 {
			baseDelay = d
		}
		rp = NewRetryingProviderWithConfig(cb, maxAttempts, baseDelay)
	} else {
		cb = NewCircuitBreakerProvider(p)
		rp = NewRetryingProvider(cb)
	}

	return NewAuditedProvider(NewTracedProvider(rp), f.recorder, scope, providerID)
}

func (f *ProviderFactory) For(scope, providerID string) (Provider, error) {
	if providerID == "demo" {
		return f.wrapChain(NewDemoProvider(), scope, providerID), nil
	}
	if providerID == "copilot" {
		p, err := f.forCopilot(scope)
		if err != nil {
			return nil, err
		}
		return f.wrapChain(p, scope, providerID), nil
	}
	ctor, ok := providerCtors[providerID]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerID)
	}
	key, err := f.getKey(scope, storageKey(providerID))
	if err != nil {
		return nil, fmt.Errorf("get %s key: %w", providerID, err)
	}
	if key == "" {
		return nil, fmt.Errorf("%s: %w", providerID, ErrKeyNotConfigured)
	}
	return f.wrapChain(ctor(key), scope, providerID), nil
}

// embedModelSetter is implemented by providers whose embedding model name can be
// overridden at construction (the OpenAI-compatible family via openaiBase, plus
// Gemini). The factory's ForEmbedding applies a deployer-chosen model so RAG can
// use a different embedding model than the provider's hardcoded default without
// a code change.
type embedModelSetter interface {
	setEmbeddingModel(model string)
}

// ForEmbedding is like For but applies an embedding-model override to the
// constructed provider. Providers that don't support an override (copilot, demo)
// ignore the model and behave as For. The override is applied BEFORE wrapping
// (circuit breaker / retry / tracing / audit) so the inner provider carries it.
func (f *ProviderFactory) ForEmbedding(scope, providerID, embeddingModel string) (Provider, error) {
	if providerID == "demo" {
		return f.For(scope, providerID)
	}
	if providerID == "copilot" {
		// Copilot has no embeddings; the override is meaningless. Use the normal
		// path so dual-auth still resolves correctly.
		return f.For(scope, providerID)
	}
	ctor, ok := providerCtors[providerID]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerID)
	}
	key, err := f.getKey(scope, storageKey(providerID))
	if err != nil {
		return nil, fmt.Errorf("get %s key: %w", providerID, err)
	}
	if key == "" {
		return nil, fmt.Errorf("%s: %w", providerID, ErrKeyNotConfigured)
	}
	p := ctor(key)
	if embeddingModel != "" {
		if setter, ok := p.(embedModelSetter); ok {
			setter.setEmbeddingModel(embeddingModel)
		}
	}
	return f.wrapChain(p, scope, providerID), nil
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
// Both the OAuth token (from the device flow) and the manual PAT are per-user
// keys resolved under the caller's scope.
func (f *ProviderFactory) forCopilot(scope string) (Provider, error) {
	// Try OAuth token first (per-user, stored by the device flow).
	oauthToken, oauthErr := f.getKey(scope, "copilot-oauth-token")
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
