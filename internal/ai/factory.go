package ai

import (
	"fmt"
	"sync"
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
	getKey func(scope, provider string) (string, error)
	// getOrgKey resolves an organisation's SHARED credential (G7); nil when
	// the deployment has no org secret store (desktop, local mode). A
	// missing credential is ("", nil), like getKey.
	getOrgKey   func(orgID, provider string) (string, error)
	copilotAuth *CopilotAuth
	recorder    UsageRecorder
	rtCfg       *config.RuntimeConfig
	// embeddingDims is the knowledge base's vector width (PAD_EMBEDDING_DIM).
	// ForEmbedding hands it to providers whose embedding model can shorten
	// its vectors (OpenAI text-embedding-3-*, natively or on Azure) so the
	// chunks they produce are always pgvector-indexable. 0 = don't ask.
	embeddingDims int
}

func NewProviderFactory(getKey func(scope, provider string) (string, error), copilotAuth *CopilotAuth, recorder UsageRecorder, rtCfg *config.RuntimeConfig) *ProviderFactory {
	return &ProviderFactory{getKey: getKey, copilotAuth: copilotAuth, recorder: recorder, rtCfg: rtCfg}
}

// SetOrgKeyLookup enables organisation-shared credentials (G7): ForOrg and
// ForEmbeddingOrg consult getOrgKey when the org has one. Set once at boot,
// before requests are served; nil (the default) makes both behave as For and
// ForEmbedding.
func (f *ProviderFactory) SetOrgKeyLookup(getOrgKey func(orgID, provider string) (string, error)) {
	f.getOrgKey = getOrgKey
}

// OrgScope is the memo/cache scope a provider built from an organisation's
// shared credential runs under: the served-model memo, learned windows and
// the provider service's live listings are then shared by the org's members
// and invalidated together (ProviderService.InvalidateScope), instead of
// being learned once per member for the same resource. It is NOT the usage
// scope — usage rows name the member, and the org through the request's
// OrgID or, for a call that carries none, the chain's usageOrg — and cannot
// collide with a user id.
func OrgScope(orgID string) string { return "org:" + orgID }

// resolveKey applies the member-first rule: the caller's own key when they
// have one, else — with an org and an org store — the organisation's shared
// one, under which the chain runs in OrgScope. A member key-store failure
// (a decrypt failure after a secret rotation, a database error) is reported
// rather than silently falling through to the org's credential: the member
// configured something and must be told it is unreadable.
func (f *ProviderFactory) resolveKey(scope, orgID, providerID string) (key, memoScope string, err error) {
	key, err = f.getKey(scope, storageKey(providerID))
	if err != nil {
		return "", "", fmt.Errorf("get %s key: %w", providerID, err)
	}
	if key != "" {
		return key, scope, nil
	}
	if orgID != "" && f.getOrgKey != nil {
		key, err = f.getOrgKey(orgID, storageKey(providerID))
		if err != nil {
			return "", "", fmt.Errorf("get %s org key: %w", providerID, err)
		}
		if key != "" {
			return key, OrgScope(orgID), nil
		}
	}
	return "", "", fmt.Errorf("%s: %w", providerID, ErrKeyNotConfigured)
}

// resolveOrgFirstKey is resolveKey with the organisation's shared credential
// preferred (ForEmbeddingOrg): one corpus, one resource.
func (f *ProviderFactory) resolveOrgFirstKey(scope, orgID, providerID string) (key, memoScope string, err error) {
	if orgID != "" && f.getOrgKey != nil {
		key, err = f.getOrgKey(orgID, storageKey(providerID))
		if err != nil {
			return "", "", fmt.Errorf("get %s org key: %w", providerID, err)
		}
		if key != "" {
			return key, OrgScope(orgID), nil
		}
	}
	key, err = f.getKey(scope, storageKey(providerID))
	if err != nil {
		return "", "", fmt.Errorf("get %s key: %w", providerID, err)
	}
	if key != "" {
		return key, scope, nil
	}
	return "", "", fmt.Errorf("%s: %w", providerID, ErrKeyNotConfigured)
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
	// The "key" is the JSON configuration document (endpoint + credential).
	"azure-foundry": func(k string) Provider { return NewAzureFoundryProvider(k) },
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
	return f.wrapChainScoped(p, scope, scope, "", providerID)
}

