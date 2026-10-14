package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/cache"
	"pad-core/logger"
	"pad-core/models"
)

// modelListings caches a configured provider's live model listing per credential scope (ADR 0017): a listing is
// what that credential may use, so the scope it ran on (a user's own keys, an org's shared ones, the local user's)
// is its tenant. The storage is the service's own (per-key and per-scope invalidation marks, a TTL per listing).
var modelListings = cache.Declare("providers.model-listings",
	cache.Keys(cache.KindTenant, cache.KindParam),
	cache.Shared(cache.MechanismInvalidation, "a credential change on any replica drops the listings on every replica once committed; the 10 min TTL bounds a lost message"))

// credentialTenant is the cache tenant of a credential scope: an org's shared credential (ai.OrgScope), a user's
// own keys, or the local user's ("").
func credentialTenant(scope string) cache.Part {
	if orgID, ok := strings.CutPrefix(scope, ai.OrgScope("")); ok {
		return cache.Org(orgID)
	}
	return callerTenant(scope)
}

// modelListingKey keys scope's listing of providerID; the zero Key (never cached) for an org scope without an id.
func modelListingKey(scope, providerID string) cache.Key {
	t := credentialTenant(scope)
	if t.IsZero() {
		return cache.Key{}
	}
	k, _ := modelListings.Key(t, cache.Param("provider", providerID))
	return k
}

// dynamicModelTTL bounds how long a CONFIGURED provider's live model list is reused (else every Settings open fans out upstream calls).
const dynamicModelTTL = 10 * time.Minute

// emptyListingTTL caches an EMPTY live listing briefly: no second upstream call, fills in soon.
const emptyListingTTL = 30 * time.Second

// invalidation is a per-key or per-scope invalidation mark: the generation it was made at and when.
type invalidation struct {
	gen uint64
	at  time.Time
}

// invalidationFence keeps a mark long past any in-flight listing (liveModelsBudget + grace, or a caller's deadline).
const invalidationFence = dynamicModelTTL

// liveModelsBudget bounds each provider's live listing so the response stays under the client timeout; a miss falls back to metadata. Variable for tests.
var liveModelsBudget = 8 * time.Second

// liveModelsGrace is how long ListProviders waits past liveModelsBudget for a listing that ignores its context.
const liveModelsGrace = 500 * time.Millisecond

// ProviderService manages AI provider configuration, authentication, and connectivity.
type ProviderService struct {
	auth        *ai.GitHubAuth
	copilotAuth *ai.CopilotAuth
	factory     *ai.ProviderFactory
	secrets     KeyStore
	// orgKeys holds organisations' shared credentials; nil on the desktop and in local mode.
	orgKeys OrgKeyStore
	// azure is the Azure AI Foundry model registry; nil (tests) keeps the single-document behaviour.
	azure *AzureModelService
	// profile is the running deployment profile (WithProfile): a provider is offered when its spec's Modes name
	// it. "" (a service built without one, in tests) stands in the local-models egress gate.
	profile capability.Profile

	modelMu    sync.Mutex
	modelCache map[cache.Key]modelCacheEntry // modelListingKey(scope, providerID)
	// modelGen is a monotonic stamp bumped by every invalidation; a listing writes back only if not invalidated after its stamp.
	modelGen         uint64
	invalidatedKeys  map[cache.Key]invalidation
	invalidatedScope map[string]invalidation
	// invalidatedAll is the generation of the last whole-cache drop (InvalidateAllModels).
	invalidatedAll uint64

	// bus carries the drops that are not a domain event's to the other replicas (WithProviderEvents); nil announces nothing.
	bus *eventbus.Bus
}

type modelCacheEntry struct {
	models []ai.ModelInfo
	// defaultModel is the CONFIGURED provider's default, which can differ from the metadata provider's.
	defaultModel string
	expiresAt    time.Time
}

// ProviderOption configures NewProviderService.
type ProviderOption func(*ProviderService)

// WithProfile names the running deployment profile (capability.Of(cfg).Profile()); the listing and the
// catalogue offer a provider when its spec's Modes include it.
func WithProfile(p capability.Profile) ProviderOption {
	return func(s *ProviderService) { s.profile = p }
}

// WithProviderEvents lets the service announce the listing drops that no CredentialChanged announces (a key saved
// through the GitHub or Copilot sign-in, the local and Azure configurations, an organisation's Azure credentials
// disabled as a side effect of an admin's demotion) to the other replicas on bus. nil announces nothing.
func WithProviderEvents(bus *eventbus.Bus) ProviderOption {
	return func(s *ProviderService) { s.bus = bus }
}

