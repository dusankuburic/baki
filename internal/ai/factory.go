package ai

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/config"
)

// ProviderFactory creates Provider instances using a caller-supplied key-fetching function, keeping the ai
// package free of storage dependencies. All keys — manual API keys and OAuth tokens alike — resolve under the
// caller's scope, so each user's connected providers are isolated.
type ProviderFactory struct {
	getKey func(scope, provider string) (string, error)
	// getOrgKey resolves an organisation's SHARED credential (G7); nil when the deployment has no org secret
	// store (desktop, local mode). A missing credential is ("", nil), like getKey.
	getOrgKey   func(orgID, provider string) (string, error)
	copilotAuth *CopilotAuth
	recorder    UsageRecorder
	rtCfg       *config.RuntimeConfig
	// embeddingDims is the knowledge base's vector width (PAD_EMBEDDING_DIM), handed by ForEmbedding to
	// providers whose embedding model can shorten its vectors (text-embedding-3-*). 0 = don't ask.
	embeddingDims int
}

func NewProviderFactory(getKey func(scope, provider string) (string, error), copilotAuth *CopilotAuth, recorder UsageRecorder, rtCfg *config.RuntimeConfig) *ProviderFactory {
	return &ProviderFactory{getKey: getKey, copilotAuth: copilotAuth, recorder: recorder, rtCfg: rtCfg}
}

// SetOrgKeyLookup enables organisation-shared credentials (G7); set once at boot. nil (the default) makes
// ForOrg and ForEmbeddingOrg behave as For and ForEmbedding.
func (f *ProviderFactory) SetOrgKeyLookup(getOrgKey func(orgID, provider string) (string, error)) {
	f.getOrgKey = getOrgKey
}

// OrgScope is the memo/cache scope a provider built from an organisation's shared credential runs under: the
// served-model memo, learned windows and live listings are shared and invalidated together, not learned once
// per member. NOT the usage scope — rows name the member and org via OrgID/usageOrg.
func OrgScope(orgID string) string { return "org:" + orgID }

// resolveKey applies the member-first rule: the caller's own key when they have one, else the org's shared
// one (chain in OrgScope). A member key-store failure is reported, never silently fallen through to the org's.
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

// providerCtors maps provider IDs to their constructors; "copilot" is handled specially in For() for dual-auth.
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

// credentialOrg is the organisation whose shared credential a chain resolved to memoScope runs on: orgID when
// memoScope is OrgScope(orgID), else "".
func credentialOrg(orgID, memoScope string) string {
	if orgID != "" && memoScope == OrgScope(orgID) {
		return orgID
	}
	return ""
}

// wrapChainScoped is wrapChain with the memo scope named separately: scope attributes usage (the member),
// memoScope keys the served-model memo and learned windows (OrgScope for an org's shared credential, so the
// org's members share what one learned), and usageOrg is that organisation — rows the chain records without
// an org of their own are attributed to it, so spend on an org credential lands on the org's budget.
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

// ForOrg is For with the organisation's shared credential as the member-first fallback: the caller's own key
// when they have one, else the org's (chain memos in OrgScope). Copilot is always the member's; an org never
// has one. An empty orgID, or no org store, behaves exactly as For.
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

// AzureConnectionRef names one Azure AI Foundry resource's credential in the model registry: the caller's own
// (OrgID "") or an organisation's shared one; ConnID is the resource id (AzureDefaultConnectionID = legacy).
type AzureConnectionRef struct {
	OrgID  string
	ConnID string
}

// ForAzureConnection builds the provider for one registry resource (NewAzureFoundryConnectionProvider) from
// exactly the credential ref names — never another, no member-first fallback: the registry resolved which
// resource the request runs on, and a missing credential is ErrKeyNotConfigured. An org ref runs its memos
// under OrgScope with the org as usageOrg. models is the resource's FULL entry set — the picker's too, so
// both share one listing cache key.
func (f *ProviderFactory) ForAzureConnection(scope string, ref AzureConnectionRef, models []AzureRegisteredModel) (Provider, error) {
	key, memoScope, usageOrg, err := f.resolveAzureConnectionKey(scope, ref)
	if err != nil {
		return nil, err
	}
	p := NewAzureFoundryConnectionProvider(key, models, "")
	return f.wrapChainScoped(p, scope, memoScope, usageOrg, azureFoundryProviderID), nil
}

// ForAzureConnectionEmbedding is ForAzureConnection for the knowledge base's embedder: entry must be the
// designated embedding model (kind embedding) or the call fails with ErrEmbeddingDeploymentMissing —
// ErrOrgEmbeddingDeploymentMissing on an org ref, which names the org admin.
func (f *ProviderFactory) ForAzureConnectionEmbedding(scope string, ref AzureConnectionRef, entry AzureRegisteredModel) (Provider, error) {
	if entry.Kind != AzureModelKindEmbedding || !azureDeploymentName.MatchString(strings.TrimSpace(entry.Deployment)) {
		if ref.OrgID != "" {
			return nil, fmt.Errorf("%s: %w", azureFoundryProviderID, ErrOrgEmbeddingDeploymentMissing)
		}
		return nil, fmt.Errorf("%s: %w", azureFoundryProviderID, ErrEmbeddingDeploymentMissing)
	}
	key, memoScope, usageOrg, err := f.resolveAzureConnectionKey(scope, ref)
	if err != nil {
		return nil, err
	}
	raw := newAzureFoundryConnectionProvider(key, []AzureRegisteredModel{entry}, entry.Deployment, azureHTTPClient(), processEntraSources)
	p, err := f.prepareEmbedder(raw, azureFoundryProviderID, "")
	if err != nil {
		return nil, err
	}
	return f.wrapChainScoped(p, scope, memoScope, usageOrg, azureFoundryProviderID), nil
}

