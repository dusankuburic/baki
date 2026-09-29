package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/storage"
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
// caller's scope (per-user in cloud mode).
func (s *ProviderService) isConfigured(scope, providerID string) bool {
	switch providerID {
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
func (s *ProviderService) isOrgConfigured(orgID, providerID string) bool {
	if s.orgKeys == nil || orgID == "" {
		return false
	}
	switch providerID {
	case "copilot":
		return false
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
func (s *ProviderService) ListProviders(ctx context.Context, scope, orgID string) (providers []models.ProviderInfo, err error) {
	defer logger.Guard("App.ListProviders", &err)

	metas := ai.AvailableProviders()
	metaProviders := make([]ai.MetadataProvider, len(metas))
	configured := make([]bool, len(metas))
	shared := make([]bool, len(metas))
	live := make([]<-chan liveListing, len(metas))
	for i, meta := range metas {
		metaProviders[i] = ai.GetMetadataProvider(meta.ID)
		if metaProviders[i] == nil {
			continue
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

		providers = append(providers, models.ProviderInfo{
			ID:           meta.ID,
			Name:         meta.Name,
			Configured:   configured[i],
			Shared:       shared[i],
			Models:       modelDetails,
			DefaultModel: defaultModel,
			ContextLimit: p.ContextLimit(),
			AuthType:     meta.AuthType,
		})
	}

	return providers, nil
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
		msg := "no model to test: name a chat deployment in the provider settings"
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
		if errors.Is(chatErr, ai.ErrProviderPermissionDenied) || errors.Is(chatErr, ai.ErrEntraCredentialsRejected) || errors.Is(chatErr, ai.ErrEntraTokenRejected) {
			// Auth failures (they also match ErrApiKeyInvalid) whose one-line
			// text is what the user needs: the principal and the data action
			// its missing role grants, Entra ID's AADSTS code, or Azure's
			// explanation for a rejected Entra ID access token. "invalid
			// API key" named a key an Entra ID principal does not have.
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

// --- Azure AI Foundry configuration ---

// GetAzureFoundryConfig returns the caller's stored Azure AI Foundry document
// with the secrets removed. A missing or unreadable document reads as
// unconfigured (the user re-enters it).
func (s *ProviderService) GetAzureFoundryConfig(scope string) (cfg ai.AzureFoundryPublicConfig, err error) {
	defer logger.Guard("App.GetAzureFoundryConfig", &err)
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
// cached listing) is released with it.
func (s *ProviderService) SaveAzureFoundryConfig(scope string, cfg ai.AzureFoundryConfig) (err error) {
	defer logger.Guard("App.SaveAzureFoundryConfig", &err)
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

// storedAzureConfig reads and parses a stored document; hadPrev is false
// when there is none or it is unreadable (then nothing is carried over).
func storedAzureConfig(get func() (string, error)) (prev ai.AzureFoundryConfig, hadPrev bool) {
	raw, err := get()
	if err != nil || raw == "" {
		return ai.AzureFoundryConfig{}, false
	}
	p, parseErr := ai.ParseAzureFoundryConfig(raw)
	if parseErr != nil {
		return ai.AzureFoundryConfig{}, false
	}
	return p, true
}

// carryAzureSecret keeps the stored secret for a save that omits it — an API
// key whenever the auth mode matches, an Entra ID client secret only for the
// same principal (see SaveAzureFoundryConfig) — and only while the document
// still sends it to the hosts it was entered for: the endpoint's host
// unchanged, and a project endpoint (if any) on the previous project's or
// endpoint's host. Otherwise anyone able to edit the document — another org
// admin, for a shared one — could point the stored key at another
// allow-listed host without ever knowing it. A refusal for that reason is a
// MISSING_SECRET error saying why; nil means carried, or nothing to carry
// (validation then reports a missing secret the usual way).
func carryAzureSecret(cfg *ai.AzureFoundryConfig, prev ai.AzureFoundryConfig, hadPrev bool) error {
	if cfg.HasSecret() || !hadPrev || prev.Auth.Mode != cfg.Auth.Mode {
		return nil
	}
	switch cfg.Auth.Mode {
	case ai.AzureAuthModeAPIKey:
		// The resource key is not principal-bound.
	case ai.AzureAuthModeEntra:
		if prev.Auth.TenantID != cfg.Auth.TenantID || prev.Auth.ClientID != cfg.Auth.ClientID {
			return nil // another principal: its own secret is required
		}
	default:
		return nil
	}
	if !azureSecretHostsKept(prev, *cfg) {
		return &ai.AzureConfigError{
			Code:    ai.AzureCfgErrMissingSecret,
			Message: "the endpoint changed: re-enter the " + azureSecretName(cfg.Auth.Mode) + " — a stored secret is only kept for the endpoint it was entered for",
		}
	}
	if cfg.Auth.Mode == ai.AzureAuthModeEntra {
		cfg.Auth.ClientSecret = prev.Auth.ClientSecret
	} else {
		cfg.Auth.Key = prev.Auth.Key
	}
	return nil
}

// azureSecretName is how a save error names the secret a mode needs.
func azureSecretName(mode string) string {
	if mode == ai.AzureAuthModeEntra {
		return "client secret"
	}
	return "API key"
}

// azureSecretHostsKept reports whether next sends its credential only to
// hosts prev already sent it to: the same endpoint host, and no project
// endpoint or one on prev's project or endpoint host (the same resource). A
// removed project endpoint, another project path on the same host or a
// changed deployment name keeps the secret.
func azureSecretHostsKept(prev, next ai.AzureFoundryConfig) bool {
	prevHost, nextHost := azureURLHost(prev.Endpoint), azureURLHost(next.Endpoint)
	if nextHost == "" || nextHost != prevHost {
		return false
	}
	if next.ProjectEndpoint == "" {
		return true
	}
	project := azureURLHost(next.ProjectEndpoint)
	return project != "" && (project == prevHost || project == azureURLHost(prev.ProjectEndpoint))
}

// azureURLHost is the lower-cased host[:port] of an Azure URL, "" when it
// has none or does not parse.
func azureURLHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// saveCarryError is what a save whose secret was not carried reports: a
// validation failure of the document itself (a host that is not allowed, a
// missing deployment) wins over the refusal, which is only worth acting on
// once the rest of the document is right.
func saveCarryError(cfg ai.AzureFoundryConfig, refusal error) error {
	probe := cfg
	if probe.Auth.Mode == ai.AzureAuthModeEntra {
		probe.Auth.ClientSecret = "placeholder"
	} else {
		probe.Auth.Key = "placeholder"
	}
	if err := ai.ValidateAzureFoundryConfig(probe); err != nil {
		return err
	}
	return refusal
}

// --- Organisation-shared Azure AI Foundry configuration (G7) ---
//
// Trust model: an org admin is untrusted (anyone can create an org), so the
// shared document is only what that admin lends the org's members. Members
// read its public form (no secret, ever); admins write it; admins and
// members — never viewers, guests or outside collaborators — may run on it,
// a member's own document winning over it for chat (ProviderFactory.ForOrg),
// while the knowledge base prefers it (one corpus, one resource). Usage on
// it is attributed to the org and capped by the org's daily budget like
// every org-scoped turn, and the row goes with the organisation. When the
// admin who supplied its secret (the lender, secret_set_by) or last saved it
// stops being an admin — demoted, removed, account deleted — it is disabled
// (DisableOrgAzureFoundryConfigSavedBy): the credential was theirs to lend,
// and an edit by another admin that kept the stored secret does not make it
// theirs.

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
func (s *ProviderService) GetOrgAzureFoundryConfig(orgID string) (cfg OrgAzureFoundryConfig, err error) {
	defer logger.Guard("App.GetOrgAzureFoundryConfig", &err)
	if s.orgKeys == nil {
		return OrgAzureFoundryConfig{}, ErrOrgKeyStoreUnavailable
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

// formatOrgKeyStamp renders an org credential's updated_at as the
// compare-and-swap token the org GET returns and a save sends back:
// RFC 3339 in UTC with every sub-second digit the store kept.
func formatOrgKeyStamp(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

// parseOrgKeyStamp reads a save's updatedAt token; "" is the zero time (the
// caller saw no document).
func parseOrgKeyStamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, &ai.AzureConfigError{Code: ai.AzureCfgErrInvalidConfig, Message: "updatedAt must be the RFC 3339 timestamp the configuration was loaded with"}
	}
	return t, nil
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
// cached listings. Deleting a missing document is a no-op.
func (s *ProviderService) DeleteOrgAzureFoundryConfig(orgID string) (err error) {
	defer logger.Guard("App.DeleteOrgAzureFoundryConfig", &err)
	if s.orgKeys == nil {
		return ErrOrgKeyStoreUnavailable
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

// DisableOrgAzureFoundryConfigSavedBy deletes the organisation's shared
// document when userID is the admin who supplied its secret (the lender) or
// last saved it — called when that admin is demoted, removed from the org or
// deletes their account: the credential was theirs to lend, and a key its
// owner can no longer manage must not stay live for the org. Another admin's
// later edit that kept the stored secret does not save it from its lender's
// departure. The delete is conditional on the stored secret_set_by /
// updated_by, so a newer save by another admin with their own secret is
// kept. disabled reports whether a document was removed; the process state
// kept for it is released and the org's cached listings dropped, as
// DeleteOrgAzureFoundryConfig does. A deployment without an org store has
// nothing to disable.
func (s *ProviderService) DisableOrgAzureFoundryConfigSavedBy(orgID, userID string) (disabled bool, err error) {
	defer logger.Guard("App.DisableOrgAzureFoundryConfigSavedBy", &err)
	if s.orgKeys == nil || orgID == "" || userID == "" {
		return false, nil
	}
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.orgKeys.GetOrg(orgID, ai.AzureFoundryProviderID) })
	deleted, err := s.orgKeys.DeleteOrgIfSavedBy(orgID, ai.AzureFoundryProviderID, userID)
	if err != nil || !deleted {
		return false, err
	}
	if hadPrev {
		ai.ForgetAzureFoundryCredential(prev)
	}
	s.InvalidateScope(ai.OrgScope(orgID))
	return true, nil
}

// DisableOrgCredentialsSavedBy is DisableOrgAzureFoundryConfigSavedBy for
// every organisation whose shared document userID lent or last saved
// (account erasure: the membership rows go with the account, the credential
// rows do not). Returns the orgs disabled; an org that fails is logged and skipped
// so the others are still disabled.
func (s *ProviderService) DisableOrgCredentialsSavedBy(userID string) (orgIDs []string, err error) {
	defer logger.Guard("App.DisableOrgCredentialsSavedBy", &err)
	if s.orgKeys == nil || userID == "" {
		return nil, nil
	}
	candidates, err := s.orgKeys.OrgsWithKeySavedBy(ai.AzureFoundryProviderID, userID)
	if err != nil {
		return nil, err
	}
	for _, orgID := range candidates {
		disabled, derr := s.DisableOrgAzureFoundryConfigSavedBy(orgID, userID)
		if derr != nil {
			logger.Warn("azure ai foundry: disabling a departed admin's shared credential failed", "org", orgID, "error", derr)
			continue
		}
		if disabled {
			orgIDs = append(orgIDs, orgID)
		}
	}
	return orgIDs, nil
}

// PrepareOrgDeletion reads what the process must release when orgID is
// deleted — the shared document's credential (its Entra ID token source and
// cached listing) — while the row still exists (it cascades with the
// organisation), and returns the release to run once the delete succeeded:
// forget that credential and drop the org's cached listings.
func (s *ProviderService) PrepareOrgDeletion(orgID string) (release func()) {
	var prev ai.AzureFoundryConfig
	hadPrev := false
	if s.orgKeys != nil && orgID != "" {
		prev, hadPrev = storedAzureConfig(func() (string, error) { return s.orgKeys.GetOrg(orgID, ai.AzureFoundryProviderID) })
	}
	return func() {
		if hadPrev {
			ai.ForgetAzureFoundryCredential(prev)
		}
		s.InvalidateScope(ai.OrgScope(orgID))
	}
}

// PrepareAccountErasure is PrepareOrgDeletion for a user's own credentials:
// it reads the user's Azure AI Foundry document before the erasure cascades
// it away and returns the release that forgets its credential and drops the
// scope's cached listings.
func (s *ProviderService) PrepareAccountErasure(scope string) (release func()) {
	var prev ai.AzureFoundryConfig
	hadPrev := false
	if s.secrets != nil {
		prev, hadPrev = storedAzureConfig(func() (string, error) { return s.secrets.Get(scope, ai.AzureFoundryProviderID) })
	}
	return func() {
		if hadPrev {
			ai.ForgetAzureFoundryCredential(prev)
		}
		s.InvalidateScope(scope)
	}
}