// NewProviderService builds the provider service. orgKeys holds organisations' shared credentials (nil leaves
// the org methods reporting ErrOrgKeyStoreUnavailable); azure is the Azure AI Foundry model registry the
// single-document routes act on as resource "default" (nil keeps the single document).
func NewProviderService(auth *ai.GitHubAuth, copilotAuth *ai.CopilotAuth, factory *ai.ProviderFactory, secrets KeyStore, orgKeys OrgKeyStore, azure *AzureModelService, opts ...ProviderOption) *ProviderService {
	s := &ProviderService{
		auth:        auth,
		copilotAuth: copilotAuth,
		factory:     factory,
		secrets:     secrets,
		orgKeys:     orgKeys,
		azure:       azure,
		modelCache:  make(map[cache.Key]modelCacheEntry),

		invalidatedKeys:  make(map[cache.Key]invalidation),
		invalidatedScope: make(map[string]invalidation),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// AzureModels is the wired Azure AI Foundry model registry (nil without one).
func (s *ProviderService) AzureModels() *AzureModelService { return s.azure }

// ProviderCodeUnknown is the client code for a provider id no spec describes (ADR 0025): 400 on /api/keys/*
// and chat create, the ok:false result of the connection test, the stream error of a turn that reaches the
// factory with one.
const ProviderCodeUnknown = "PROVIDER_UNKNOWN"

// ErrProviderUnknown is ai.ErrUnknownProvider as the services return it; match with errors.Is.
var ErrProviderUnknown = ai.ErrUnknownProvider

// azureListing is the Azure AI Foundry model registry as the azure-foundry spec's ai.ListingHook: the provider
// is configured when the caller (or their organisation) holds a resource, and the picker lists the registry's
// entries without a live listing. DI installs it on the spec (ai.InstallListing); a service built with the
// registry reaches it through listingHook as well, so a test that wires a registry needs no process-global
// install and tests without one keep the single-document behaviour.
type azureListing struct {
	registry *AzureModelService
	// orgKeys answers for an organisation where organisations cannot hold registries (its shared document).
	orgKeys OrgKeyStore
}

// AzureListing is the registry as the azure-foundry spec's ListingHook (nil without a registry); orgKeys is
// the organisation keystore the shared document lives in where organisations hold no registry.
func AzureListing(a *AzureModelService, orgKeys OrgKeyStore) ai.ListingHook {
	if a == nil {
		return nil
	}
	return azureListing{registry: a, orgKeys: orgKeys}
}

func (l azureListing) Configured(ctx context.Context, scope string) (bool, error) {
	return l.registry.HasPersonalResources(ctx, scope)
}

// OrgConfigured: the organisation's registry when organisations can hold one here, else its shared document.
func (l azureListing) OrgConfigured(ctx context.Context, orgID string) (bool, error) {
	if l.registry.OrgRegistriesAvailable() {
		return l.registry.OrgHasResources(ctx, orgID)
	}
	if l.orgKeys == nil {
		return false, nil
	}
	return l.orgKeys.HasOrg(orgID, ai.AzureFoundryProviderID)
}

func (l azureListing) Picker(ctx context.Context, scope, orgID string) (ai.ProviderListing, error) {
	listing, err := l.registry.PickerListing(ctx, scope, orgID)
	if err != nil {
		return ai.ProviderListing{}, err
	}
	return ai.ProviderListing{
		Models:           listing.Models,
		DefaultModel:     listing.DefaultModel,
		Configured:       listing.Configured,
		Shared:           listing.Shared,
		SuggestDiscovery: listing.SuggestDiscovery,
	}, nil
}

// connectionTester is a family whose connection test is its own (the registry's resource probe, no model call).
// ok is false when the family has no test for that scope (an organisation without registries here).
type connectionTester interface {
	testConnection(ctx context.Context, as AzureScope) (res *models.ProviderTestResult, ok bool, err error)
}

func (l azureListing) testConnection(ctx context.Context, as AzureScope) (*models.ProviderTestResult, bool, error) {
	if as.OrgID != "" && !l.registry.OrgRegistriesAvailable() {
		return nil, false, nil
	}
	res, err := l.registry.legacyTest(ctx, as)
	return res, true, err
}

// registryFamily reports whether providerID is the family the Azure AI Foundry registry a serves. The one
// place that knows the registry was written for the azure-foundry spec; every other site asks a hook.
func registryFamily(a *AzureModelService, providerID string) bool {
	return a != nil && providerID == ai.AzureFoundryProviderID
}

// listingHook is providerID's family hook: the registry this service was built with, else the one installed
// on the spec (DI), else nil — the keystore keys and the catalogue or a live listing answer for it.
func (s *ProviderService) listingHook(providerID string) ai.ListingHook {
	if registryFamily(s.azure, providerID) {
		return azureListing{registry: s.azure, orgKeys: s.orgKeys}
	}
	if spec, ok := ai.SpecOf(providerID); ok {
		return spec.Listing
	}
	return nil
}

// runningProfile is the profile the listing filters on: the configured one, else the one the local-models
// egress gate implies (a service built without a profile, in tests).
func (s *ProviderService) runningProfile() capability.Profile {
	if s.profile != "" {
		return s.profile
	}
	if ai.LocalModelsAvailable() {
		return capability.Desktop
	}
	return capability.Cloud
}

// offered reports whether spec is offered in this deployment: its Modes name the running profile.
func (s *ProviderService) offered(spec ai.ProviderSpec) bool {
	return slices.Contains(spec.Modes, s.runningProfile())
}

// OrgKeysAvailable reports whether this deployment can hold organisation credentials (cloud mode).
func (s *ProviderService) OrgKeysAvailable() bool { return s.orgKeys != nil }

// ErrOrgKeyStoreUnavailable is returned by org credential methods where no org secret store exists (desktop, local mode).
var ErrOrgKeyStoreUnavailable = errors.New("organisation credentials require the database keystore (cloud mode)")

// ErrOrgCredentialUnreadable: the org's shared document exists but cannot be read (not "not configured"). Maps to 500.
var ErrOrgCredentialUnreadable = errors.New("the organisation's shared Azure AI Foundry configuration could not be read")

// ErrOrgCredentialConflict: an org save carried a stale updatedAt. Nothing was written; maps to 409 CONFLICT.
var ErrOrgCredentialConflict = errors.New("the organisation's shared configuration changed since it was loaded — reload it and apply your edit again")

// OrgRoleUsesSharedCredential reports whether a role may run on the org's shared credential: admins and members only.
func OrgRoleUsesSharedCredential(role auth.Role) bool {
	return role == auth.RoleAdmin || role == auth.RoleMember
}

// listingScope names whose credential a listing runs on, so the cached listing is keyed and invalidated by that credential.
type listingScope struct {
	scope      string
	orgID      string
	cacheScope string
	shared     bool
}

// personalScope is the listing scope of a caller running on their own keys.
func personalScope(scope string) listingScope {
	return listingScope{scope: scope, cacheScope: scope}
}

// dynamicModels returns a configured provider's live list, cached per (scope, provider); alias providers and invalidated runs are never cached.
func (s *ProviderService) dynamicModels(ctx context.Context, ls listingScope, providerID string) (models []ai.ModelInfo, defaultModel string, ok bool) {
	scope := ls.cacheScope
	key := modelListingKey(scope, providerID)
	cacheable := !ai.UsesModelAliases(providerID) && !key.IsZero()
	s.modelMu.Lock()
	if e, hit := s.modelCache[key]; cacheable && hit && time.Now().Before(e.expiresAt) {
		s.modelMu.Unlock()
		modelListings.RecordHit()
		return e.models, e.defaultModel, true
	}
	gen := s.modelGen
	s.modelMu.Unlock()
	if cacheable {
		modelListings.RecordMiss()
	}

	// Member-first: the org's credential exactly when the caller has none.
	realProvider, err := s.factory.ForOrg(ctx, ls.scope, ls.orgID, providerID)
	if err != nil {
		return nil, "", false
	}
	models, err = realProvider.Models(ctx)
	if err != nil {
		logger.Warn("list dynamic provider models", "provider", providerID, "error", err)
		return nil, "", false
	}
	defaultModel = realProvider.DefaultModel()
	if cacheable && ctx.Err() == nil {
		ttl := dynamicModelTTL
		if len(models) == 0 {
			ttl = emptyListingTTL // retry soon, but not on every Settings open
		}
		s.modelMu.Lock()
		if !s.invalidatedSinceLocked(key, scope, gen) {
			// Insert-time pruning: expired entries are never removed by reads, so a departed user's keys cannot accumulate.
			s.pruneModelCacheLocked(time.Now())
			if _, replaced := s.modelCache[key]; replaced {
				modelListings.RecordEviction(cache.EvictReplaced)
			}
			s.modelCache[key] = modelCacheEntry{models: models, defaultModel: defaultModel, expiresAt: time.Now().Add(ttl)}
			modelListings.RecordStore()
		}
		s.modelMu.Unlock()
	}
	return models, defaultModel, true
}

// invalidatedSinceLocked reports whether key or its scope was invalidated after gen. Caller holds modelMu.
func (s *ProviderService) invalidatedSinceLocked(key cache.Key, scope string, gen uint64) bool {
	if inv, ok := s.invalidatedKeys[key]; ok && inv.gen > gen {
		return true
	}
	if s.invalidatedAll > gen {
		return true
	}
	inv, ok := s.invalidatedScope[scope]
	return ok && inv.gen > gen
}

// modelCacheMax bounds the live-listing cache's size, mirroring ai's azureModelsCacheMax.
const modelCacheMax = 512

// pruneModelCacheLocked drops expired entries first, then the closest to expiry at modelCacheMax. Caller holds modelMu.
func (s *ProviderService) pruneModelCacheLocked(now time.Time) {
	for k, e := range s.modelCache {
		if !now.Before(e.expiresAt) {
			delete(s.modelCache, k)
			modelListings.RecordEviction(cache.EvictExpired)
		}
	}
	s.pruneInvalidationsLocked(now)
	for len(s.modelCache) >= modelCacheMax {
		var oldest cache.Key
		oldestExp, first := time.Time{}, true
		for k, e := range s.modelCache {
			if first || e.expiresAt.Before(oldestExp) {
				oldest, oldestExp, first = k, e.expiresAt, false
			}
		}
		delete(s.modelCache, oldest)
		modelListings.RecordEviction(cache.EvictCapacity)
	}
}

// pruneInvalidationsLocked drops dead marks (past invalidationFence); runs on every insert AND invalidation. Caller holds modelMu.
func (s *ProviderService) pruneInvalidationsLocked(now time.Time) {
	for k, inv := range s.invalidatedKeys {
		if now.Sub(inv.at) > invalidationFence {
			delete(s.invalidatedKeys, k)
		}
	}
	for k, inv := range s.invalidatedScope {
		if now.Sub(inv.at) > invalidationFence {
			delete(s.invalidatedScope, k)
		}
	}
}

// InvalidateScope drops every cached listing of one scope and bumps the generation against in-flight listings.
func (s *ProviderService) InvalidateScope(scope string) {
	s.modelMu.Lock()
	now := time.Now()
	s.pruneInvalidationsLocked(now)
	s.modelGen++
	s.invalidatedScope[scope] = invalidation{gen: s.modelGen, at: now}
	tenant := credentialTenant(scope)
	for k := range s.modelCache {
		if parts := k.Parts(); len(parts) > 0 && parts[0] == tenant {
			delete(s.modelCache, k)
			modelListings.RecordEviction(cache.EvictDeleted)
		}
	}
	s.modelMu.Unlock()
}

// InvalidateAllModels drops every cached listing and refuses the listings in flight: what a replica does when it
// cannot tell which of its listings a lost invalidation concerned.
func (s *ProviderService) InvalidateAllModels() {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	s.modelGen++
	s.invalidatedAll = s.modelGen
	for k := range s.modelCache {
		delete(s.modelCache, k)
		modelListings.RecordEviction(cache.EvictDeleted)
	}
}

// InvalidateModels drops one (scope, provider) cached listing so a config change shows immediately.
func (s *ProviderService) InvalidateModels(scope, providerID string) {
	key := modelListingKey(scope, providerID)
	s.modelMu.Lock()
	now := time.Now()
	s.pruneInvalidationsLocked(now)
	s.modelGen++
	s.invalidatedKeys[key] = invalidation{gen: s.modelGen, at: now}
	if _, ok := s.modelCache[key]; ok {
		delete(s.modelCache, key)
		modelListings.RecordEviction(cache.EvictDeleted)
	}
	s.modelMu.Unlock()
}

// SubscribeModelCache drops s's cached live listings when the credentials they were listed with change: a key
// saved or deleted (one provider, or the whole scope when the event names none), an erased account's scope, a
// deleted organisation's shared scope. The cache's own generation fences listings in flight.
func SubscribeModelCache(bus *eventbus.Bus, s *ProviderService) {
	eventbus.Subscribe(bus, eventbus.Sync, "provider-model-cache", func(_ context.Context, e events.CredentialChanged) {
		if e.ProviderID == "" || e.Kind == events.CredentialScopeErased {
			s.InvalidateScope(e.Scope)
			return
		}
		s.InvalidateModels(e.Scope, e.ProviderID)
	}, eventbus.Replicated(func(e events.CredentialChanged) []eventbus.Invalidation {
		// "scope" is named even when empty (the deployment's); "provider" only for a single provider's listing.
		scope := eventbus.NewScope("scope", e.Scope)
		if e.ProviderID != "" && e.Kind != events.CredentialScopeErased {
			scope["provider"] = e.ProviderID
		}
		return []eventbus.Invalidation{{Key: "providers.model-listings", Scope: scope}}
	}, "providers.model-listings"))
	eventbus.Subscribe(bus, eventbus.Sync, "provider-model-cache", func(_ context.Context, e events.AccountErased) {
		s.InvalidateScope(e.KeyScope)
	}, eventbus.Replicated(func(e events.AccountErased) []eventbus.Invalidation {
		return []eventbus.Invalidation{{Key: "providers.model-listings", Scope: eventbus.NewScope("scope", e.KeyScope)}}
	}, "providers.model-listings"))
	eventbus.Subscribe(bus, eventbus.Sync, "provider-model-cache", func(_ context.Context, e events.OrgMemberChanged) {
		if e.Kind == events.OrgDeleted && e.OrgID != "" {
			s.InvalidateScope(ai.OrgScope(e.OrgID))
		}
	}, eventbus.Replicated(func(e events.OrgMemberChanged) []eventbus.Invalidation {
		if e.Kind != events.OrgDeleted || e.OrgID == "" {
			return nil
		}
		return []eventbus.Invalidation{{Key: "providers.model-listings", Scope: eventbus.NewScope("scope", ai.OrgScope(e.OrgID))}}
	}, "providers.model-listings"))
	// What a peer dropped: one provider's listing, a scope's, or (an empty scope: a resync) every listing.
	bus.OnRemote("providers.model-listings", func(sc eventbus.Scope) {
		scope, named := sc["scope"]
		switch {
		case !named:
			s.InvalidateAllModels()
		case sc["provider"] != "":
			s.InvalidateModels(scope, sc["provider"])
		default:
			s.InvalidateScope(scope)
		}
	})
}

// invalidateScopeEverywhere drops scope's listings here and tells the other replicas to drop theirs, for a change no
// CredentialChanged announces.
func (s *ProviderService) invalidateScopeEverywhere(scope string) {
	s.InvalidateScope(scope)
	s.bus.Invalidate(context.Background(), "providers.model-listings", eventbus.NewScope("scope", scope))
}

// invalidateModelsEverywhere is invalidateScopeEverywhere for one provider's listing: a key saved, replaced or
// revoked through a method of this service (the GitHub and Copilot sign-ins, the local and Azure configurations),
// which no CredentialChanged announces.
func (s *ProviderService) invalidateModelsEverywhere(scope, providerID string) {
	s.InvalidateModels(scope, providerID)
	s.bus.Invalidate(context.Background(), "providers.model-listings", eventbus.NewScope("scope", scope, "provider", providerID))
}

// chatModels drops non-chat entries (embedding deployments): kept in Models() for pricing, never in the chat picker.
func chatModels(in []ai.ModelInfo) []ai.ModelInfo {
	out := make([]ai.ModelInfo, 0, len(in))
	for _, m := range in {
		if !m.Embedding {
			out = append(out, m)
		}
	}
	return out
}

// chooseDefaultModel picks the UI preselection: the provider's default, else the metadata fallback, else the first chat-capable entry.
func chooseDefaultModel(preferred string, models []ai.ModelInfo, fallback string) string {
	has := func(id string) bool {
		for _, m := range models {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	if preferred != "" && has(preferred) {
		return preferred
	}
	if fallback != "" && has(fallback) {
		return fallback
	}
	for _, m := range models {
		if !m.Embedding && !ai.IsAgentModel(m.ID) && !isNonChatModelID(m.ID) {
			return m.ID
		}
	}
	for _, m := range models {
		if !m.Embedding {
			return m.ID
		}
	}
	if fallback != "" {
		return fallback
	}
	return preferred
}

// nonChatModelFamilies are id fragments of models the chat route cannot serve (image, speech, transcription, realtime, video, embedding).
var nonChatModelFamilies = []string{"dall-e", "gpt-image", "whisper", "tts", "transcribe", "realtime", "sora", "embed"}

// isNonChatModelID keeps a non-chat deployment from being chosen as the chat default or connection-test model.
func isNonChatModelID(id string) bool {
	lower := strings.ToLower(id)
	for _, f := range nonChatModelFamilies {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return false
}

// connectionTestModels lists the test's candidates in order: default first (aliases: the configured deployment), free model next.
func (s *ProviderService) connectionTestModels(ctx context.Context, provider ai.Provider, providerID string) []string {
	var candidates []string
	add := func(id string) {
		if id == "" {
			return
		}
		for _, c := range candidates {
			if c == id {
				return
			}
		}
		candidates = append(candidates, id)
	}
	def := provider.DefaultModel()
	add(def)
	add(provider.FreeModel())
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	live, err := provider.Models(lctx)
	if err != nil {
		return candidates
	}
	chat := chatModels(live)
	for _, m := range chat {
		if m.ID == def {
			return candidates
		}
	}
	pick := chooseDefaultModel("", chat, "")
	if ai.UsesModelAliases(providerID) {
		add(pick) // the configured deployment stays first (#6)
		return candidates
	}
	if pick != "" && pick != def {
		rest := candidates
		candidates = append([]string{pick}, rest...)
	}
	return candidates
}

// azureProjectProbe explains an empty Azure candidate list (an Entra ID + project document naming no deployment). Replaceable in tests.
var azureProjectProbe = ai.ProbeAzureFoundryProject

// liveListing is one configured provider's live listing (dynamicModels).
type liveListing struct {
	models       []ai.ModelInfo
	defaultModel string
	ok           bool
}

// startLiveListing runs dynamicModels for one provider under its own liveModelsBudget, bounded by the request context.
func (s *ProviderService) startLiveListing(ctx context.Context, ls listingScope, providerID string) <-chan liveListing {
	ch := make(chan liveListing, 1)
	go func() {
		var l liveListing
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("list dynamic provider models panicked", "provider", providerID, "panic", r)
				l = liveListing{}
			}
			ch <- l
		}()
		lctx, cancel := context.WithTimeout(ctx, liveModelsBudget)
		defer cancel()
		l.models, l.defaultModel, l.ok = s.dynamicModels(lctx, ls, providerID)
	}()
	return ch
}

// awaitLiveListing returns the listing once it arrives, or a failed one when wait expires first.
func awaitLiveListing(wait context.Context, ch <-chan liveListing) liveListing {
	select {
	case l := <-ch:
		return l
	default:
	}
	select {
	case l := <-ch:
		return l
	case <-wait.Done():
		return liveListing{}
	}
}

// isConfigured reports whether the caller has credentials for a provider: the family's hook answers for a
// family (the registry's resources), else the spec's keystore rows — the device-flow token or the stored key.
func (s *ProviderService) isConfigured(scope, providerID string) bool {
	if hook := s.listingHook(providerID); hook != nil {
		ok, err := hook.Configured(context.Background(), scope)
		if err != nil {
			logger.Warn("provider listing: reading the caller's configuration failed", "provider", providerID, "error", err)
		}
		return ok
	}
	spec, known := ai.SpecOf(providerID)
	if !known {
		return false
	}
	if spec.Keys.OAuthToken != "" {
		if ok, _ := s.secrets.Has(scope, spec.Keys.OAuthToken); ok {
			return true
		}
	}
	ok, _ := s.secrets.Has(scope, spec.Keys.Primary)
	return ok
}

// isOrgConfigured reports whether the organisation holds a shared credential: never for a provider that is
// not org-shareable (Copilot, a person's subscription), the family's hook for a family, else the spec's key.
func (s *ProviderService) isOrgConfigured(orgID, providerID string) bool {
	if s.orgKeys == nil || orgID == "" {
		return false
	}
	spec, known := ai.SpecOf(providerID)
	if !known || !spec.Caps.Has(ai.CapOrgShared) {
		return false
	}
	if hook := s.listingHook(providerID); hook != nil {
		ok, err := hook.OrgConfigured(context.Background(), orgID)
		if err != nil {
			logger.Warn("provider listing: reading the organisation's configuration failed", "provider", providerID, "org", orgID, "error", err)
		}
		return ok
	}
	ok, _ := s.orgKeys.HasOrg(orgID, spec.Keys.Primary)
	return ok
}

// resolveListing decides whose credential a provider is listed on (G7, member-first): the caller's own, else the org's.
func (s *ProviderService) resolveListing(scope, orgID, providerID string) (ls listingScope, configured bool) {
	if s.isConfigured(scope, providerID) {
		return personalScope(scope), true
	}
	if s.isOrgConfigured(orgID, providerID) {
		return listingScope{scope: scope, orgID: orgID, cacheScope: ai.OrgScope(orgID), shared: true}, true
	}
	return personalScope(scope), false
}

// ListProviders lists every provider of the running profile with models and default; live listings run
// concurrently under liveModelsBudget. A family with a Listing hook is listed from it (no live listing); a
// loopback provider only once the caller configured it (the catalogue lists it regardless).
func (s *ProviderService) ListProviders(ctx context.Context, scope, orgID string) (providers []models.ProviderInfo, err error) {
	defer logger.Guard("App.ListProviders", &err)

	specs := ai.Specs()
	metaProviders := make([]ai.MetadataProvider, len(specs))
	configured := make([]bool, len(specs))
	shared := make([]bool, len(specs))
	live := make([]<-chan liveListing, len(specs))
	for i, spec := range specs {
		if !s.offered(spec) || (spec.Egress.Kind == ai.EgressLoopback && !s.isConfigured(scope, spec.ID)) {
			continue
		}
		metaProviders[i] = ai.GetMetadataProvider(spec.ID)
		if metaProviders[i] == nil {
			continue
		}
		if s.listingHook(spec.ID) != nil {
			continue // listed from the family's hook below: no live listing
		}
		var ls listingScope
		ls, configured[i] = s.resolveListing(scope, orgID, spec.ID)
		shared[i] = ls.shared
		if configured[i] {
			live[i] = s.startLiveListing(ctx, ls, spec.ID)
		}
	}
	wait, cancel := context.WithTimeout(ctx, liveModelsBudget+liveModelsGrace)
	defer cancel()

	for i, spec := range specs {
		p := metaProviders[i]
		if p == nil {
			continue
		}
		if hook := s.listingHook(spec.ID); hook != nil {
			info, err := s.hookProviderInfo(ctx, scope, orgID, spec, p, hook)
			if err != nil {
				return nil, err
			}
			providers = append(providers, info)
			continue
		}

		modelInfos, mErr := p.Models(ctx)
		if mErr != nil {
			logger.Warn("list provider models", "provider", spec.ID, "error", mErr)
		}
		defaultModel := p.DefaultModel()
		if live[i] != nil {
			if l := awaitLiveListing(wait, live[i]); l.ok {
				modelInfos = chatModels(l.models)
				switch {
				case spec.Caps.Has(ai.CapModelAliases) && l.defaultModel != "":
					// The user's configuration names the default (the chat deployment); it wins even when the listing lacks it.
					defaultModel = l.defaultModel
				case spec.Caps.Has(ai.CapModelAliases):
					// Nothing named: the first chat-capable listed deployment, never the catalogue's reference name.
					defaultModel = chooseDefaultModel("", modelInfos, "")
				default:
					defaultModel = chooseDefaultModel(l.defaultModel, modelInfos, defaultModel)
				}
			}
		}

		modelDetails := make([]models.ModelDetail, 0, len(modelInfos))
		for _, m := range modelInfos {
			modelDetails = append(modelDetails, models.ModelDetail{
				ID:             m.ID,
				DisplayName:    m.DisplayName,
				ContextLimit:   m.ContextLimit,
				InputCostPerM:  m.Pricing.InputCostPerM,
				OutputCostPerM: m.Pricing.OutputCostPerM,
				SupportsTools:  p.SupportsTools(),
			})
		}

		contextLimit := p.ContextLimit()
		// A provider without a catalogue serves whatever window the caller configured, which the key-less
		// metadata provider cannot know.
		if spec.Catalog == "" && len(modelInfos) > 0 && modelInfos[0].ContextLimit > 0 {
			contextLimit = modelInfos[0].ContextLimit
		}

		providers = append(providers, specFields(models.ProviderInfo{
			ID:           spec.ID,
			Name:         spec.Name,
			Configured:   configured[i],
			Shared:       shared[i],
			Models:       modelDetails,
			DefaultModel: defaultModel,
			ContextLimit: contextLimit,
			AuthType:     string(spec.AuthType),
		}, spec))
	}

	return providers, nil
}

// hookProviderInfo builds a family's listing entry from its hook (network-free).
func (s *ProviderService) hookProviderInfo(ctx context.Context, scope, orgID string, spec ai.ProviderSpec, p ai.MetadataProvider, hook ai.ListingHook) (models.ProviderInfo, error) {
	listing, err := hook.Picker(ctx, scope, orgID)
	if err != nil {
		return models.ProviderInfo{}, fmt.Errorf("list the %s models: %w", spec.Name, err)
	}
	return specFields(models.ProviderInfo{
		ID:               spec.ID,
		Name:             spec.Name,
		Configured:       listing.Configured || listing.Shared,
		Shared:           listing.Shared,
		Models:           listing.Models,
		DefaultModel:     listing.DefaultModel,
		ContextLimit:     p.ContextLimit(),
		AuthType:         string(spec.AuthType),
		SuggestDiscovery: listing.SuggestDiscovery,
	}, spec), nil
}

// specFields adds the spec's optional wire fields to a listing entry (ADR 0025).
func specFields(info models.ProviderInfo, spec ai.ProviderSpec) models.ProviderInfo {
	cred := spec.WireCredential()
	info.Caps = spec.Caps.Names()
	info.Modes = spec.WireModes()
	info.Credential = &cred
	info.Pricing = string(spec.Pricing)
	info.Color = spec.Color
	info.Icon = spec.Icon
	if endpoints := providerEndpoints(spec); endpoints != (models.ProviderEndpoints{}) {
		info.Endpoints = &endpoints
		info.Configurable = true
	}
	return info
}

// Routes that manage a credential, named per provider in the catalogue.
const (
	keysSaveRoute        = "/api/keys/save"
	providerTestRoute    = "/api/providers/test"
	providerOAuthRoutes  = "/api/providers/%s/%s"
	githubModelsOAuthDir = "github"
)

// providerEndpoints names the routes a client manages spec's credential with: the key routes for a bare key,
// the device flow's four for an OAuth provider (mounted under their historical directories), the connection
// test alone for a document provider (its own panel saves it; an unknown document provider has no panel, so
// nothing is offered — "configured on the server"). The zero value means not configurable.
func providerEndpoints(spec ai.ProviderSpec) models.ProviderEndpoints {
	switch spec.Credential.Kind {
	case ai.CredentialAPIKey:
		return models.ProviderEndpoints{Save: keysSaveRoute, Test: providerTestRoute}
	case ai.CredentialOAuthDevice:
		dir := spec.ID
		if spec.ID == ai.GitHubModelsProviderID {
			dir = githubModelsOAuthDir
		}
		return models.ProviderEndpoints{
			Test:   providerTestRoute,
			Start:  fmt.Sprintf(providerOAuthRoutes, dir, "start"),
			Poll:   fmt.Sprintf(providerOAuthRoutes, dir, "poll"),
			Revoke: fmt.Sprintf(providerOAuthRoutes, dir, "revoke"),
			User:   fmt.Sprintf(providerOAuthRoutes, dir, "user"),
		}
	case ai.CredentialDocument:
		if spec.ID == ai.AzureFoundryProviderID || spec.ID == ai.LocalProviderID {
			return models.ProviderEndpoints{Test: providerTestRoute}
		}
	}
	return models.ProviderEndpoints{}
}

// ProviderCatalog is every registered provider of the running profile in display order, configured or not,
// with its credential schema and the routes that manage it (GET /api/providers/catalog). Network-free: the
// configured state is the keystore's (or the family hook's), never a live listing.
func (s *ProviderService) ProviderCatalog(ctx context.Context, scope, orgID string) (entries []models.ProviderCatalogEntry, err error) {
	defer logger.Guard("App.ProviderCatalog", &err)
	for _, spec := range ai.Specs() {
		if !s.offered(spec) {
			continue
		}
		endpoints := providerEndpoints(spec)
		entries = append(entries, models.ProviderCatalogEntry{
			ID:           spec.ID,
			Name:         spec.Name,
			AuthType:     string(spec.AuthType),
			Modes:        spec.WireModes(),
			Caps:         spec.Caps.Names(),
			Credential:   spec.WireCredential(),
			Endpoints:    endpoints,
			Configured:   s.isConfigured(scope, spec.ID) || s.isOrgConfigured(orgID, spec.ID),
			Color:        spec.Color,
			Icon:         spec.Icon,
			Pricing:      string(spec.Pricing),
			Configurable: endpoints != (models.ProviderEndpoints{}),
		})
	}
	return entries, nil
}

// unknownProviderResult is the connection test's answer for an id no spec describes: today's 200 shape, coded.
func unknownProviderResult(providerID string) *models.ProviderTestResult {
	return &models.ProviderTestResult{Ok: false, Error: fmt.Sprintf("unknown provider: %s", providerID), Code: ProviderCodeUnknown}
}

// TestProviderConnection sends a tiny chat request per candidate; model-level rejections try the next, outages stop.
func (s *ProviderService) TestProviderConnection(ctx context.Context, scope, providerID string) (result *models.ProviderTestResult, err error) {
	defer logger.Guard("App.TestProviderConnection", &err)

	if !ai.KnownProvider(providerID) {
		return unknownProviderResult(providerID), nil
	}
	if tester, ok := s.listingHook(providerID).(connectionTester); ok {
		// Deprecated for Azure AI Foundry (the registry's test route tests one resource or model): no model call.
		if res, handled, err := tester.testConnection(ctx, AzureScope{Scope: scope}); handled {
			return res, err
		}
	}
	provider, err := s.factory.For(ctx, scope, providerID)
	if err != nil {
		return &models.ProviderTestResult{Ok: false, Error: connectionTestError(providerID, "", err), Code: connectionTestCode(err)}, nil
	}
	return s.runConnectionTest(ctx, provider, providerID, func() string {
		raw, getErr := s.secrets.Get(scope, providerID)
		if getErr != nil {
			return ""
		}
		return raw
	}), nil
}

// TestOrgProviderConnection is TestProviderConnection for an org's SHARED credential — the provider is built from the org's credential only; scope is the admin's, for attribution.
func (s *ProviderService) TestOrgProviderConnection(ctx context.Context, scope, orgID, providerID string) (result *models.ProviderTestResult, err error) {
	defer logger.Guard("App.TestOrgProviderConnection", &err)
	if s.orgKeys == nil {
		return nil, ErrOrgKeyStoreUnavailable
	}
	if !ai.KnownProvider(providerID) {
		return unknownProviderResult(providerID), nil
	}
	if tester, ok := s.listingHook(providerID).(connectionTester); ok {
		// Deprecated: the organisation's resource "default", else its first.
		if res, handled, err := tester.testConnection(ctx, AzureScope{Scope: scope, OrgID: orgID}); handled {
			return res, err
		}
	}
	provider, err := s.factory.ForOrgCredential(ctx, scope, orgID, providerID)
	if err != nil {
		return &models.ProviderTestResult{Ok: false, Error: connectionTestError(providerID, "", err), Code: connectionTestCode(err)}, nil
	}
	return s.runConnectionTest(ctx, provider, providerID, func() string {
		raw, getErr := s.orgKeys.GetOrg(orgID, providerID)
		if getErr != nil {
			return ""
		}
		return raw
	}), nil
}

// runConnectionTest is the shared body of the connection tests; rawDoc returns the stored Azure document for the project probe.
func (s *ProviderService) runConnectionTest(ctx context.Context, provider ai.Provider, providerID string, rawDoc func() string) *models.ProviderTestResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	testModels := s.connectionTestModels(ctx, provider, providerID)
	if len(testModels) == 0 {
		msg := "no model to test: add a chat model to the provider under Settings → AI Accounts"
		if spec, _ := ai.SpecOf(providerID); spec.Egress.Kind == ai.EgressLoopback {
			msg = "no model found: start the local server, load a model and check the base URL"
			if !ai.LocalModelsAvailable() {
				msg = "local models are available in the desktop app only"
			}
		}
		if providerID == ai.AzureFoundryProviderID {
			// An Entra ID + project document may rely on the project listing; an empty listing's failure is the answer.
			if raw := rawDoc(); raw != "" {
				if perr := azureProjectProbe(ctx, raw); perr != nil {
					msg = connectionTestError(providerID, "the project endpoint", perr)
				}
			}
		}
		return &models.ProviderTestResult{Ok: false, Latency: int(time.Since(start).Milliseconds()), Error: msg}
	}

	var firstErr error
	for _, model := range testModels {
		_, chatErr := provider.Chat(ctx, ai.Request{
			Model:       model,
			Messages:    []ai.Message{{Role: "user", Content: "Hi"}},
			MaxTokens:   16,
			Temperature: 0,
		})
		if chatErr == nil {
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: true, Latency: latency}
		}
		if code := connectionTestCode(chatErr); code != "" {
			// The wrapper's pre-call hook refused the test (the daily budget): nothing was sent, no candidate helps.
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: false, Latency: latency, Error: chatErr.Error(), Code: code}
		}
		if errors.Is(chatErr, ai.ErrProviderPermissionDenied) || errors.Is(chatErr, ai.ErrEntraCredentialsRejected) || errors.Is(chatErr, ai.ErrEntraTokenRejected) || errors.Is(chatErr, ai.ErrClaudeEntraOnly) {
			// Auth failures whose one-line text is what the user needs (missing role, AADSTS code, Entra-only refusal).
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: false, Latency: latency, Error: connectionTestError(providerID, "", chatErr)}
		}
		if errors.Is(chatErr, ai.ErrApiKeyInvalid) {
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: false, Latency: latency, Error: "invalid API key"}
		}
		if errors.Is(chatErr, ai.ErrRateLimited) || errors.Is(chatErr, ai.ErrOutputLimit) {
			// The credentials work: a rate limit is the resource answering; an output-limit stop is hidden reasoning under the 16-token cap.
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: true, Latency: latency}
		}
		if errors.Is(chatErr, ai.ErrProviderDown) || errors.Is(chatErr, ai.ErrCircuitOpen) || errors.Is(chatErr, ai.ErrProviderConfigInvalid) {
			// Nothing model-level: the next candidate would only repeat the same retries against the same outage.
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: false, Latency: latency, Error: connectionTestError(providerID, "", chatErr)}
		}
		if firstErr == nil {
			firstErr = chatErr
		}
		if ctx.Err() != nil {
			break
		}
	}

	latency := int(time.Since(start).Milliseconds())
	return &models.ProviderTestResult{Ok: false, Latency: latency, Error: connectionTestError(providerID, "", firstErr)}
}

