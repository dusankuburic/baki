package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// dynamicModelTTL bounds how long a CONFIGURED provider's live model list
// (fetched from the provider's API) is reused. Provider catalogs change
// rarely; without a cache, opening the settings panel twice fanned out an
// upstream HTTP call per provider per request, and a slow upstream stalled
// the response for up to the request timeout.
const dynamicModelTTL = 10 * time.Minute

// emptyListingTTL caches a configured provider's EMPTY live listing briefly:
// long enough that opening Settings twice does not fan out a second upstream
// call for a provider with no entitlements, short enough that a listing that
// fills in (a deployment created, a role granted) shows up soon.
const emptyListingTTL = 30 * time.Second

// invalidation is a per-key or per-scope invalidation mark: the generation
// it was made at and when, so stale marks can be pruned.
type invalidation struct {
	gen uint64
	at  time.Time
}

// invalidationFence keeps a mark long past any in-flight listing
// (liveModelsBudget + grace, or a caller's own deadline).
const invalidationFence = dynamicModelTTL

// liveModelsBudget bounds each configured provider's live listing in
// ListProviders. The listings run concurrently, so the whole response stays
// well under the client's 30 s request timeout even when one upstream hangs;
// a provider that misses the budget is listed from its metadata instead. A
// variable so tests can shorten it.
var liveModelsBudget = 8 * time.Second

// liveModelsGrace is how long ListProviders waits past liveModelsBudget for a
// listing that ignores its context before falling back to the metadata.
const liveModelsGrace = 500 * time.Millisecond

// ProviderService manages AI provider configuration, authentication, and connectivity.
type ProviderService struct {
	auth        *ai.GitHubAuth
	copilotAuth *ai.CopilotAuth
	factory     *ai.ProviderFactory
	secrets     KeyStore
	// orgKeys holds organisations' shared credentials (G7); nil on the
	// desktop and in local mode, where the org routes report it missing.
	orgKeys OrgKeyStore
	// azure is the Azure AI Foundry model registry (Part 9). With it, every
	// call site that knew only the single "azure-foundry" document acts on
	// the registry — its resource "default", or every resource; nil (a
	// service built without it, in tests) keeps the single-document
	// behaviour.
	azure *AzureModelService

	modelMu    sync.Mutex
	modelCache map[string]modelCacheEntry // key: scope + "\x00" + providerID
	// modelGen is a monotonic stamp bumped by every invalidation (under
	// modelMu). A listing records it when it starts and writes back only if
	// neither its key nor its scope was invalidated after that stamp — the
	// pre-save listing must not stay cached for dynamicModelTTL, but one
	// user's Azure save must not discard another user's in-flight listing.
	modelGen         uint64
	invalidatedKeys  map[string]invalidation
	invalidatedScope map[string]invalidation
}

type modelCacheEntry struct {
	models []ai.ModelInfo
	// defaultModel is the CONFIGURED provider's default, which can differ
	// from the metadata provider's.
	defaultModel string
	expiresAt    time.Time
}

func NewProviderService(auth *ai.GitHubAuth, copilotAuth *ai.CopilotAuth, factory *ai.ProviderFactory, secrets KeyStore) *ProviderService {
	return &ProviderService{
		auth:        auth,
		copilotAuth: copilotAuth,
		factory:     factory,
		secrets:     secrets,
		modelCache:  make(map[string]modelCacheEntry),

		invalidatedKeys:  make(map[string]invalidation),
		invalidatedScope: make(map[string]invalidation),
	}
}

// SetOrgKeyStore enables organisation-shared credentials (G7). Set once at
// boot, before requests are served; nil leaves the org methods reporting
// ErrOrgKeyStoreUnavailable and ListProviders ignoring the org.
func (s *ProviderService) SetOrgKeyStore(store OrgKeyStore) { s.orgKeys = store }

// SetAzureModels wires the Azure AI Foundry model registry (Part 9). Set
// once at boot, before requests are served.
func (s *ProviderService) SetAzureModels(azure *AzureModelService) { s.azure = azure }

// AzureModels is the wired Azure AI Foundry model registry (nil without
// one).
func (s *ProviderService) AzureModels() *AzureModelService { return s.azure }

// OrgKeysAvailable reports whether this deployment can hold organisation
// credentials (cloud mode with the database keystore).
func (s *ProviderService) OrgKeysAvailable() bool { return s.orgKeys != nil }

// ErrOrgKeyStoreUnavailable is returned by the organisation credential
// methods on a deployment without an org secret store (desktop, local mode).
var ErrOrgKeyStoreUnavailable = errors.New("organisation credentials require the database keystore (cloud mode)")

// ErrOrgCredentialUnreadable: the organisation's shared document exists but
// could not be read — a decrypt failure after a deployment key rotation, a
// database error, a document this build cannot parse. Reported instead of
// "not configured", which would have told an admin the org has none while
// its members keep failing on it. The HTTP layer maps it to 500
// ORG_CREDENTIAL_UNREADABLE.
var ErrOrgCredentialUnreadable = errors.New("the organisation's shared Azure AI Foundry configuration could not be read")

// ErrOrgCredentialConflict: an org save carried an updatedAt that no longer
// matches the stored document (another admin saved or removed it since the
// caller read it), or none while a document exists. Nothing was written; the
// HTTP layer maps it to 409 CONFLICT.
var ErrOrgCredentialConflict = errors.New("the organisation's shared configuration changed since it was loaded — reload it and apply your edit again")