// resolveAzureConnectionKey reads the credential document ref names: the caller's own key (memo scope =
// scope) or the org's shared one (OrgScope, usage to the org). A key-store failure is reported, never
// answered with another credential; an absent document is ErrKeyNotConfigured.
func (f *ProviderFactory) resolveAzureConnectionKey(scope string, ref AzureConnectionRef) (key, memoScope, usageOrg string, err error) {
	if !ValidAzureConnectionID(ref.ConnID) {
		return "", "", "", fmt.Errorf("%s: invalid resource id %q", azureFoundryProviderID, ref.ConnID)
	}
	name := AzureConnectionKey(ref.ConnID)
	if ref.OrgID == "" {
		key, err = f.getKey(scope, name)
		if err != nil {
			return "", "", "", fmt.Errorf("get %s key: %w", name, err)
		}
		if key == "" {
			return "", "", "", fmt.Errorf("%s: %w", name, ErrKeyNotConfigured)
		}
		return key, scope, "", nil
	}
	if f.getOrgKey == nil {
		return "", "", "", fmt.Errorf("%s: %w", name, ErrKeyNotConfigured)
	}
	key, err = f.getOrgKey(ref.OrgID, name)
	if err != nil {
		return "", "", "", fmt.Errorf("get %s org key: %w", name, err)
	}
	if key == "" {
		return "", "", "", fmt.Errorf("%s: %w", name, ErrKeyNotConfigured)
	}
	return key, OrgScope(ref.OrgID), ref.OrgID, nil
}

// UsedOrgCredential reports whether p — a chain the factory built — runs on an organisation's shared
// credential rather than the caller's own (so the chat service can point the member at the org admin).
func UsedOrgCredential(p Provider) bool {
	ap, ok := p.(*auditedProvider)
	return ok && ap.usageOrg != ""
}

// ForOrgCredential builds a provider from the organisation's shared credential ONLY — for an admin's
// connection test of what members run on. The test call's usage row names scope but bills the org; memos run
// in OrgScope. Copilot is never shared.
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

// ForEmbeddingOrg is ForEmbedding with the organisation's shared credential PREFERRED: the org's knowledge
// base is one corpus and must be embedded with one resource, whichever member uploads or searches. When the
// ORG's credential is resolved and cannot embed (a Foundry document without an embedding deployment) the
// error is ErrOrgEmbeddingDeploymentMissing, on which the rag service refuses the personal-key fallback —
// a member's key must not seed the org corpus; a member's OWN document keeps the plain sentinel.
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
		if memoScope == OrgScope(orgID) && errors.Is(err, ErrEmbeddingDeploymentMissing) {
			return nil, fmt.Errorf("%s: %w", providerID, ErrOrgEmbeddingDeploymentMissing)
		}
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

// embeddingReadiness is implemented by providers that can hold a usable credential and still be unable to
// embed (a Foundry document without an embedding deployment); ForEmbedding reports that so callers scanning
// for an embedder move on instead of failing at Embed time.
type embeddingReadiness interface {
	embeddingUnavailable() error
}

// embedDimensionSetter is implemented by providers that can ask their
// embedding endpoint for a specific vector width (the OpenAI-compatible
// family sends `dimensions` for text-embedding-3-* models).
type embedDimensionSetter interface {
	setEmbeddingDimensions(n int)
}

// embedModelSetter is implemented by providers whose embedding model name can be overridden at construction
// (the OpenAI-compatible family via openaiBase, plus Gemini), so RAG can use a different embedding model.
type embedModelSetter interface {
	setEmbeddingModel(model string)
}

// ForEmbedding is like For but applies an embedding-model override BEFORE wrapping, so the inner provider
// carries it; providers without an override (copilot) ignore the model and behave as For.
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

// prepareEmbedder applies the embedding-model and vector-width overrides and checks readiness — before
// wrapping, since the wrappers hide the concrete type.
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

// GetMetadataProvider returns a MetadataProvider for reading provider information (models, pricing, context
// limits) without an API key; it intentionally omits Chat and Stream — use For for a functional Provider.
func GetMetadataProvider(providerID string) MetadataProvider {
	if providerID == "copilot" {
		return NewCopilotProvider("")
	}
	if ctor, ok := lookupProviderCtor(providerID); ok {
		return ctor("")
	}
	return nil
}

// forCopilot implements dual-auth: the per-user OAuth token (device flow) first, else the manual PAT.
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

// RegisterProvider adds (or replaces) a provider constructor at runtime; it exists for tests, not as a plugin API.
func RegisterProvider(id string, ctor func(key string) Provider) {
	providerMu.Lock()
	defer providerMu.Unlock()
	providerCtors[id] = ctor
}