// connectionTestCode is the coded class of a connection-test failure the client branches on: the pre-call
// gates' refusals (the daily budget, the provider policy). "" for every other failure, which keeps its text.
func connectionTestCode(err error) string {
	switch {
	case errors.Is(err, ErrAIBudgetExceeded):
		return ChatErrBudgetExceeded
	case errors.Is(err, ErrAIBudgetUnavailable):
		return ChatErrBudgetUnavailable
	case errors.Is(err, ai.ErrProviderNotAllowed):
		return ProviderCodeNotAllowed
	case errors.Is(err, ai.ErrProviderPolicyUnavailable):
		return ProviderCodePolicyUnavailable
	}
	return ""
}

// connectionTestError is the test's text for err: a connect failure (DNS, dial, proxy, TLS, egress) is fixed copy — the raw text names addresses a client must never see.
func connectionTestError(providerID, target string, err error) string {
	what, ok := egress.DescribeFailure(err)
	if !ok {
		return err.Error()
	}
	logger.Warn("provider connection test: connection failure", "provider", providerID, "error", err)
	if target == "" {
		target = "the endpoint"
	}
	return "could not connect to " + target + ": " + what
}

// primaryKey and oauthTokenKey are the keystore rows of a provider, from its spec ("" for an unknown id).
func primaryKey(providerID string) string {
	spec, _ := ai.SpecOf(providerID)
	return spec.Keys.Primary
}

