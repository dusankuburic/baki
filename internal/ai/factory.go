package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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
	resilience  ResilienceConfig
	// embeddingDims is the knowledge base's vector width (PAD_EMBEDDING_DIM), handed by ForEmbedding to
	// providers whose embedding model can shorten its vectors (text-embedding-3-*). 0 = don't ask.
	embeddingDims int
	// budget is the pre-call gate every chain this factory builds consults (WithBudgetChecker, ADR 0028); nil
	// gates nothing.
	budget BudgetChecker
	// policy decides which providers may be built at all (WithProviderPolicy), asked before any key is read;
	// nil allows every registered provider.
	policy ProviderPolicy
}

// FactoryOption configures NewProviderFactory.
type FactoryOption func(*ProviderFactory)

// WithBudgetChecker makes every chain the factory builds consult b before each billed call.
func WithBudgetChecker(b BudgetChecker) FactoryOption {
	return func(f *ProviderFactory) { f.budget = b }
}

// WithProviderPolicy makes every builder ask p whether the provider may be used, before it reads a credential.
func WithProviderPolicy(p ProviderPolicy) FactoryOption {
	return func(f *ProviderFactory) { f.policy = p }
}

// ResilienceConfig is the circuit breaker and retry policy every provider chain is wrapped in, built once from the
// configuration (PAD_CB_FAILURES, PAD_CB_OPEN_DURATION, PAD_RETRY_MAX_ATTEMPTS, PAD_RETRY_BASE_DELAY). A field at 0
// (or below) takes the package default.
type ResilienceConfig struct {
	// Threshold is the consecutive retryable failures that open a provider's circuit.
	Threshold int
	// OpenFor is how long an open circuit refuses calls before it lets a probe through.
	OpenFor time.Duration
	// Attempts is how many times a call is made at most, the first included.
	Attempts int
	// BaseDelay is the first backoff between attempts; it doubles each attempt.
	BaseDelay time.Duration
}

// withDefaults fills the unset fields with the package defaults.
func (r ResilienceConfig) withDefaults() ResilienceConfig {
	if r.Threshold <= 0 {
		r.Threshold = cbFailureThreshold
	}
	if r.OpenFor <= 0 {
		r.OpenFor = cbOpenDuration
	}
	if r.Attempts <= 0 {
		r.Attempts = retryMaxAttempts
	}
	if r.BaseDelay <= 0 {
		r.BaseDelay = retryBaseDelay
	}
	return r
}