// OrgRoleUsesSharedCredential reports whether an organisation role may run
// on the org's shared credential (G7): admins and members. Viewers and
// guests — and anyone outside the org, including a per-flow collaborator on
// one of its flows — need a credential of their own.
func OrgRoleUsesSharedCredential(role auth.Role) bool {
	return role == auth.RoleAdmin || role == auth.RoleMember
}

// listingScope names whose credential a provider listing runs on: the
// caller's own (cacheScope = scope) or the organisation's shared one
// (cacheScope = ai.OrgScope(orgID), shared = true), so the cached live
// listing is keyed — and invalidated — by the credential it was built from.
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

// dynamicModels returns the configured provider's live model list, cached for
// dynamicModelTTL per (scope, provider). Static metadata providers are cheap
// (no network) and are NOT cached. An empty listing is cached only for
// emptyListingTTL (the next call soon retries instead of pinning "no
// models"). Nothing is cached when the caller's budget expired while it ran
// (it may be cut short), or when an InvalidateModels / InvalidateScope for
// this key ran meanwhile (it describes the previous configuration).
//
// Alias providers (Azure AI Foundry) are never cached here: their listing is
// built from the user's own document plus, with Entra ID and a project
// endpoint, the project listing, which the provider caches itself per
// credential set and deployment names — and only when complete. A second,
// blind cache here would pin a degraded listing (agents missing after a
// transient project failure) for dynamicModelTTL.
func (s *ProviderService) dynamicModels(ctx context.Context, ls listingScope, providerID string) (models []ai.ModelInfo, defaultModel string, ok bool) {
	scope := ls.cacheScope
	key := scope + "\x00" + providerID
	cacheable := !ai.UsesModelAliases(providerID)
	s.modelMu.Lock()
	if e, hit := s.modelCache[key]; cacheable && hit && time.Now().Before(e.expiresAt) {
		s.modelMu.Unlock()
		return e.models, e.defaultModel, true
	}
	gen := s.modelGen
	s.modelMu.Unlock()

	// Member-first: with no org (personalScope) this is the caller's own
	// key; with one it is the org's credential exactly when the caller has
	// none — the same rule that decided ls.shared.
	realProvider, err := s.factory.ForOrg(ls.scope, ls.orgID, providerID)
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
			// Insert-time pruning: expired entries are never removed by reads,
			// so this is the only thing that keeps a departed user's keys from
			// accumulating in the map forever.
			s.pruneModelCacheLocked(time.Now())
			s.modelCache[key] = modelCacheEntry{models: models, defaultModel: defaultModel, expiresAt: time.Now().Add(ttl)}
		}
		s.modelMu.Unlock()
	}
	return models, defaultModel, true
}

// invalidatedSinceLocked reports whether key or its scope was invalidated
// after the generation stamp gen (taken when the listing started). Caller
// holds modelMu.
func (s *ProviderService) invalidatedSinceLocked(key, scope string, gen uint64) bool {
	if inv, ok := s.invalidatedKeys[key]; ok && inv.gen > gen {
		return true
	}
	inv, ok := s.invalidatedScope[scope]
	return ok && inv.gen > gen
}

// modelCacheMax bounds the live-listing cache's size, mirroring the ai
// package's azureModelsCacheMax for its Azure model listings.
const modelCacheMax = 512

// pruneModelCacheLocked drops expired entries first — they are invisible to
// reads already, and nothing else ever removes them, so without this a
// departed or deleted user's entries accumulated forever — then, while the
// map is at modelCacheMax, the entry closest to expiry (the least valuable:
// the next read would drop it anyway). Mirrors pruneAzureModelsCacheLocked in
// internal/ai. Bounded work: one pass over the map per insert, plus one pass
// per eviction.
func (s *ProviderService) pruneModelCacheLocked(now time.Time) {
	for k, e := range s.modelCache {
		if !now.Before(e.expiresAt) {
			delete(s.modelCache, k)
		}
	}
	// Invalidation marks only fence listings that were in flight when they
	// were made; past invalidationFence none can be, so the mark is dead.
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
	for len(s.modelCache) >= modelCacheMax {
		oldest, oldestExp, first := "", time.Time{}, true
		for k, e := range s.modelCache {
			if first || e.expiresAt.Before(oldestExp) {
				oldest, oldestExp, first = k, e.expiresAt, false
			}
		}
		delete(s.modelCache, oldest)
	}
}

// InvalidateScope drops every cached live listing belonging to one scope
// (user): account deletion cascades the user's provider keys in SQL, and this
// is what tells the in-process cache. Also bumps the generation so a listing
// already in flight for that user does not write itself back (same fence as
// InvalidateModels).
func (s *ProviderService) InvalidateScope(scope string) {
	s.modelMu.Lock()
	s.modelGen++
	s.invalidatedScope[scope] = invalidation{gen: s.modelGen, at: time.Now()}
	prefix := scope + "\x00"
	for k := range s.modelCache {
		if strings.HasPrefix(k, prefix) {
			delete(s.modelCache, k)
		}
	}
	s.modelMu.Unlock()
}

// InvalidateModels drops the cached live listing for one (scope, provider) so
// a configuration change (new Azure deployment names, rotated credential,
// removed key) shows up on the next listing instead of after
// dynamicModelTTL, and bumps the generation so a listing already in flight
// for the old configuration does not write itself back.
func (s *ProviderService) InvalidateModels(scope, providerID string) {
	key := scope + "\x00" + providerID
	s.modelMu.Lock()
	s.modelGen++
	s.invalidatedKeys[key] = invalidation{gen: s.modelGen, at: time.Now()}
	delete(s.modelCache, key)
	s.modelMu.Unlock()
}