func oauthTokenKey(providerID string) string {
	spec, _ := ai.SpecOf(providerID)
	return spec.Keys.OAuthToken
}

// --- GitHub Models OAuth ---

func (s *ProviderService) StartGitHubAuth(ctx context.Context) (resp *ai.DeviceAuthResponse, err error) {
	defer logger.Guard("App.StartGitHubAuth", &err)
	return s.auth.StartDeviceFlow(ctx)
}

func (s *ProviderService) PollGitHubAuth(ctx context.Context, scope string, deviceCode string) (result *ai.GitHubAuthResult, err error) {
	defer logger.Guard("App.PollGitHubAuth", &err)

	result, err = s.auth.PollToken(ctx, deviceCode)
	if err != nil {
		return nil, err
	}

	if result.Status == "success" && result.Token != "" {
		if saveErr := s.secrets.Save(scope, primaryKey(ai.GitHubModelsProviderID), result.Token); saveErr != nil {
			logger.Error("failed to save github token", "error", saveErr)
		} else {
			// A replacement token may carry other entitlements: drop the previous token's cached listing (mirrors Revoke).
			s.invalidateModelsEverywhere(scope, ai.GitHubModelsProviderID)
		}
		result.Token = ""
	}

	return result, nil
}

func (s *ProviderService) RevokeGitHubAuth(scope string) (err error) {
	defer logger.Guard("App.RevokeGitHubAuth", &err)
	if err := s.secrets.Delete(scope, primaryKey(ai.GitHubModelsProviderID)); err != nil {
		return err
	}
	// Drop the cached listing so the revoked credential's models disappear immediately.
	s.invalidateModelsEverywhere(scope, ai.GitHubModelsProviderID)
	return nil
}