// NewProviderFactory builds the factory. resilience is the policy of every chain it builds; nil takes the package
// defaults. The budget gate and the provider policy are options, given at construction (the desktop and the
// tests build factories without them).
func NewProviderFactory(getKey func(scope, provider string) (string, error), copilotAuth *CopilotAuth, recorder UsageRecorder, resilience *ResilienceConfig, opts ...FactoryOption) *ProviderFactory {
	f := &ProviderFactory{getKey: getKey, copilotAuth: copilotAuth, recorder: recorder}
	if resilience != nil {
		f.resilience = *resilience
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
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

// storageKey returns the keyring key name of a provider's credential (ProviderSpec.Keys.Primary); an unknown
// id is its own key.
func storageKey(providerID string) string {
	if s, ok := lookupSpec(providerID); ok {
		return s.Keys.Primary
	}
	return providerID
}

// credentialRule is whose credential a builder resolves.
type credentialRule int

const (
	// memberOnly: the caller's own key (For, ForEmbedding).
	memberOnly credentialRule = iota
	// memberFirst: the caller's own key, else the organisation's shared one (ForOrg).
	memberFirst
	// orgFirst: the organisation's shared key, else the caller's own (ForEmbeddingOrg).
	orgFirst
	// orgOnly: the organisation's shared key, nothing else (ForOrgCredential).
	orgOnly
)

// resolveCredential reads the credential a chain runs on by the spec's rule: a provider that is never shared
// (no CapOrgShared — Copilot, a person's subscription) collapses every org rule onto the member's own key, a
// spec Resolver reads the keys itself, and the rest read Keys.Primary under rule. memoScope and usageOrg are
// as wrapChainScoped wants them.
func (f *ProviderFactory) resolveCredential(spec *ProviderSpec, scope, orgID string, rule credentialRule) (cred Credential, memoScope, usageOrg string, err error) {
	if !spec.Caps.Has(CapOrgShared) {
		if rule == orgOnly {
			return Credential{}, "", "", fmt.Errorf("%s: %w", spec.ID, ErrKeyNotConfigured)
		}
		rule = memberOnly
	}
	if spec.Resolver != nil {
		return spec.Resolver(f, scope, orgID)
	}
	var key string
	switch rule {
	case memberFirst:
		key, memoScope, err = f.resolveKey(scope, orgID, spec.ID)
	case orgFirst:
		key, memoScope, err = f.resolveOrgFirstKey(scope, orgID, spec.ID)
	case orgOnly:
		if orgID == "" || f.getOrgKey == nil {
			return Credential{}, "", "", fmt.Errorf("%s: %w", spec.ID, ErrKeyNotConfigured)
		}
		key, err = f.getOrgKey(orgID, spec.Keys.Primary)
		if err != nil {
			return Credential{}, "", "", fmt.Errorf("get %s org key: %w", spec.ID, err)
		}
		if key == "" {
			return Credential{}, "", "", fmt.Errorf("%s: %w", spec.ID, ErrKeyNotConfigured)
		}
		memoScope = OrgScope(orgID)
	default:
		key, err = f.getKey(scope, spec.Keys.Primary)
		if err != nil {
			return Credential{}, "", "", fmt.Errorf("get %s key: %w", spec.ID, err)
		}
		if key == "" {
			return Credential{}, "", "", fmt.Errorf("%s: %w", spec.ID, ErrKeyNotConfigured)
		}
		memoScope = scope
	}
	if err != nil {
		return Credential{}, "", "", err
	}
	return Credential{Key: key}, memoScope, credentialOrg(orgID, memoScope), nil
}

// build is every builder: the policy asked first (a refused provider never reads a key), the spec's
// constructor on the credential rule resolves, the embedding overrides applied when the chain is for embedding
// and the provider can embed, wrapped in the resilience chain. An embedding deployment missing from an ORG
// credential is ErrOrgEmbeddingDeploymentMissing (ForEmbeddingOrg).
func (f *ProviderFactory) build(ctx context.Context, scope, orgID, providerID string, rule credentialRule, forEmbedding bool, embeddingModel string) (Provider, error) {
	spec, ok := lookupSpec(providerID)
	if !ok {
		return nil, unknownProviderError(providerID)
	}
	if err := f.admitProvider(ctx, scope, orgID, providerID); err != nil {
		return nil, err
	}
	cred, memoScope, usageOrg, err := f.resolveCredential(spec, scope, orgID, rule)
	if err != nil {
		return nil, err
	}
	p := spec.Ctor(cred)
	if forEmbedding && spec.Caps.Has(CapEmbeddings) {
		if p, err = f.prepareEmbedder(p, providerID, embeddingModel); err != nil {
			if usageOrg != "" && errors.Is(err, ErrEmbeddingDeploymentMissing) {
				return nil, fmt.Errorf("%s: %w", providerID, ErrOrgEmbeddingDeploymentMissing)
			}
			return nil, err
		}
	}
	return f.wrapChainScoped(p, scope, memoScope, usageOrg, providerID), nil
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
	r := f.resilience.withDefaults() // a ProviderFactory literal (tests) has none set
	cb := NewCircuitBreakerProviderWithConfig(p, r.Threshold, r.OpenFor)
	rp := NewRetryingProviderWithConfig(cb, r.Attempts, r.BaseDelay)

	ap := newAuditedProvider(NewTracedProvider(rp), f.recorder, scope, providerID)
	ap.memoScope = memoScope
	ap.usageOrg = usageOrg
	ap.budget = f.budget
	// The wrappers above hide the raw provider's optional methods; the
	// audited wrapper reads what it needs from p before they do.
	ap.adoptRawHints(p)
	return ap
}

// ForOrg is For with the organisation's shared credential as the member-first fallback: the caller's own key
// when they have one, else the org's (chain memos in OrgScope). A provider without CapOrgShared (Copilot) is
// always the member's; an org never has one. An empty orgID, or no org store, behaves exactly as For. ctx
// carries the policy's reads and the budget scope of an org flow (every builder).
func (f *ProviderFactory) ForOrg(ctx context.Context, scope, orgID, providerID string) (Provider, error) {
	return f.build(ctx, scope, orgID, providerID, memberFirst, false, "")
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
func (f *ProviderFactory) ForAzureConnection(ctx context.Context, scope string, ref AzureConnectionRef, models []AzureRegisteredModel) (Provider, error) {
	if err := f.admitProvider(ctx, scope, ref.OrgID, azureFoundryProviderID); err != nil {
		return nil, err
	}
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
func (f *ProviderFactory) ForAzureConnectionEmbedding(ctx context.Context, scope string, ref AzureConnectionRef, entry AzureRegisteredModel) (Provider, error) {
	if entry.Kind != AzureModelKindEmbedding || !azureDeploymentName.MatchString(strings.TrimSpace(entry.Deployment)) {
		if ref.OrgID != "" {
			return nil, fmt.Errorf("%s: %w", azureFoundryProviderID, ErrOrgEmbeddingDeploymentMissing)
		}
		return nil, fmt.Errorf("%s: %w", azureFoundryProviderID, ErrEmbeddingDeploymentMissing)
	}
	if err := f.admitProvider(ctx, scope, ref.OrgID, azureFoundryProviderID); err != nil {
		return nil, err
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
// in OrgScope. A provider without CapOrgShared (Copilot) is never shared: ErrKeyNotConfigured.
func (f *ProviderFactory) ForOrgCredential(ctx context.Context, scope, orgID, providerID string) (Provider, error) {
	return f.build(ctx, scope, orgID, providerID, orgOnly, false, "")
}

// ForEmbeddingOrg is ForEmbedding with the organisation's shared credential PREFERRED: the org's knowledge
// base is one corpus and must be embedded with one resource, whichever member uploads or searches. When the
// ORG's credential is resolved and cannot embed (a Foundry document without an embedding deployment) the
// error is ErrOrgEmbeddingDeploymentMissing, on which the rag service refuses the personal-key fallback —
// a member's key must not seed the org corpus; a member's OWN document keeps the plain sentinel.
func (f *ProviderFactory) ForEmbeddingOrg(ctx context.Context, scope, orgID, providerID, embeddingModel string) (Provider, error) {
	return f.build(ctx, scope, orgID, providerID, orgFirst, true, embeddingModel)
}

// For builds the provider on the caller's own credential (the spec's Resolver for Copilot's dual auth).
func (f *ProviderFactory) For(ctx context.Context, scope, providerID string) (Provider, error) {
	return f.build(ctx, scope, "", providerID, memberOnly, false, "")
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
// carries it; a provider that cannot embed (no CapEmbeddings) ignores the model and behaves as For.
func (f *ProviderFactory) ForEmbedding(ctx context.Context, scope, providerID, embeddingModel string) (Provider, error) {
	return f.build(ctx, scope, "", providerID, memberOnly, true, embeddingModel)
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
// limits) without an API key (the spec's constructor on an empty credential); it intentionally omits Chat and
// Stream — use For for a functional Provider. nil for an unknown id.
func GetMetadataProvider(providerID string) MetadataProvider {
	if spec, ok := lookupSpec(providerID); ok {
		return spec.Ctor(Credential{})
	}
	return nil
}