// credentialOrg is the organisation whose shared credential a chain resolved
// to memoScope runs on: orgID when memoScope is OrgScope(orgID), else "" (the
// caller's own credential).
func credentialOrg(orgID, memoScope string) string {
	if orgID != "" && memoScope == OrgScope(orgID) {
		return orgID
	}
	return ""
}

// wrapChainScoped is wrapChain with the memo scope named separately: scope
// attributes usage (the member), memoScope keys the served-model memo and
// learned windows (OrgScope for a provider built from an org's shared
// credential, so the org's members share what one of them learned).
// usageOrg is that organisation ("" for the caller's own credential): every
// usage row the chain records without an org of its own (embeddings, the
// admin's connection test) is attributed to it, so spend on an org's
// credential always lands on the org's daily budget.
func (f *ProviderFactory) wrapChainScoped(p Provider, scope, memoScope, usageOrg, providerID string) Provider {
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

	ap := newAuditedProvider(NewTracedProvider(rp), f.recorder, scope, providerID)
	ap.memoScope = memoScope
	ap.usageOrg = usageOrg
	// The wrappers above hide the raw provider's optional methods; the
	// audited wrapper reads what it needs from p before they do.
	ap.adoptRawHints(p)
	return ap
}

// ForOrg is For with the organisation's shared credential as the fallback
// (G7, member-first): the caller's own key when they have one, else the
// org's, under which the chain's memos run in OrgScope(orgID). An empty
// orgID, or a factory without an org store, behaves exactly as For. Copilot
// is always the member's (OAuth or PAT); an org never has a Copilot key.
func (f *ProviderFactory) ForOrg(scope, orgID, providerID string) (Provider, error) {
	if providerID == "copilot" {
		return f.For(scope, providerID)
	}
	ctor, ok := lookupProviderCtor(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerID)
	}
	key, memoScope, err := f.resolveKey(scope, orgID, providerID)
	if err != nil {
		return nil, err
	}
	return f.wrapChainScoped(ctor(key), scope, memoScope, credentialOrg(orgID, memoScope), providerID), nil
}

// UsedOrgCredential reports whether p — a chain the factory built — runs on
// an organisation's shared credential rather than the caller's own, so the
// chat service can tell the member that a rejected credential is the org
// admin's to fix. false for anything else (a bare provider, a test double).
func UsedOrgCredential(p Provider) bool {
	ap, ok := p.(*auditedProvider)
	return ok && ap.usageOrg != ""
}

// ForOrgCredential builds a provider from the organisation's shared
// credential ONLY — never the caller's own — for an admin's connection test
// of what the members will run on. scope names the caller on the test call's
// usage row, which is attributed to the org (its credential, its budget);
// memos run in OrgScope. Copilot is never shared.
func (f *ProviderFactory) ForOrgCredential(scope, orgID, providerID string) (Provider, error) {
	if providerID == "copilot" {
		return nil, fmt.Errorf("copilot: %w", ErrKeyNotConfigured)
	}
	ctor, ok := lookupProviderCtor(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerID)
	}
	if orgID == "" || f.getOrgKey == nil {
		return nil, fmt.Errorf("%s: %w", providerID, ErrKeyNotConfigured)
	}
	key, err := f.getOrgKey(orgID, storageKey(providerID))
	if err != nil {
		return nil, fmt.Errorf("get %s org key: %w", providerID, err)
	}
	if key == "" {
		return nil, fmt.Errorf("%s: %w", providerID, ErrKeyNotConfigured)
	}
	return f.wrapChainScoped(ctor(key), scope, OrgScope(orgID), orgID, providerID), nil
}

// ForEmbeddingOrg is ForEmbedding with the organisation's shared credential
// PREFERRED over the member's: the org's knowledge base is one corpus and
// must be embedded with one resource, whichever member uploads or searches.
// Without an org credential the member's own is used, as ForEmbedding does.
func (f *ProviderFactory) ForEmbeddingOrg(scope, orgID, providerID, embeddingModel string) (Provider, error) {
	if providerID == "copilot" {
		return f.For(scope, providerID)
	}
	ctor, ok := lookupProviderCtor(providerID)
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerID)
	}
	key, memoScope, err := f.resolveOrgFirstKey(scope, orgID, providerID)
	if err != nil {
		return nil, err
	}
	p, err := f.prepareEmbedder(ctor(key), providerID, embeddingModel)
	if err != nil {
		return nil, err
	}
	return f.wrapChainScoped(p, scope, memoScope, credentialOrg(orgID, memoScope), providerID), nil
}