func (s *ProviderService) GetGitHubUser(ctx context.Context, scope string) (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetGitHubUser", &err)

	token, err := s.secrets.Get(scope, primaryKey(ai.GitHubModelsProviderID))
	if err != nil {
		// No stored token (or no secret storage) → not connected, not an error.
		return nil, nil
	}
	return s.auth.GetUser(ctx, token)
}

// --- GitHub Copilot OAuth ---

func (s *ProviderService) StartCopilotAuth(ctx context.Context) (resp *ai.DeviceAuthResponse, err error) {
	defer logger.Guard("App.StartCopilotAuth", &err)
	return s.copilotAuth.StartDeviceFlow(ctx)
}

func (s *ProviderService) PollCopilotAuth(ctx context.Context, scope string, deviceCode string) (result *ai.GitHubAuthResult, err error) {
	defer logger.Guard("App.PollCopilotAuth", &err)

	result, err = s.copilotAuth.PollToken(ctx, deviceCode)
	if err != nil {
		return nil, err
	}

	if result.Status == "success" && result.Token != "" {
		if saveErr := s.secrets.Save(scope, oauthTokenKey(ai.CopilotProviderID), result.Token); saveErr != nil {
			logger.Error("failed to save copilot oauth token", "error", saveErr)
		} else {
			// A replacement token may carry other entitlements: drop the previous token's cached listing (mirrors Revoke).
			s.invalidateModelsEverywhere(scope, ai.CopilotProviderID)
		}
		result.Token = ""
		// Pre-warm the Copilot session-token cache so the first chat turn doesn't pay the exchange RTT.
		s.preWarmCopilot(scope)
	}

	return result, nil
}