// chatModels drops the entries that are not chat models (embedding
// deployments, flagged by the provider): they stay in Models() for pricing
// but are never offered in the chat picker or chosen as its default.
func chatModels(in []ai.ModelInfo) []ai.ModelInfo {
	out := make([]ai.ModelInfo, 0, len(in))
	for _, m := range in {
		if !m.Embedding {
			out = append(out, m)
		}
	}
	return out
}

// chooseDefaultModel picks the model the UI preselects for a configured
// provider: the provider's own default when the live listing has it, else the
// metadata fallback when it has that, else the first chat-capable entry
// (skipping agents, embedding deployments and non-chat model families), else
// the first entry that is not an embedding deployment. A user-named Azure AI
// Foundry resource rarely has a deployment called "gpt-4.1".
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

// nonChatModelFamilies are id fragments of models the chat-completions route
// cannot serve — image, speech, transcription, realtime-audio, video and
// embedding models — which a deployment listing may contain.
var nonChatModelFamilies = []string{"dall-e", "gpt-image", "whisper", "tts", "transcribe", "realtime", "sora", "embed"}

// isNonChatModelID is the heuristic that keeps a non-chat deployment from
// being chosen as the chat default or connection-test model.
func isNonChatModelID(id string) bool {
	lower := strings.ToLower(id)
	for _, f := range nonChatModelFamilies {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return false
}

// connectionTestModels lists the models the connection test tries, in order.
// Alias providers (Azure AI Foundry): the configured chat deployment first —
// tried even when the live listing lacks it — then the free model, then the
// first chat-capable listed model, so a resource whose deployments carry
// custom names still passes. Every other provider: when the live listing
// (5 s budget) has the catalogue default, the default then the free model;
// when it lacks it, the first chat-capable listed model goes FIRST — a
// catalogue default the listing lacks is a guaranteed miss, so the test
// costs one call and, when both fail, the report is about a model the token
// actually has. providerID is the registry id (the wrapped chain's ID() is
// the raw provider's).
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

// azureProjectProbe explains an empty Azure AI Foundry candidate list (an
// Entra ID + project document that names no deployment): the token
// refusal or the project listing's failure. A package variable so tests
// replace the network call.
var azureProjectProbe = ai.ProbeAzureFoundryProject

// liveListing is one configured provider's live listing (dynamicModels).
type liveListing struct {
	models       []ai.ModelInfo
	defaultModel string
	ok           bool
}

// startLiveListing runs dynamicModels for one provider in the background
// under its own liveModelsBudget (still bounded by the request context).
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

// awaitLiveListing returns the listing once it arrives, or a failed one when
// wait expires first (a listing that ignores its context).
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

// isConfigured reports whether the caller has credentials for a provider.
// OAuth device-flow tokens and manual API keys are both resolved under the
// caller's scope (per-user in cloud mode). Azure AI Foundry with the model
// registry: at least one resource of the caller's own.
func (s *ProviderService) isConfigured(scope, providerID string) bool {
	switch providerID {
	case ai.AzureFoundryProviderID:
		if s.azure != nil {
			ok, err := s.azure.HasPersonalResources(context.Background(), scope)
			if err != nil {
				logger.Warn("azure ai foundry: reading the caller's resources failed", "error", err)
			}
			return ok
		}
		ok, _ := s.secrets.Has(scope, providerID)
		return ok
	case "github-models":
		ok, _ := s.secrets.Has(scope, "github-models-token")
		return ok
	case "copilot":
		oauthOk, _ := s.secrets.Has(scope, "copilot-oauth-token")
		patOk, _ := s.secrets.Has(scope, "copilot")
		return oauthOk || patOk
	default:
		ok, _ := s.secrets.Has(scope, providerID)
		return ok
	}
}

// isOrgConfigured reports whether the organisation holds a shared credential
// for a provider. Copilot is per member only (OAuth or PAT), never shared.
// Azure AI Foundry with the model registry: the organisation holds at least
// one resource.
func (s *ProviderService) isOrgConfigured(orgID, providerID string) bool {
	if s.orgKeys == nil || orgID == "" {
		return false
	}
	switch providerID {
	case "copilot":
		return false
	case ai.AzureFoundryProviderID:
		if s.azure != nil && s.azure.OrgRegistriesAvailable() {
			ok, err := s.azure.OrgHasResources(context.Background(), orgID)
			if err != nil {
				logger.Warn("azure ai foundry: reading the organisation's resources failed", "org", orgID, "error", err)
			}
			return ok
		}
		ok, _ := s.orgKeys.HasOrg(orgID, providerID)
		return ok
	case "github-models":
		ok, _ := s.orgKeys.HasOrg(orgID, "github-models-token")
		return ok
	default:
		ok, _ := s.orgKeys.HasOrg(orgID, providerID)
		return ok
	}
}

// resolveListing decides whose credential a provider is listed on (G7,
// member-first): the caller's own when they have one, else the org's shared
// one when there is an org and it has one. configured is false when neither.
func (s *ProviderService) resolveListing(scope, orgID, providerID string) (ls listingScope, configured bool) {
	if s.isConfigured(scope, providerID) {
		return personalScope(scope), true
	}
	if s.isOrgConfigured(orgID, providerID) {
		return listingScope{scope: scope, orgID: orgID, cacheScope: ai.OrgScope(orgID), shared: true}, true
	}
	return personalScope(scope), false
}

// ListProviders lists every provider with its models and default. The
// configured providers' live listings run concurrently, each under
// liveModelsBudget; one that fails or misses the budget falls back to the
// metadata catalogue, and the order stays AvailableProviders'.
//
// orgID ("" = none) adds the organisation's shared credentials (G7): a
// provider the caller has not configured but the org has is listed as
// configured and Shared, its live listing built from — and cached under —
// the org's credential. The caller must already be verified an admin or
// member of orgID (OrgRoleUsesSharedCredential — the handler does); this
// method does not check.
//
// Azure AI Foundry with the model registry is listed from the registry
// (azureProviderInfo), never from a live listing: its models are the
// caller's registered entries and orgID's shared ones, and nothing about it
// waits on the network. A registry that cannot be read fails the listing —
// an Azure entry without its models would read as "every model was
// removed" to a client, which then re-pins its threads.
func (s *ProviderService) ListProviders(ctx context.Context, scope, orgID string) (providers []models.ProviderInfo, err error) {
	defer logger.Guard("App.ListProviders", &err)

	metas := ai.AvailableProviders()
	metaProviders := make([]ai.MetadataProvider, len(metas))
	configured := make([]bool, len(metas))
	shared := make([]bool, len(metas))
	live := make([]<-chan liveListing, len(metas))
	for i, meta := range metas {
		// The local models provider is listed only when this process may use
		// it (desktop) and the caller configured it: an unconfigured card
		// would offer a server nobody set up.
		if meta.ID == ai.LocalProviderID && (!ai.LocalModelsAvailable() || !s.isConfigured(scope, meta.ID)) {
			continue
		}
		metaProviders[i] = ai.GetMetadataProvider(meta.ID)
		if metaProviders[i] == nil {
			continue
		}
		if s.fromAzureRegistry(meta.ID) {
			continue // listed from the registry below: no live listing
		}
		var ls listingScope
		ls, configured[i] = s.resolveListing(scope, orgID, meta.ID)
		shared[i] = ls.shared
		if configured[i] {
			live[i] = s.startLiveListing(ctx, ls, meta.ID)
		}
	}
	wait, cancel := context.WithTimeout(ctx, liveModelsBudget+liveModelsGrace)
	defer cancel()

	for i, meta := range metas {
		p := metaProviders[i]
		if p == nil {
			continue
		}
		if s.fromAzureRegistry(meta.ID) {
			info, err := s.azureProviderInfo(ctx, scope, orgID, meta, p)
			if err != nil {
				return nil, err
			}
			providers = append(providers, info)
			continue
		}

		modelInfos, mErr := p.Models(ctx)
		if mErr != nil {
			logger.Warn("list provider models", "provider", meta.ID, "error", mErr)
		}
		defaultModel := p.DefaultModel()
		if live[i] != nil {
			if l := awaitLiveListing(wait, live[i]); l.ok {
				modelInfos = chatModels(l.models)
				switch {
				case ai.UsesModelAliases(meta.ID) && l.defaultModel != "":
					// The user's own configuration names the default (Azure
					// AI Foundry: the chat deployment). It wins even when the
					// listing lacks it — a project listing may omit it and
					// the name is what the user deployed.
					defaultModel = l.defaultModel
				case ai.UsesModelAliases(meta.ID):
					// Nothing named (Entra ID + project endpoint): the first
					// chat-capable listed deployment, never the catalogue's
					// reference name.
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
		// The local provider's window is the caller's configured one, which
		// the key-less metadata provider cannot know; the live listing
		// carries it on every entry.
		if meta.ID == ai.LocalProviderID && len(modelInfos) > 0 && modelInfos[0].ContextLimit > 0 {
			contextLimit = modelInfos[0].ContextLimit
		}

		providers = append(providers, models.ProviderInfo{
			ID:           meta.ID,
			Name:         meta.Name,
			Configured:   configured[i],
			Shared:       shared[i],
			Models:       modelDetails,
			DefaultModel: defaultModel,
			ContextLimit: contextLimit,
			AuthType:     meta.AuthType,
		})
	}

	return providers, nil
}

// fromAzureRegistry reports whether providerID's listing entry comes from
// the Azure AI Foundry model registry (azureProviderInfo) rather than a live
// listing: Azure AI Foundry, with the registry wired.
func (s *ProviderService) fromAzureRegistry(providerID string) bool {
	return s.azure != nil && providerID == ai.AzureFoundryProviderID
}

// azureProviderInfo is the Azure AI Foundry listing entry built from the
// model registry (AzureModelService.PickerListing, network-free): the
// caller's chat, Claude and agent entries and orgID's shared ones — "m/<id>"
// ids with purpose, status, window and aliases, in display order — the
// server default ("" when no entry can run), and whether to suggest
// discovering the models of the caller's migrated project. Configured and
// Shared keep the listing contract of every other provider (the member-first
// rule of resolveListing): configured when the caller has a resource of
// their own OR runs on the organisation's shared ones, shared in the second
// case only. The name, auth type and provider-wide window stay the
// catalogue's.
func (s *ProviderService) azureProviderInfo(ctx context.Context, scope, orgID string, meta ai.ProviderMetadata, p ai.MetadataProvider) (models.ProviderInfo, error) {
	listing, err := s.azure.PickerListing(ctx, scope, orgID)
	if err != nil {
		return models.ProviderInfo{}, fmt.Errorf("list the Azure AI Foundry models: %w", err)
	}
	return models.ProviderInfo{
		ID:               meta.ID,
		Name:             meta.Name,
		Configured:       listing.Configured || listing.Shared,
		Shared:           listing.Shared,
		Models:           listing.Models,
		DefaultModel:     listing.DefaultModel,
		ContextLimit:     p.ContextLimit(),
		AuthType:         meta.AuthType,
		SuggestDiscovery: listing.SuggestDiscovery,
	}, nil
}

// TestProviderConnection sends a tiny chat request to the candidates of
// connectionTestModels in order. Credentials are judged by the first answer
// that says something about them: a success, an invalid key or a rate limit
// (the key works). A model-level rejection — 404 DeploymentNotFound, 400
// OperationNotSupported, insufficient balance on one model — says nothing
// about the credentials, so the next candidate is tried while the budget
// lasts; when every candidate fails, the reported error is the first
// candidate's (the configured default's), which names what the user set up.
// An outage (the upstream unreachable or failing after the retry wrapper's
// attempts, an open circuit, an invalid stored document) is not model-level
// either way: the test stops there instead of repeating the same retries
// against the same outage for every candidate.
func (s *ProviderService) TestProviderConnection(ctx context.Context, scope, providerID string) (result *models.ProviderTestResult, err error) {
	defer logger.Guard("App.TestProviderConnection", &err)

	if providerID == ai.AzureFoundryProviderID && s.azure != nil {
		// Deprecated for Azure AI Foundry (the registry's test route tests
		// one resource or model): resource "default", else the first one,
		// probed as a resource — no model call.
		return s.azure.legacyTest(ctx, AzureScope{Scope: scope})
	}
	provider, err := s.factory.For(scope, providerID)
	if err != nil {
		return &models.ProviderTestResult{Ok: false, Error: connectionTestError(providerID, "", err)}, nil
	}
	return s.runConnectionTest(ctx, provider, providerID, func() string {
		raw, getErr := s.secrets.Get(scope, providerID)
		if getErr != nil {
			return ""
		}
		return raw
	}), nil
}

// TestOrgProviderConnection is TestProviderConnection for an organisation's
// SHARED credential (G7): the provider is built from the org's credential
// only — never the admin's own — so the result says whether what the org's
// members will run on works. scope is the admin's, for usage attribution of
// the test call.
func (s *ProviderService) TestOrgProviderConnection(ctx context.Context, scope, orgID, providerID string) (result *models.ProviderTestResult, err error) {
	defer logger.Guard("App.TestOrgProviderConnection", &err)
	if s.orgKeys == nil {
		return nil, ErrOrgKeyStoreUnavailable
	}
	if providerID == ai.AzureFoundryProviderID && s.azure != nil && s.azure.OrgRegistriesAvailable() {
		// Deprecated: the organisation's resource "default", else its first.
		return s.azure.legacyTest(ctx, AzureScope{Scope: scope, OrgID: orgID})
	}
	provider, err := s.factory.ForOrgCredential(scope, orgID, providerID)
	if err != nil {
		return &models.ProviderTestResult{Ok: false, Error: connectionTestError(providerID, "", err)}, nil
	}
	return s.runConnectionTest(ctx, provider, providerID, func() string {
		raw, getErr := s.orgKeys.GetOrg(orgID, providerID)
		if getErr != nil {
			return ""
		}
		return raw
	}), nil
}

// runConnectionTest is the shared body of the connection tests; rawDoc
// returns the stored Azure AI Foundry document (or "") for the project
// probe that explains an empty candidate list.
func (s *ProviderService) runConnectionTest(ctx context.Context, provider ai.Provider, providerID string, rawDoc func() string) *models.ProviderTestResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	testModels := s.connectionTestModels(ctx, provider, providerID)
	if len(testModels) == 0 {
		msg := "no model to test: add a chat model to the provider under Settings → AI Accounts"
		if providerID == ai.LocalProviderID {
			msg = "no model found: start the local server, load a model and check the base URL"
			if !ai.LocalModelsAvailable() {
				msg = "local models are available in the desktop app only"
			}
		}
		if providerID == ai.AzureFoundryProviderID {
			// An Entra ID + project document may name nothing and rely on the
			// project listing; when that listing is empty, its failure
			// (rejected credentials, a missing role, a wrong project) is the
			// answer, not "name a deployment".
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
		if errors.Is(chatErr, ai.ErrProviderPermissionDenied) || errors.Is(chatErr, ai.ErrEntraCredentialsRejected) || errors.Is(chatErr, ai.ErrEntraTokenRejected) || errors.Is(chatErr, ai.ErrClaudeEntraOnly) {
			// Auth failures (they also match ErrApiKeyInvalid) whose one-line
			// text is what the user needs: the principal and the data action
			// its missing role grants, Entra ID's AADSTS code, Azure's
			// explanation for a rejected Entra ID access token, or the
			// Entra-only refusal a key gets against a Claude Mythos
			// deployment. "invalid API key" named a key an Entra ID
			// principal does not have.
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: false, Latency: latency, Error: connectionTestError(providerID, "", chatErr)}
		}
		if errors.Is(chatErr, ai.ErrApiKeyInvalid) {
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: false, Latency: latency, Error: "invalid API key"}
		}
		if errors.Is(chatErr, ai.ErrRateLimited) || errors.Is(chatErr, ai.ErrOutputLimit) {
			// The credentials work: a rate limit is the resource answering,
			// and an output-limit stop is a reply the model spent entirely on
			// hidden reasoning (always-thinking models under the test's
			// 16-token cap) — the call itself succeeded.
			latency := int(time.Since(start).Milliseconds())
			return &models.ProviderTestResult{Ok: true, Latency: latency}
		}
		if errors.Is(chatErr, ai.ErrProviderDown) || errors.Is(chatErr, ai.ErrCircuitOpen) || errors.Is(chatErr, ai.ErrProviderConfigInvalid) {
			// Nothing model-level: the upstream is unreachable or failing
			// (RetryingProvider already spent its attempts and folded
			// transport errors into ErrProviderDown), its circuit is open, or
			// the stored document is invalid. The next candidate would only
			// repeat the same retries against the same outage.
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

// connectionTestError is the text a connection test reports for err. A
// failure to connect — a DNS, dial, proxy or TLS failure, or the egress
// guard refusing the address — is fixed copy (egress.DescribeFailure): the
// raw text names the address the host resolved to and the operator's
// HTTP(S) proxy, which a client must never see; the detail goes to the
// server log. Anything else (an HTTP answer, Azure's explanation, a
// validation error) is the error's own text. target names what could not be
// reached ("" = the endpoint).
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
		if saveErr := s.secrets.Save(scope, "github-models-token", result.Token); saveErr != nil {
			logger.Error("failed to save github token", "error", saveErr)
		} else {
			// A replacement token may carry other entitlements: the previous
			// token's cached listing must not outlive it (mirrors Revoke).
			s.InvalidateModels(scope, "github-models")
		}
		result.Token = ""
	}

	return result, nil
}

func (s *ProviderService) RevokeGitHubAuth(scope string) (err error) {
	defer logger.Guard("App.RevokeGitHubAuth", &err)
	if err := s.secrets.Delete(scope, "github-models-token"); err != nil {
		return err
	}
	// github-models is a cacheable live listing: drop it so the revoked
	// credential's models disappear immediately instead of after
	// dynamicModelTTL (mirrors DeleteApiKey's hook).
	s.InvalidateModels(scope, "github-models")
	return nil
}

func (s *ProviderService) GetGitHubUser(ctx context.Context, scope string) (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetGitHubUser", &err)

	token, err := s.secrets.Get(scope, "github-models-token")
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
		if saveErr := s.secrets.Save(scope, "copilot-oauth-token", result.Token); saveErr != nil {
			logger.Error("failed to save copilot oauth token", "error", saveErr)
		} else {
			// A replacement token may carry other entitlements: the previous
			// token's cached listing must not outlive it (mirrors Revoke).
			s.InvalidateModels(scope, "copilot")
		}
		result.Token = ""
		// C-4: pre-warm the Copilot session-token cache so the first chat turn
		// doesn't pay the session-token exchange RTT on the critical path.
		// ListProviders will also warm it on next session load, but firing it
		// now covers the turn the user sends immediately after connecting.
		s.preWarmCopilot(scope)
	}

	return result, nil
}

// preWarmCopilot best-effort resolves the configured Copilot GitHub token and
// triggers a background session-token exchange so the cache is primed before
// the first chat turn. Fire-and-forget: errors are logged and swallowed (the
// token will be exchanged lazily on first Stream if this fails). The context
// is detached from the caller and bounded so a slow exchange can't linger.
func (s *ProviderService) preWarmCopilot(scope string) {
	if s.copilotAuth == nil {
		return
	}
	go func() {
		// The exchange is HTTP+JSON into provider-owned code; a panic here
		// would crash the whole process (nothing above recovers a detached
		// goroutine). Match startLiveListing: recover, log, and let the
		// first Stream exchange lazily instead.
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("copilot session-token pre-warm panicked", "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// OAuth device-flow token (per-user) first, then manual PAT.
		token, err := s.secrets.Get(scope, "copilot-oauth-token")
		if err != nil || token == "" {
			token, err = s.secrets.Get(scope, "copilot")
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
	if err := s.secrets.Delete(scope, "copilot-oauth-token"); err != nil {
		return err
	}
	// copilot is a cacheable live listing: drop it so the revoked
	// credential's models disappear immediately instead of after
	// dynamicModelTTL (mirrors DeleteApiKey's hook).
	s.InvalidateModels(scope, "copilot")
	return nil
}

func (s *ProviderService) GetCopilotUser(ctx context.Context, scope string) (user *ai.GitHubUser, err error) {
	defer logger.Guard("App.GetCopilotUser", &err)

	token, err := s.secrets.Get(scope, "copilot-oauth-token")
	if err != nil {
		// No stored token (or no secret storage) → not connected, not an error.
		return nil, nil
	}
	return s.auth.GetUser(ctx, token)
}

// --- Local models configuration ---

// GetLocalConfig returns the caller's stored local models configuration (base
// URL and stored context window). An absent or unreadable document is the
// unconfigured state. Where local models are unavailable (cloud) it refuses
// with the *ai.LocalConfigError a save gets (LOCAL_MODELS_UNAVAILABLE), so
// the settings UI drops the section instead of offering an unusable form.
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

// SaveLocalConfig validates the base URL (loopback http(s) server root, and
// only where the runtime egress policy allows loopback) and the context
// window (0 = the default, else 1024..2000000 tokens), and stores the
// document. Refusals are *ai.LocalConfigError values carrying a client code.
// The cached listing of the previous configuration is dropped (its entries
// carry the previous window).
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
	s.InvalidateModels(scope, ai.LocalProviderID)
	return cfg, nil
}

// --- Azure AI Foundry configuration ---

// GetAzureFoundryConfig returns the caller's stored Azure AI Foundry document
// with the secrets removed. A missing or unreadable document reads as
// unconfigured (the user re-enters it). Deprecated with the model registry
// (one release): it reads resource "default", its deployment fields built
// from the registry's entries, never from the document's stale ones.
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

// SaveAzureFoundryConfig validates and stores the document. A request that
// omits the secret keeps the stored one — for an API key whenever the auth
// mode matches, for Microsoft Entra ID only when the tenant ID and client ID
// are unchanged (the same service principal), and for either only while the
// endpoint host is unchanged (carryAzureSecret: a changed host is
// MISSING_SECRET, the secret must be re-entered) — so editing a deployment
// name never asks the user to re-enter a key but rotating to a
// DIFFERENT principal cannot silently attach the old principal's secret
// (which Entra ID would later reject with AADSTS7000215). A changed principal
// with no secret fails validation like any missing secret. Validation
// failures are *ai.AzureConfigError values carrying a client code. When the
// saved document's credential differs from the previous one's, what the
// process kept for the previous credential (its Entra ID token source and
// cached listing) is released with it. Deprecated with the model registry
// (one release): the deployment fields are mapped onto resource "default"'s
// entries (missing ones created, removed ones deleted) and only the
// credential part is saved, the document's old deployment fields carried
// over unchanged (a rolled-back binary still reads them).
func (s *ProviderService) SaveAzureFoundryConfig(scope string, cfg ai.AzureFoundryConfig) (err error) {
	defer logger.Guard("App.SaveAzureFoundryConfig", &err)
	if s.azure != nil {
		if err := s.azure.legacySaveConfig(context.Background(), scope, cfg); err != nil {
			return err
		}
		s.InvalidateModels(scope, ai.AzureFoundryProviderID)
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
	s.InvalidateModels(scope, ai.AzureFoundryProviderID)
	ai.InvalidateAzureFoundryModels(cfg)
	if hadPrev && prev.Fingerprint() != cfg.Fingerprint() {
		ai.ForgetAzureFoundryCredential(prev)
	}
	return nil
}

// --- Organisation-shared Azure AI Foundry configuration (G7) ---
//
// Trust model: an org admin is untrusted (anyone can create an org), so the
// shared document is only what that admin lends the org's members. Members
// read its public form (no secret, ever); admins write it; admins and
// members — never viewers, guests or outside collaborators — may run on it,
// a member's own document winning over it for chat (ProviderFactory.ForOrg),
// while the knowledge base prefers it (one corpus, one resource). Usage on
// it is attributed to the org but never budget-gated: Azure deployments are
// not priced by baki (their cost is read in the Azure portal), so the org's
// daily budget does not count them. The row goes with the organisation.
// When the admin who supplied its secret (the lender, secret_set_by) stops
// being an admin — demoted, removed, account deleted — it is disabled
// (DisableOrgAzureFoundryConfigSavedBy): the credential was theirs to lend,
// and an edit by another admin that kept the stored secret does not make it
// theirs (it moves updated_by only, never secret_set_by).

// OrgAzureFoundryConfig is an organisation's shared Azure AI Foundry
// document in its public form, with who shared it and when. UpdatedBy is the
// lender's display name — the admin who supplied the stored secret, resolved
// by the handler from SecretSetByID (an edit that kept the secret does not
// change who lends it); UpdatedByID is the admin who last saved the
// document. UpdatedAt is the last save, RFC 3339 with the store's full
// sub-second precision — a save sends it back verbatim as its
// compare-and-swap token.
type OrgAzureFoundryConfig struct {
	ai.AzureFoundryPublicConfig
	Shared        bool   `json:"shared"`
	UpdatedBy     string `json:"updatedBy,omitempty"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
	UpdatedByID   string `json:"-"`
	SecretSetByID string `json:"-"`
}

// GetOrgAzureFoundryConfig returns the organisation's shared document with
// the secrets removed; a missing document reads as unconfigured (Configured
// false, Shared false). One that exists but cannot be read (a decrypt or
// database failure, an unparseable document) is ErrOrgCredentialUnreadable,
// not "unconfigured": the org HAS a credential its members are failing on.
// Deprecated with the model registry (one release): the organisation's
// resource "default", its deployment fields built from the registry.
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

// SaveOrgAzureFoundryConfig is SaveAzureFoundryConfig for the organisation's
// shared document: the same validation, the same carry-over of an omitted
// secret from the stored document (endpoint hosts unchanged), the same
// release of a replaced credential's process state. updatedBy is the saving
// admin's user id; it becomes the credential's lender only when the save
// supplies the secret — a carried-over secret keeps its lender. The org's
// cached listings (every member's, under ai.OrgScope) are dropped so the new
// deployments show up on the next listing.
//
// Compare-and-swap: expectedUpdatedAt is the updatedAt the admin's GET
// returned ("" when it said configured:false). The write happens only while
// the stored document still carries it — or, for "", while there is none —
// else ErrOrgCredentialConflict and nothing is written, so two admins
// editing at once can no longer silently overwrite each other. Returns the
// new updatedAt (the next save's token).
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
	// The Azure CLI sign-in mode is a personal, desktop-only credential: it
	// runs as whoever is signed into `az` on the machine that chats. An
	// org-shared document is used by every member on their own machines, so
	// it must be an API key or a service principal. Refused here, at save
	// time, in every deployment mode — the provider-level validation only
	// refuses the mode on server deployments (F-51).
	if cfg.Auth.Mode == ai.AzureAuthModeEntraCLI {
		return "", &ai.AzureConfigError{Code: ai.AzureCfgErrInvalidAuth,
			Message: "the Azure CLI sign-in mode cannot be shared with an organisation — save it as your own Azure AI Foundry configuration, or share an API key or Microsoft Entra ID service principal"}
	}
	if s.azure != nil && s.azure.OrgRegistriesAvailable() {
		// Deprecated with the model registry: the credential part is saved
		// (the same compare-and-swap and repair as below), the deployment
		// fields are mapped onto the organisation's resource "default".
		at, err := s.azure.legacyOrgSaveConfig(context.Background(), orgID, updatedBy, expected, cfg)
		if err != nil {
			return "", err
		}
		s.InvalidateScope(ai.OrgScope(orgID))
		return at, nil
	}
	// A token-less save that supplies a fresh secret may be the repair of
	// an unreadable stored row (a decrypt failure after a key rotation, a
	// document this build cannot parse): its GET fails with
	// ErrOrgCredentialUnreadable and hands back no compare-and-swap token,
	// so the zero-expected branch below would answer 409 against the very
	// row the save is meant to replace. The row's OrgKeyMeta stamp becomes
	// the token instead — a true compare-and-swap, so two admins repairing
	// at once cannot overwrite each other — and hadPrev stays false (the
	// stored document is unusable, nothing is carried and the repairing
	// admin becomes the new lender). Any other stored state (no row, a
	// readable document, a store error) keeps today's path.
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
	// Validation passed, so the document holds a secret: one the request did
	// not supply was carried over from the stored document, and its lender
	// stays. The compare-and-swap guarantees the row carried over from is the
	// row being replaced.
	secretCarried := !supplied
	at, ok, err := s.orgKeys.SaveOrgIfUnchanged(orgID, ai.AzureFoundryProviderID, raw, updatedBy, secretCarried, expected)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrOrgCredentialConflict
	}
	s.InvalidateScope(ai.OrgScope(orgID))
	ai.InvalidateAzureFoundryModels(cfg)
	if hadPrev && prev.Fingerprint() != cfg.Fingerprint() {
		ai.ForgetAzureFoundryCredential(prev)
	}
	return formatOrgKeyStamp(at), nil
}

// DeleteOrgAzureFoundryConfig removes the organisation's shared document,
// releases what the process kept for its credential and drops the org's
// cached listings. Deleting a missing document is a no-op. Deprecated with
// the model registry (one release): it removes the organisation's resource
// "default" with its models (the credential after the commit).
func (s *ProviderService) DeleteOrgAzureFoundryConfig(orgID string) (err error) {
	defer logger.Guard("App.DeleteOrgAzureFoundryConfig", &err)
	if s.orgKeys == nil {
		return ErrOrgKeyStoreUnavailable
	}
	if s.azure != nil && s.azure.OrgRegistriesAvailable() {
		if err := s.azure.legacyDeleteDefault(context.Background(), storageif.AzureOrgOwner(orgID), ""); err != nil {
			return err
		}
		s.InvalidateScope(ai.OrgScope(orgID))
		return nil
	}
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.orgKeys.GetOrg(orgID, ai.AzureFoundryProviderID) })
	if err := s.orgKeys.DeleteOrg(orgID, ai.AzureFoundryProviderID); err != nil {
		return err
	}
	if hadPrev {
		ai.ForgetAzureFoundryCredential(prev)
	}
	s.InvalidateScope(ai.OrgScope(orgID))
	return nil
}

// DisableOrgAzureFoundryConfigSavedBy disables the organisation's shared
// Azure AI Foundry credentials when userID is the admin who supplied their
// secret — the lender, secret_set_by (updated_by only for a row no save has
// stamped since the v24 backfill) — called when that admin is demoted,
// removed from the org or deletes their account: the credential was theirs
// to lend, and a key its owner can no longer manage must not stay live for
// the org. Another admin's later edit that kept the stored secret does not
// save it from its lender's departure, and does not make the editor a
// lender either. The delete is conditional on the stored secret_set_by, so a
// newer save by another admin with their own secret is kept. Family-wide:
// every resource's credential userID lent goes (see
// DisableOrgAzureCredentialsSavedBy, which names the keys for the audit
// rows); disabled reports whether any did — the F-48 sharedCredentialDisabled
// flag. A deployment without an org store has nothing to disable.
func (s *ProviderService) DisableOrgAzureFoundryConfigSavedBy(orgID, userID string) (disabled bool, err error) {
	defer logger.Guard("App.DisableOrgAzureFoundryConfigSavedBy", &err)
	keys, err := s.DisableOrgAzureCredentialsSavedBy(orgID, userID)
	return len(keys) > 0, err
}

// DisableOrgCredentialsSavedBy is DisableOrgAzureFoundryConfigSavedBy for
// every organisation whose shared credentials userID lent (account erasure:
// the membership rows go with the account, the credential rows do not).
// Returns the orgs disabled; an org that fails is logged and skipped so the
// others are still disabled. DisableOrgAzureCredentialsLentBy names the keys.
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

// PrepareOrgDeletion reads what the process must release when orgID is
// deleted — every Azure AI Foundry credential of the organisation (each one's
// Entra ID token source and cached listings) — while the rows still exist
// (they cascade with the organisation, as do its registry rows), and returns
// the release to run once the delete succeeded: forget those credentials,
// drop the org's cached listings and what the registry remembers of it.
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

// PrepareAccountErasure is PrepareOrgDeletion for a user's own credentials:
// it reads every Azure AI Foundry credential of the user before the erasure
// cascades them away (with the user's registry rows) and returns the release
// that forgets them and drops the scope's cached listings.
func (s *ProviderService) PrepareAccountErasure(scope string) (release func()) {
	var docs []string
	if s.secrets != nil {
		var ids []string
		if s.azure != nil {
			ids = s.azure.registryConnectionIDs(context.Background(), s.azure.userOwner(scope))
		}
		docs = userAzureDocuments(s.secrets, scope, ids)
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