func (f *ProviderFactory) For(scope, providerID string) (Provider, error) {
	if providerID == "copilot" {
		p, err := f.forCopilot(scope)
		if err != nil {
			return nil, err
		}
		return f.wrapChain(p, scope, providerID), nil
	}
	ctor, ok := lookupProviderCtor(providerID)
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

// SetEmbeddingDimensions records the vector width ForEmbedding requests from
// providers that support Matryoshka-style truncation (embedDimensionSetter).
func (f *ProviderFactory) SetEmbeddingDimensions(n int) {
	if n > 0 {
		f.embeddingDims = n
	}
}

// EmbeddingDimensions reports the vector width ForEmbedding requests (0 =
// none configured).
func (f *ProviderFactory) EmbeddingDimensions() int { return f.embeddingDims }

// embeddingReadiness is implemented by providers that can hold a usable key
// (credential document) and still be unable to embed — an Azure AI Foundry
// configuration without an embedding deployment. ForEmbedding reports that
// as an error so callers scanning for an embedder move on to the next
// provider instead of failing later at Embed time.
type embeddingReadiness interface {
	embeddingUnavailable() error
}

// embedDimensionSetter is implemented by providers that can ask their
// embedding endpoint for a specific vector width (the OpenAI-compatible
// family sends `dimensions` for text-embedding-3-* models).
type embedDimensionSetter interface {
	setEmbeddingDimensions(n int)
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
// constructed provider. Providers that don't support an override (copilot)
// ignore the model and behave as For. The override is applied BEFORE wrapping
// (circuit breaker / retry / tracing / audit) so the inner provider carries it.
func (f *ProviderFactory) ForEmbedding(scope, providerID, embeddingModel string) (Provider, error) {
	if providerID == "copilot" {
		// Copilot has no embeddings; the override is meaningless. Use the normal
		// path so dual-auth still resolves correctly.
		return f.For(scope, providerID)
	}
	ctor, ok := lookupProviderCtor(providerID)
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
	p, err := f.prepareEmbedder(ctor(key), providerID, embeddingModel)
	if err != nil {
		return nil, err
	}
	return f.wrapChain(p, scope, providerID), nil
}

// prepareEmbedder applies the embedding-model and vector-width overrides to
// a raw provider and checks it can embed at all — before wrapping, since the
// wrappers hide the concrete type.
func (f *ProviderFactory) prepareEmbedder(p Provider, providerID, embeddingModel string) (Provider, error) {
	if embeddingModel != "" {
		if setter, ok := p.(embedModelSetter); ok {
			setter.setEmbeddingModel(embeddingModel)
		}
	}
	if f.embeddingDims > 0 {
		if setter, ok := p.(embedDimensionSetter); ok {
			setter.setEmbeddingDimensions(f.embeddingDims)
		}
	}
	// After the setters (an embedding-model override can make a provider
	// ready) and before wrapChain (the wrappers hide the concrete type).
	if r, ok := p.(embeddingReadiness); ok {
		if err := r.embeddingUnavailable(); err != nil {
			return nil, fmt.Errorf("%s: %w", providerID, err)
		}
	}
	return p, nil
}

// GetMetadataProvider returns a MetadataProvider suitable for reading provider
// information (models, pricing, context limits) without requiring a valid API key.
// The returned value intentionally omits Chat and Stream — use ProviderFactory.For
// to obtain a fully functional Provider.
func GetMetadataProvider(providerID string) MetadataProvider {
	if providerID == "copilot" {
		return NewCopilotProvider("")
	}
	if ctor, ok := lookupProviderCtor(providerID); ok {
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

// providerMu guards providerCtors against RegisterProvider racing a lookup.
var providerMu sync.RWMutex

func lookupProviderCtor(id string) (func(key string) Provider, bool) {
	providerMu.RLock()
	defer providerMu.RUnlock()
	ctor, ok := providerCtors[id]
	return ctor, ok
}

// RegisterProvider adds (or replaces) a provider constructor at runtime so a
// scripted provider is reachable through the normal factory path. It exists
// for tests — the removed "demo" provider used to play that role, driving a
// real stream end to end without network — and is not a plugin API: the
// catalog, pricing and capability tables are not consulted for it.
func RegisterProvider(id string, ctor func(key string) Provider) {
	providerMu.Lock()
	defer providerMu.Unlock()
	providerCtors[id] = ctor
}