// preWarmCopilot triggers a background session-token exchange so the cache is primed before the first chat turn.
func (s *ProviderService) preWarmCopilot(scope string) {
	if s.copilotAuth == nil {
		return
	}
	go func() {
		// A panic in this detached goroutine would crash the process; recover and let the first Stream exchange lazily.
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("copilot session-token pre-warm panicked", "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// OAuth device-flow token (per-user) first, then manual PAT.
		token, err := s.secrets.Get(scope, oauthTokenKey(ai.CopilotProviderID))
		if err != nil || token == "" {
			token, err = s.secrets.Get(scope, primaryKey(ai.CopilotProviderID))
			if err != nil || token == "" {
				return // not configured — nothing to warm
			}
		}
		if _, err := s.copilotAuth.GetSessionToken(ctx, token); err != nil {
			logger.Warn("copilot session-token pre-warm failed", "error", err)
		}
	}()
}

func (s *ProviderService) RevokeCopilotAuth(scope string) (err error) {
	defer logger.Guard("App.RevokeCopilotAuth", &err)
	if err := s.secrets.Delete(scope, oauthTokenKey(ai.CopilotProviderID)); err != nil {
		return err
	}
	// Drop the cached listing so the revoked credential's models disappear immediately.
	s.invalidateModelsEverywhere(scope, ai.CopilotProviderID)
	return nil
}

func (s *ProviderService) GetCopilotUser(ctx context.Context, scope string) (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetCopilotUser", &err)

	token, err := s.secrets.Get(scope, oauthTokenKey(ai.CopilotProviderID))
	if err != nil {
		// No stored token (or no secret storage) → not connected, not an error.
		return nil, nil
	}
	return s.auth.GetUser(ctx, token)
}

// --- Local models configuration ---

// GetLocalConfig returns the caller's stored local models configuration; absent or unreadable is unconfigured.
func (s *ProviderService) GetLocalConfig(scope string) (cfg ai.LocalPublicConfig, err error) {
	defer logger.Guard("App.GetLocalConfig", &err)
	if err := ai.CheckLocalModelsAvailable(); err != nil {
		return ai.LocalPublicConfig{}, err
	}
	raw, getErr := s.secrets.Get(scope, ai.LocalProviderID)
	if getErr != nil || raw == "" {
		return ai.LocalPublicConfig{}, nil
	}
	parsed, parseErr := ai.ParseLocalConfig(raw)
	if parseErr != nil {
		logger.Warn("local models: stored configuration unreadable", "error", parseErr)
		return ai.LocalPublicConfig{}, nil
	}
	return parsed.Public(), nil
}

// SaveLocalConfig validates the base URL and context window (0 = default, else 1024..2000000), stores and drops the cached listing.
func (s *ProviderService) SaveLocalConfig(scope, baseURL string, contextWindow int) (cfg ai.LocalConfig, err error) {
	defer logger.Guard("App.SaveLocalConfig", &err)
	cfg, err = ai.NewLocalConfigForSave(baseURL, contextWindow)
	if err != nil {
		return ai.LocalConfig{}, err
	}
	raw, err := cfg.Encode()
	if err != nil {
		return ai.LocalConfig{}, err
	}
	if err := s.secrets.Save(scope, ai.LocalProviderID, raw); err != nil {
		return ai.LocalConfig{}, err
	}
	s.invalidateModelsEverywhere(scope, ai.LocalProviderID)
	return cfg, nil
}

// --- Azure AI Foundry configuration ---

// GetAzureFoundryConfig returns the caller's stored document, secrets removed; missing/unreadable = unconfigured.
func (s *ProviderService) GetAzureFoundryConfig(scope string) (cfg ai.AzureFoundryPublicConfig, err error) {
	defer logger.Guard("App.GetAzureFoundryConfig", &err)
	if s.azure != nil {
		return s.azure.legacyGetConfig(context.Background(), scope)
	}
	raw, getErr := s.secrets.Get(scope, ai.AzureFoundryProviderID)
	if getErr != nil || raw == "" {
		return ai.AzureFoundryPublicConfig{}, nil
	}
	parsed, parseErr := ai.ParseAzureFoundryConfig(raw)
	if parseErr != nil {
		logger.Warn("azure ai foundry: stored configuration unreadable", "error", parseErr)
		return ai.AzureFoundryPublicConfig{}, nil
	}
	return parsed.Public(), nil
}

// SaveAzureFoundryConfig validates and stores the document; an omitted secret is carried per carryAzureSecret.
func (s *ProviderService) SaveAzureFoundryConfig(scope string, cfg ai.AzureFoundryConfig) (err error) {
	defer logger.Guard("App.SaveAzureFoundryConfig", &err)
	if s.azure != nil {
		if err := s.azure.legacySaveConfig(context.Background(), scope, cfg); err != nil {
			return err
		}
		s.invalidateModelsEverywhere(scope, ai.AzureFoundryProviderID)
		return nil
	}
	cfg.Normalize()
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.secrets.Get(scope, ai.AzureFoundryProviderID) })
	if refusal := carryAzureSecret(&cfg, prev, hadPrev); refusal != nil {
		return saveCarryError(cfg, refusal)
	}
	if err := ai.ValidateAzureFoundryConfig(cfg); err != nil {
		return err
	}
	raw, err := cfg.Encode()
	if err != nil {
		return err
	}
	if err := s.secrets.Save(scope, ai.AzureFoundryProviderID, raw); err != nil {
		return err
	}
	s.invalidateModelsEverywhere(scope, ai.AzureFoundryProviderID)
	ai.InvalidateAzureFoundryModels(cfg)
	if hadPrev && prev.Fingerprint() != cfg.Fingerprint() {
		ai.ForgetAzureFoundryCredential(prev)
	}
	return nil
}

// --- Organisation-shared Azure AI Foundry configuration ---
// Trust model: the shared document is only what an admin lends — members read, admins write, admins+members run on it; a demoted lender disables it.

// OrgAzureFoundryConfig is the org's shared document in public form with who shared it and when; UpdatedAt is the CAS token.
type OrgAzureFoundryConfig struct {
	ai.AzureFoundryPublicConfig
	Shared        bool   `json:"shared"`
	UpdatedBy     string `json:"updatedBy,omitempty"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
	UpdatedByID   string `json:"-"`
	SecretSetByID string `json:"-"`
}

// GetOrgAzureFoundryConfig returns the org's shared document, secrets removed; unreadable-but-present is ErrOrgCredentialUnreadable.
func (s *ProviderService) GetOrgAzureFoundryConfig(orgID string) (cfg OrgAzureFoundryConfig, err error) {
	defer logger.Guard("App.GetOrgAzureFoundryConfig", &err)
	if s.orgKeys == nil {
		return OrgAzureFoundryConfig{}, ErrOrgKeyStoreUnavailable
	}
	if s.azure != nil && s.azure.OrgRegistriesAvailable() {
		return s.azure.legacyOrgGetConfig(context.Background(), orgID)
	}
	raw, getErr := s.orgKeys.GetOrg(orgID, ai.AzureFoundryProviderID)
	if errors.Is(getErr, storage.ErrSecretNotFound) || (getErr == nil && raw == "") {
		return OrgAzureFoundryConfig{}, nil
	}
	if getErr != nil {
		return OrgAzureFoundryConfig{}, fmt.Errorf("%w: %w", ErrOrgCredentialUnreadable, getErr)
	}
	parsed, parseErr := ai.ParseAzureFoundryConfig(raw)
	if parseErr != nil {
		return OrgAzureFoundryConfig{}, fmt.Errorf("%w: %w", ErrOrgCredentialUnreadable, parseErr)
	}
	meta, hasMeta, metaErr := s.orgKeys.OrgKeyMeta(orgID, ai.AzureFoundryProviderID)
	if metaErr != nil {
		return OrgAzureFoundryConfig{}, fmt.Errorf("%w: %w", ErrOrgCredentialUnreadable, metaErr)
	}
	out := OrgAzureFoundryConfig{AzureFoundryPublicConfig: parsed.Public(), Shared: true}
	if hasMeta {
		out.UpdatedByID = meta.UpdatedBy
		out.SecretSetByID = meta.SecretSetBy
		if out.SecretSetByID == "" {
			out.SecretSetByID = meta.UpdatedBy // a row no save has stamped since v24
		}
		if !meta.UpdatedAt.IsZero() {
			out.UpdatedAt = formatOrgKeyStamp(meta.UpdatedAt)
		}
	}
	return out, nil
}

// SaveOrgAzureFoundryConfig is SaveAzureFoundryConfig for the org's shared document: same carry rules; CAS on expectedUpdatedAt (else 409).
func (s *ProviderService) SaveOrgAzureFoundryConfig(orgID, updatedBy, expectedUpdatedAt string, cfg ai.AzureFoundryConfig) (updatedAt string, err error) {
	defer logger.Guard("App.SaveOrgAzureFoundryConfig", &err)
	if s.orgKeys == nil {
		return "", ErrOrgKeyStoreUnavailable
	}
	expected, err := parseOrgKeyStamp(expectedUpdatedAt)
	if err != nil {
		return "", err
	}
	cfg.Normalize()
	// The Azure CLI sign-in is a personal, desktop-only credential (whoever is signed into `az`); an org document must be an API key or service principal.
	if cfg.Auth.Mode == ai.AzureAuthModeEntraCLI {
		return "", &ai.AzureConfigError{Code: ai.AzureCfgErrInvalidAuth,
			Message: "the Azure CLI sign-in mode cannot be shared with an organisation — save it as your own Azure AI Foundry configuration, or share an API key or Microsoft Entra ID service principal"}
	}
	if s.azure != nil && s.azure.OrgRegistriesAvailable() {
		// Deprecated with the registry: only the credential part is saved (same CAS).
		at, err := s.azure.legacyOrgSaveConfig(context.Background(), orgID, updatedBy, expected, cfg)
		if err != nil {
			return "", err
		}
		s.invalidateScopeEverywhere(ai.OrgScope(orgID))
		return at, nil
	}
	// A token-less save with a fresh secret may repair an unreadable row: the row's OrgKeyMeta stamp becomes the token.
	if expected.IsZero() && cfg.HasSecret() {
		stamp, repair, rerr := unreadableOrgStamp(s.orgKeys, orgID, ai.AzureFoundryProviderID)
		if rerr != nil {
			return "", rerr
		}
		if repair {
			expected = stamp
		}
	}
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.orgKeys.GetOrg(orgID, ai.AzureFoundryProviderID) })
	supplied := cfg.HasSecret()
	if refusal := carryAzureSecret(&cfg, prev, hadPrev); refusal != nil {
		return "", saveCarryError(cfg, refusal)
	}
	if err := ai.ValidateAzureFoundryConfig(cfg); err != nil {
		return "", err
	}
	raw, err := cfg.Encode()
	if err != nil {
		return "", err
	}
	// Validation passed, so the document holds a secret: an unsupplied one was carried, and its lender stays.
	secretCarried := !supplied
	at, ok, err := s.orgKeys.SaveOrgIfUnchanged(orgID, ai.AzureFoundryProviderID, raw, updatedBy, secretCarried, expected)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrOrgCredentialConflict
	}
	s.invalidateScopeEverywhere(ai.OrgScope(orgID))
	ai.InvalidateAzureFoundryModels(cfg)
	if hadPrev && prev.Fingerprint() != cfg.Fingerprint() {
		ai.ForgetAzureFoundryCredential(prev)
	}
	return formatOrgKeyStamp(at), nil
}

// DeleteOrgAzureFoundryConfig removes the org's shared document, releases process state and drops cached listings; missing = no-op.
func (s *ProviderService) DeleteOrgAzureFoundryConfig(orgID string) (err error) {
	defer logger.Guard("App.DeleteOrgAzureFoundryConfig", &err)
	if s.orgKeys == nil {
		return ErrOrgKeyStoreUnavailable
	}
	if s.azure != nil && s.azure.OrgRegistriesAvailable() {
		if err := s.azure.legacyDeleteDefault(context.Background(), storageif.AzureOrgOwner(orgID), ""); err != nil {
			return err
		}
		s.invalidateScopeEverywhere(ai.OrgScope(orgID))
		return nil
	}
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.orgKeys.GetOrg(orgID, ai.AzureFoundryProviderID) })
	if err := s.orgKeys.DeleteOrg(orgID, ai.AzureFoundryProviderID); err != nil {
		return err
	}
	if hadPrev {
		ai.ForgetAzureFoundryCredential(prev)
	}
	s.invalidateScopeEverywhere(ai.OrgScope(orgID))
	return nil
}

// DisableOrgAzureFoundryConfigSavedBy disables the org's credentials userID lent (secret_set_by); a newer admin's save is kept.
func (s *ProviderService) DisableOrgAzureFoundryConfigSavedBy(orgID, userID string) (disabled bool, err error) {
	defer logger.Guard("App.DisableOrgAzureFoundryConfigSavedBy", &err)
	keys, err := s.DisableOrgAzureCredentialsSavedBy(orgID, userID)
	return len(keys) > 0, err
}

// DisableOrgCredentialsSavedBy is the family-wide version for every org userID lent credentials to (account erasure).
func (s *ProviderService) DisableOrgCredentialsSavedBy(userID string) (orgIDs []string, err error) {
	defer logger.Guard("App.DisableOrgCredentialsSavedBy", &err)
	refs, err := s.DisableOrgAzureCredentialsLentBy(userID)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		if len(orgIDs) == 0 || orgIDs[len(orgIDs)-1] != r.OrgID {
			orgIDs = append(orgIDs, r.OrgID)
		}
	}
	return orgIDs, nil
}

// PrepareOrgDeletion reads what to release when orgID is deleted and returns the post-delete release.
func (s *ProviderService) PrepareOrgDeletion(orgID string) (release func()) {
	var docs []string
	if s.orgKeys != nil && orgID != "" {
		docs = orgAzureDocuments(s.orgKeys, orgID)
	}
	return func() {
		for _, raw := range docs {
			forgetAzureDocument(raw)
		}
		s.InvalidateScope(ai.OrgScope(orgID))
		if s.azure != nil && orgID != "" {
			s.azure.forgetOwner(storageif.AzureOrgOwner(orgID))
		}
	}
}

// PrepareAccountErasure is PrepareOrgDeletion for a user's own credentials, read before the erasure cascades them away.
func (s *ProviderService) PrepareAccountErasure(scope string) (release func()) {
	var docs []string
	if s.secrets != nil {
		var ids []string
		var families storageif.SecretFamilyLister
		if s.azure != nil {
			ids = s.azure.registryConnectionIDs(context.Background(), s.azure.userOwner(scope))
			families = s.azure.keyFamilies
		}
		docs = userAzureDocuments(s.secrets, families, scope, ids)
	}
	return func() {
		for _, raw := range docs {
			forgetAzureDocument(raw)
		}
		s.InvalidateScope(scope)
		if s.azure != nil {
			s.azure.forgetOwner(s.azure.userOwner(scope))
		}
	}
}
