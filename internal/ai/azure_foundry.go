package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"pad-core/logger"
)

// AzureFoundryProvider serves the models deployed on an Azure AI Foundry resource through its OpenAI-compatible
// v1 endpoint, and, with a project endpoint and Entra ID credentials, the project's prompt agents (responses.go);
// /openai/v1/models lists the resource's catalogue, NOT its deployments (see Models).
type AzureFoundryProvider struct {
	openaiBase
	cfg AzureFoundryConfig
	// configErr is set when the stored document failed to parse or validate; the factory's constructor cannot
	// return an error, so every network method reports it instead of dialling a broken endpoint.
	configErr error
	// tokens is the Entra ID token source (AzureAuthModeEntra), nil for keys.
	tokens *entraTokenSource

	// claude is the Anthropic-wire provider for the resource's Claude deployments ("claude:<name>" ids), built on first use.
	claudeOnce sync.Once
	claude     *ClaudeProvider

	// fromRegistry marks a provider built for one resource of the model registry (NewAzureFoundryConnectionProvider):
	// its deployments are the registry's entries (registry, sorted), its listing never waits on the network and it names no default model.
	fromRegistry bool
	registry     []AzureRegisteredModel
	// isolated marks a provider that must leave the process-wide caches untouched — discovery and the tests:
	// no listing is read from or stored in azureModelsCache, no agent kind remembered (agentKinds).
	isolated bool
}

// NewAzureFoundryProvider builds the provider from the stored document; an empty document yields the unconfigured metadata provider (static catalogue, no network).
func NewAzureFoundryProvider(raw string) *AzureFoundryProvider {
	cfg, err := ParseAzureFoundryConfig(raw)
	if err == nil && cfg.Configured() {
		err = cfg.Validate(currentAzureHostPolicy())
	}
	return newAzureFoundryProvider(cfg, err, azureHTTPClient())
}

// newAzureFoundryProvider is the constructor proper; tests hand it a parsed (unvalidated) config and that server's client.
func newAzureFoundryProvider(cfg AzureFoundryConfig, cfgErr error, client *http.Client) *AzureFoundryProvider {
	return newAzureFoundryProviderFrom(cfg, cfgErr, client, processEntraSources)
}

// entraSourceFactory resolves the Entra ID token source a provider signs in with: processEntraSources shares
// one per principal; throwaway sources stay private to credentials that may not be saved (azure_probe.go).
type entraSourceFactory struct {
	secret func(tenantID, clientID, clientSecret string) (*entraTokenSource, error)
	cli    func(tenantID string) (*entraTokenSource, error)
}

var processEntraSources = entraSourceFactory{secret: entraSourceFor, cli: entraCLISourceFor}

// newAzureFoundryProviderFrom is newAzureFoundryProvider with the Entra ID token sources named (entraSourceFactory).
func newAzureFoundryProviderFrom(cfg AzureFoundryConfig, cfgErr error, client *http.Client, sources entraSourceFactory) *AzureFoundryProvider {
	p := &AzureFoundryProvider{}
	if cfgErr != nil {
		p.configErr = fmt.Errorf("%w: %w", ErrProviderConfigInvalid, cfgErr)
	}
	p.cfg = cfg

	base := cfg.Endpoint + "/openai/v1/chat/completions"
	p.openaiBase = openaiBase{
		client:                client,
		baseURL:               &base,
		providerLabel:         "azure ai foundry",
		embeddingModel:        cfg.EmbeddingDeployment,
		embeddingModelDefault: cfg.EmbeddingDeployment,
		maxTokensParam:        maxTokensParamCompletion,
		embedDimensionsFor:    azureEmbeddingDimensions,
	}
	if p.configErr != nil || !cfg.Configured() {
		return p
	}
	// A 403 is the resource refusing an accepted credential (network rules, a missing role), in either auth mode.
	p.handle403 = azureForbidden
	switch cfg.Auth.Mode {
	case AzureAuthModeAPIKey:
		// The key travels ONLY as the documented `api-key` header: a copy in Authorization: Bearer is not an
		// Entra token, and a front that validates bearers (APIM validate-jwt, the resource) rejected it.
		p.apiKey = cfg.Auth.Key
		p.apiKeyHeader = "api-key"
	case AzureAuthModeEntra:
		src, err := sources.secret(cfg.Auth.TenantID, cfg.Auth.ClientID, cfg.Auth.ClientSecret)
		if err != nil {
			p.configErr = fmt.Errorf("%w: %w", ErrProviderConfigInvalid, err)
			return p
		}
		p.tokens = src
		p.tokenFn = p.entraToken
		// A 401 in Entra ID mode is a rejected ACCESS TOKEN (audience, cloud, Entra auth disabled, revoked)
		// with Azure's explanation — not an API key the principal does not have (entraUnauthorized).
		p.handle401 = entraUnauthorized
	case AzureAuthModeEntraCLI:
		// The Azure CLI's signed-in user (desktop app only — the config layer refuses the mode under the cloud egress policy).
		src, err := sources.cli(cfg.Auth.TenantID)
		if err != nil {
			p.configErr = fmt.Errorf("%w: %w", ErrProviderConfigInvalid, err)
			return p
		}
		p.tokens = src
		p.tokenFn = p.entraToken
		p.handle401 = entraUnauthorized
	}
	// A deployment whose chat-completions request is rejected for combining function tools with reasoning
	// (gpt-5.6) is rerouted through the resource's Responses route, which keeps reasoning with tools.
	p.rerouteToolsRejection = func(ctx context.Context, req Request, onChunk func(Chunk)) (bool, error) {
		if use, known := responsesRouteFor(p.responsesRouteKey(req.Model)); known && !use {
			return false, nil // the route refused this model: repair instead
		}
		return true, p.withTokenRetry(ctx, func(ctx context.Context) error {
			return p.modelStream(ctx, req, onChunk)
		})
	}
	return p
}

// entraToken is the openaiBase tokenFn: it also records the token on the call's recorder, so a 401 invalidates THAT token.
func (p *AzureFoundryProvider) entraToken(ctx context.Context) (string, error) {
	tok, err := p.tokens.Token(ctx)
	if err != nil {
		return "", err
	}
	if use, _ := ctx.Value(entraTokenUseKey{}).(*entraTokenUse); use != nil {
		use.set(tok)
	}
	return tok, nil
}

// entraTokenUse records the Entra ID token one call sent. It is per call, not provider-wide: a token a
// concurrent listing fetched in between was invalidated instead of the one the service refused.
type entraTokenUse struct {
	mu  sync.Mutex
	tok string
}

func (u *entraTokenUse) set(tok string) {
	u.mu.Lock()
	u.tok = tok
	u.mu.Unlock()
}

func (u *entraTokenUse) get() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.tok
}

type entraTokenUseKey struct{}

// withEntraTokenUse returns ctx carrying a fresh recorder for one call.
func withEntraTokenUse(ctx context.Context) (context.Context, *entraTokenUse) {
	use := &entraTokenUse{}
	return context.WithValue(ctx, entraTokenUseKey{}, use), use
}

// withoutEntraTokenUse detaches ctx from a caller's recorder (the shared listing flight must not record into the chat call that started it).
func withoutEntraTokenUse(ctx context.Context) context.Context {
	return context.WithValue(ctx, entraTokenUseKey{}, (*entraTokenUse)(nil))
}

// withTokenRetry retries fn once with a fresh Entra ID token when the service refused one that has not expired
// (signing-key rollover, a gateway hiccup); the same token coming back means the rejection is real.
func (p *AzureFoundryProvider) withTokenRetry(ctx context.Context, fn func(ctx context.Context) error) error {
	callCtx, use := withEntraTokenUse(ctx)
	err := fn(callCtx)
	if err == nil || p.tokens == nil || !errors.Is(err, ErrApiKeyInvalid) ||
		errors.Is(err, errEntraToken) || errors.Is(err, ErrProviderPermissionDenied) {
		return err
	}
	failed := use.get()
	if failed == "" {
		return err // no token was sent: nothing to replace
	}
	p.tokens.Invalidate(failed)
	fresh, ferr := p.tokens.Token(ctx)
	if ferr != nil || fresh == failed {
		return err
	}
	logger.Warn("azure ai foundry rejected a cached entra id token; retrying with a fresh one")
	retryCtx, _ := withEntraTokenUse(ctx)
	return fn(retryCtx)
}

// authorize sets the auth headers on a request outside openaiBase (model and deployment listings, the Responses API).
func (p *AzureFoundryProvider) authorize(ctx context.Context, req *http.Request) error {
	switch p.cfg.Auth.Mode {
	case AzureAuthModeAPIKey:
		// Only the api-key header (see newAzureFoundryProvider).
		req.Header.Set("api-key", p.cfg.Auth.Key)
	case AzureAuthModeEntra, AzureAuthModeEntraCLI:
		tok, err := p.entraToken(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}

// Config returns the parsed document (redact with Public() before exposing).
func (p *AzureFoundryProvider) Config() AzureFoundryConfig { return p.cfg }

// ProbeAzureFoundryProject explains an empty listing for an Entra ID + project document that names no chat
// deployment: the token refusal or the deployments route's answer, not "name a chat deployment".
func ProbeAzureFoundryProject(ctx context.Context, raw string) error {
	return NewAzureFoundryProvider(raw).probeProject(ctx)
}

func (p *AzureFoundryProvider) probeProject(ctx context.Context) error {
	if p.configErr != nil {
		return p.configErr
	}
	if !p.cfg.Configured() || !p.cfg.listsProjectDeployments() {
		return nil
	}
	if _, err := p.entraToken(ctx); err != nil {
		return err
	}
	ectx, cancel := context.WithTimeout(ctx, azureProjectExtrasTimeout)
	defer cancel()
	if _, err := p.fetchProjectDeployments(ectx); err != nil {
		return fmt.Errorf("the project deployments listing failed: %w", err)
	}
	return nil
}

// ForgetAzureFoundryCredential releases what the process keeps for a document beyond the KeyStore: its Entra ID
// token source and cached project listings; memos keyed by resource hold no secret and stay.
func ForgetAzureFoundryCredential(cfg AzureFoundryConfig) {
	if !cfg.Configured() {
		return
	}
	switch cfg.Auth.Mode {
	case AzureAuthModeEntra:
		forgetEntraSource(cfg.Auth.TenantID, cfg.Auth.ClientID, cfg.Auth.ClientSecret)
	case AzureAuthModeEntraCLI:
		forgetEntraCLISource(cfg.Auth.TenantID)
	}
	// Every listing built on the credential, not only the document's own deployment keys: a registry
	// resource's listing is keyed by its entries.
	invalidateAzureCredentialListings(cfg.Fingerprint())
}

// servedMemoResource scopes the served-model memo to this resource (and project, whose agents are memoised by
// name): deployment names are only unique within one resource. Both endpoints are already normalised.
func (p *AzureFoundryProvider) servedMemoResource() string {
	if p.cfg.ProjectEndpoint == "" {
		return p.cfg.Endpoint
	}
	return p.cfg.Endpoint + "\x00" + p.cfg.ProjectEndpoint
}

// breakerScope keys circuit-breaker state by the configured resource (and project): a 5xx from one tenant's
// resource says nothing about another's. Endpoints, not the credential fingerprint: the key holds no secret.
func (p *AzureFoundryProvider) breakerScope() string {
	if p.cfg.Endpoint == "" && p.cfg.ProjectEndpoint == "" {
		return ""
	}
	return strings.ToLower(p.cfg.Endpoint) + "|" + strings.ToLower(p.cfg.ProjectEndpoint)
}

func (p *AzureFoundryProvider) ID() string        { return azureFoundryProviderID }
func (p *AzureFoundryProvider) Name() string      { return "Azure AI Foundry" }
func (p *AzureFoundryProvider) ContextLimit() int { return 128000 }
func (p *AzureFoundryProvider) FreeModel() string { return "" }

// SupportsTools: every Foundry chat model family exposes native function calling; models that don't reject the request and the loop degrades.
func (p *AzureFoundryProvider) SupportsTools() bool { return true }

// PricePerMillionTokens is zero on purpose: Foundry is not priced by baki (auditedProvider.pricingFor); the cost is read in the Azure portal.
func (p *AzureFoundryProvider) PricePerMillionTokens() Pricing { return Pricing{} }

// DefaultModel is the configured chat deployment, else the first additional one, else the first Claude one;
// "" (a document naming none, or a registry provider) lets the picker decide — never the first in sort order.
func (p *AzureFoundryProvider) DefaultModel() string {
	if p.fromRegistry {
		return ""
	}
	if !p.cfg.Configured() {
		return "gpt-4.1"
	}
	if names := p.cfg.chatDeployments(); len(names) > 0 {
		// The project listing may have re-listed it as a Claude deployment ("claude:<name>"), which /openai/v1 cannot serve.
		if id, ok := p.relistedClaude(names[0]); ok {
			return id
		}
		return names[0]
	}
	if names := p.cfg.ClaudeDeployments; len(names) > 0 {
		return claudeModelPrefix + names[0]
	}
	return ""
}

func (p *AzureFoundryProvider) EstimateTokens(text string) int { return EstimateTokensOpenAI(text) }

// setEmbeddingModel honours the deployment-wide embedding-model override (AI.EmbeddingModel) only when the
// document names no embedding deployment: a deployment name in the document is the more specific setting.
func (p *AzureFoundryProvider) setEmbeddingModel(model string) {
	if p.cfg.EmbeddingDeployment != "" {
		return
	}
	p.openaiBase.setEmbeddingModel(model)
}

func (p *AzureFoundryProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	// Claude deployments answer on the Anthropic Messages route, never on /openai/v1 (see azure_claude.go).
	if id, ok := p.claudeRoute(req.Model); ok {
		req.Model = id
		return p.claudeChat(ctx, req)
	}
	if IsAgentModel(req.Model) {
		var resp *Response
		err := p.withTokenRetry(ctx, func(ctx context.Context) error {
			var err error
			resp, err = p.agentChat(ctx, req)
			return err
		})
		return resp, err
	}
	// A deployment already rerouted to the Responses route keeps answering there on the non-streaming path too.
	if len(req.Tools) > 0 {
		if use, known := responsesRouteFor(p.responsesRouteKey(req.Model)); known && use {
			var resp *Response
			err := p.withTokenRetry(ctx, func(ctx context.Context) error {
				var err error
				resp, err = p.modelChat(ctx, req)
				return err
			})
			return resp, err
		}
	}
	var resp *Response
	err := p.withTokenRetry(ctx, func(ctx context.Context) error {
		var err error
		resp, err = p.chat(ctx, req)
		return err
	})
	return resp, err
}

func (p *AzureFoundryProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	if p.configErr != nil {
		return p.configErr
	}
	if id, ok := p.claudeRoute(req.Model); ok {
		req.Model = id
		return p.claudeStream(ctx, req, onChunk)
	}
	if IsAgentModel(req.Model) {
		return p.withTokenRetry(ctx, func(ctx context.Context) error { return p.agentStream(ctx, req, onChunk) })
	}
	// A tools turn already rerouted to the Responses route goes straight there; a route known to refuse the
	// model takes chat completions with the built-in repair at once — escaping would pay the rejection twice.
	if len(req.Tools) > 0 {
		switch use, known := responsesRouteFor(p.responsesRouteKey(req.Model)); {
		case known && use:
			return p.withTokenRetry(ctx, func(ctx context.Context) error { return p.modelStream(ctx, req, onChunk) })
		case known:
			return p.withTokenRetry(ctx, func(ctx context.Context) error { return p.streamInner(ctx, req, onChunk, false) })
		}
	}
	// A 401 arrives as the HTTP status, before any chunk, so a retry never replays a partially delivered stream.
	return p.withTokenRetry(ctx, func(ctx context.Context) error { return p.stream(ctx, req, onChunk) })
}

// claudeRoute reports whether model is served on the Anthropic Messages route and the id to call it by: a
// "claude:" id, or a bare deployment the cached project listing re-listed as Claude — /openai/v1 would 404.
func (p *AzureFoundryProvider) claudeRoute(model string) (string, bool) {
	if IsClaudeModel(model) {
		return model, true
	}
	return p.relistedClaude(model)
}

// cachedListing is this document's cached project listing while one is live; a registry provider also serves one
// expired less than azureModelsStaleGrace ago, and an isolated provider (discovery, the tests) reads none.
func (p *AzureFoundryProvider) cachedListing() ([]ModelInfo, bool) {
	if p.configErr != nil || p.isolated || !p.cfg.Configured() || !p.cfg.listsProjectDeployments() {
		return nil, false
	}
	if p.fromRegistry {
		return cachedAzureModelsWithin(azureModelsCacheKey(p.cfg), azureModelsStaleGrace)
	}
	return cachedAzureModels(azureModelsCacheKey(p.cfg))
}

// withCachedListing is cachedListing for read-only scans: fn reads the cached listing in place, under the cache
// lock — the per-turn routing scans keep nothing from the slice, so the copy cachedListing pays is pure overhead.
func (p *AzureFoundryProvider) withCachedListing(fn func([]ModelInfo) string) (string, bool) {
	if p.configErr != nil || p.isolated || !p.cfg.Configured() || !p.cfg.listsProjectDeployments() {
		return "", false
	}
	if p.fromRegistry {
		return withCachedAzureModels(azureModelsCacheKey(p.cfg), azureModelsStaleGrace, fn)
	}
	return withCachedAzureModels(azureModelsCacheKey(p.cfg), 0, fn)
}

// relistedClaude reports whether the cached listing offers the bare deployment name only as a Claude deployment, and the id it lists.
func (p *AzureFoundryProvider) relistedClaude(name string) (string, bool) {
	if name == "" || IsAgentModel(name) || IsClaudeModel(name) {
		return "", false
	}
	relisted, listed := p.withCachedListing(func(models []ModelInfo) string {
		claude := ""
		for _, m := range models {
			switch {
			case strings.EqualFold(m.ID, name):
				return "" // still listed as a chat deployment
			case claude == "" && IsClaudeModel(m.ID) && strings.EqualFold(claudeDeploymentName(m.ID), name):
				claude = m.ID
			}
		}
		return claude
	})
	if !listed {
		return "", false
	}
	return relisted, relisted != ""
}

// claudeServedModel names the model behind a Claude deployment for the sampling policy: the cached listing's
// modelName, else the served-model memo; "" when neither knows.
func (p *AzureFoundryProvider) claudeServedModel(deployment string) string {
	id := claudeModelPrefix + deployment
	if served, ok := p.withCachedListing(func(models []ModelInfo) string {
		for _, m := range models {
			if strings.EqualFold(m.ID, id) && m.ServedModel != "" {
				return m.ServedModel
			}
		}
		return ""
	}); ok && served != "" {
		return served
	}
	for _, requested := range []string{id, deployment} {
		if served, ok := lookupServedModelForResource(p.servedMemoResource(), azureFoundryProviderID, requested); ok {
			return served
		}
	}
	return ""
}

// ErrEmbeddingDeploymentMissing reports a configuration that names no embedding deployment; ForEmbedding returns it so the embedder sees the machine-readable error.
var ErrEmbeddingDeploymentMissing = errors.New("azure ai foundry: no embedding model for the knowledge base — add an embedding model under Settings → AI Accounts → Azure AI Foundry and mark it \"Use for knowledge base\"")

// ErrOrgEmbeddingDeploymentMissing is ErrEmbeddingDeploymentMissing against the ORG's shared credential: the
// member cannot fix it, so the message aims at an org admin; it wraps ErrEmbeddingDeploymentMissing.
var ErrOrgEmbeddingDeploymentMissing = fmt.Errorf("the organisation has no Azure AI Foundry embedding model for its knowledge base — ask an org admin to mark one of the organisation's embedding models \"Use for knowledge base\" (Settings → Organizations): %w", ErrEmbeddingDeploymentMissing)

// embeddingUnavailable satisfies embeddingReadiness (see ForEmbedding); it runs after the deployment-wide
// override was applied, so an AI.EmbeddingModel override still counts as a deployment.
func (p *AzureFoundryProvider) embeddingUnavailable() error {
	if p.configErr != nil {
		return p.configErr
	}
	if p.embeddingModel == "" {
		return ErrEmbeddingDeploymentMissing
	}
	return nil
}

// azureAdaName matches "ada" as a standalone token of a deployment name ("text-embedding-ada-002", "ada002") but not inside a word ("canada").
var azureAdaName = regexp.MustCompile(`(^|[^a-z])ada([^a-z]|$)`)

// azureEmbeddingDimensions decides `dimensions` for a DEPLOYMENT name, which need not reveal the model: a
// text-embedding-3 name follows the OpenAI rule, an ada name gets none, any other name gets the configured width.
func azureEmbeddingDimensions(deployment string, n int) int {
	name := strings.ToLower(deployment)
	switch {
	case n <= 0, azureAdaName.MatchString(name):
		return 0
	case strings.Contains(name, "text-embedding-3"):
		return embeddingDimensionsFor(deployment, n)
	}
	return n
}

func (p *AzureFoundryProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	if err := p.embeddingUnavailable(); err != nil {
		return nil, err
	}
	var vecs [][]float32
	err := p.withTokenRetry(ctx, func(ctx context.Context) error {
		var err error
		vecs, err = p.embed(ctx, text)
		return err
	})
	return vecs, err
}

// --- model listing ---

// azureModelsCacheTTL bounds how long a COMPLETE project listing is reused; the cache is keyed per credential
// set AND the document's deployment names (azureModelsCacheKey), and Models() runs on every turn.
const azureModelsCacheTTL = 5 * time.Minute

// azureModelsRetryAfter is how long a listing cut short by a transient project failure (network, 5xx, 429, an
// unobtainable token, the extras bound) is served before the project is asked again.
const azureModelsRetryAfter = 30 * time.Second

const azureModelsCacheMax = 512

// azureModelsFlight collapses concurrent listings of one document into one pair of project calls; see Models.
var azureModelsFlight singleflight.Group

// azureProjectExtrasTimeout bounds the project listings (deployments and agents, every page) as a whole. A variable so tests can shorten it.
var azureProjectExtrasTimeout = 10 * time.Second

type azureModelsEntry struct {
	models  []ModelInfo
	expires time.Time
}

var azureModelsCache = struct {
	mu sync.Mutex
	m  map[string]azureModelsEntry
}{m: map[string]azureModelsEntry{}}

// pruneAzureModelsCacheLocked makes room for one new entry WITHOUT wiping the map: expired listings go first,
// else the one closest to expiry — the whole-map wipe re-paid the project listing for every active user at once.
func pruneAzureModelsCacheLocked(now time.Time) {
	for k, e := range azureModelsCache.m {
		if !now.Before(e.expires) {
			delete(azureModelsCache.m, k)
		}
	}
	for len(azureModelsCache.m) >= azureModelsCacheMax {
		oldest, oldestExp, first := "", time.Time{}, true
		for k, e := range azureModelsCache.m {
			if first || e.expires.Before(oldestExp) {
				oldest, oldestExp, first = k, e.expires, false
			}
		}
		delete(azureModelsCache.m, oldest)
	}
}

// azureModelsCacheKey scopes a cached listing to the credential set AND the document's deployment names, which
// the listing is built from: two users of one shared resource must never see each other's listing.
func azureModelsCacheKey(cfg AzureFoundryConfig) string {
	parts := append([]string{cfg.Fingerprint(), cfg.ChatDeployment, cfg.EmbeddingDeployment}, cfg.Deployments...)
	for _, d := range cfg.ClaudeDeployments {
		parts = append(parts, claudeModelPrefix+d)
	}
	return strings.Join(parts, "\x00")
}

// ResetAzureFoundryCaches drops every process-wide memo (tests); a configuration save only invalidates that document's listing and token source.
func ResetAzureFoundryCaches() {
	azureModelsCache.mu.Lock()
	azureModelsCache.m = map[string]azureModelsEntry{}
	azureModelsCache.mu.Unlock()
	resetServedModels()
	resetLearnedWindows()
	resetLearnedRepairs()
	resetEmbedDimsRefusals()
	resetEntraSources()
	resetAgentKinds()
	resetResponsesToolRoutes()
}

// InvalidateAzureFoundryModels drops the cached listing for one document (same key as Models uses).
func InvalidateAzureFoundryModels(cfg AzureFoundryConfig) {
	azureModelsCache.mu.Lock()
	delete(azureModelsCache.m, azureModelsCacheKey(cfg))
	azureModelsCache.mu.Unlock()
}

// Models lists what the user can call: no data-plane route lists an API-key caller's deployments (/openai/v1/models
// is the catalogue), so the listing is the document's own deployments, sized from the catalogue — plus, with
// Entra ID and a project endpoint, the authoritative project deployments and agents. Unconfigured providers
// return the static catalogue; a registry or isolated provider never waits on the network.
func (p *AzureFoundryProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	if !p.cfg.Configured() {
		return catalogModels(azureFoundryProviderID), nil
	}
	models := p.configuredDeployments()
	if !p.cfg.listsProjectDeployments() || p.isolated {
		return models, nil
	}
	if p.fromRegistry {
		if cached, ok := p.cachedListing(); ok {
			return cached, nil
		}
		return models, nil
	}

	key := azureModelsCacheKey(p.cfg)
	if cached, ok := cachedAzureModels(key); ok {
		return cached, nil
	}

	// One listing per document at a time: the flight is detached from the first caller's cancellation (its
	// result fills the cache for everyone); each caller waits under its own ctx, else falls back to the
	// document's deployments. Complete: azureModelsCacheTTL; cut short: azureModelsRetryAfter.
	ch := azureModelsFlight.DoChan(key, p.listingFlight(ctx, key))
	select {
	case r := <-ch:
		listed := r.Val.([]ModelInfo)
		out := make([]ModelInfo, len(listed))
		copy(out, listed)
		return out, nil
	case <-ctx.Done():
		return models, nil
	}
}

// listingFlight is the shared project listing of one document (key): run once per key at a time, detached from
// the caller's cancellation, and cached as Models describes.
func (p *AzureFoundryProvider) listingFlight(ctx context.Context, key string) func() (any, error) {
	return func() (any, error) {
		listed, complete := p.projectListing(withoutEntraTokenUse(context.WithoutCancel(ctx)), p.configuredDeployments())
		if len(listed) > 0 {
			ttl := azureModelsCacheTTL
			if !complete {
				ttl = azureModelsRetryAfter
			}
			storeAzureModels(key, listed, ttl)
		}
		return listed, nil
	}
}

// cachedAzureModels returns a copy of the cached listing for key while it is live.
func cachedAzureModels(key string) ([]ModelInfo, bool) {
	return cachedAzureModelsWithin(key, 0)
}

// withCachedAzureModels runs fn on the cached listing IN PLACE under the cache mutex; fn must not retain the
// slice — it is the cache's own backing array, valid only while the lock is held.
func withCachedAzureModels[T any](key string, grace time.Duration, fn func([]ModelInfo) T) (T, bool) {
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	e, ok := azureModelsCache.m[key]
	if !ok || !time.Now().Before(e.expires.Add(grace)) {
		var zero T
		return zero, false
	}
	return fn(e.models), true
}

// cachedAzureModelsWithin is cachedAzureModels that also returns a listing expired less than grace ago; the
// listing is copied — its callers keep it past the lock, which the in-place reads forbid.
func cachedAzureModelsWithin(key string, grace time.Duration) ([]ModelInfo, bool) {
	return withCachedAzureModels(key, grace, func(models []ModelInfo) []ModelInfo {
		out := make([]ModelInfo, len(models))
		copy(out, models)
		return out
	})
}

// invalidateAzureCredentialListings drops every cached listing built on one credential set (Fingerprint),
// whatever deployment names keyed it: every key starts with the fingerprint (azureModelsCacheKey).
func invalidateAzureCredentialListings(fingerprint string) {
	prefix := fingerprint + "\x00"
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	for k := range azureModelsCache.m {
		if strings.HasPrefix(k, prefix) {
			delete(azureModelsCache.m, k)
		}
	}
}

func storeAzureModels(key string, models []ModelInfo, ttl time.Duration) {
	now := time.Now()
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	if len(azureModelsCache.m) >= azureModelsCacheMax {
		pruneAzureModelsCacheLocked(now)
	}
	azureModelsCache.m[key] = azureModelsEntry{models: models, expires: now.Add(ttl)}
}

// projectListing runs the project extras and reports completeness; the token is obtained under the caller's
// full context (the fetch is shared with chat turns) and an unobtainable token skips the extras.
func (p *AzureFoundryProvider) projectListing(ctx context.Context, models []ModelInfo) ([]ModelInfo, bool) {
	if _, err := p.entraToken(ctx); err != nil {
		logger.Warn("azure ai foundry: entra id token unavailable; listing the configured deployments only", "error", err)
		return models, false
	}
	ectx, cancel := context.WithTimeout(ctx, azureProjectExtrasTimeout)
	defer cancel()
	return p.projectExtras(ectx, models)
}

// configuredDeployments is the listing the document itself names: the chat deployment, the additional
// deployments, the Claude deployments, then the embedding deployment.
func (p *AzureFoundryProvider) configuredDeployments() []ModelInfo {
	var out []ModelInfo
	for _, d := range p.cfg.chatDeployments() {
		out = append(out, p.deploymentEntry(d, false))
	}
	for _, d := range p.cfg.ClaudeDeployments {
		out = append(out, claudeDeploymentEntry(d))
	}
	if d := p.cfg.EmbeddingDeployment; d != "" && !strings.EqualFold(d, p.cfg.ChatDeployment) {
		out = append(out, p.deploymentEntry(d, true))
	}
	return out
}

// deploymentEntry is the listing entry for one deployment name: a catalogued name carries that model's context
// window and output cap up front; a router alias keeps the conservative window, anything else the provider-wide one.
func (p *AzureFoundryProvider) deploymentEntry(name string, embedding bool) ModelInfo {
	m := ModelInfo{ID: name, DisplayName: name, ContextLimit: p.ContextLimit(), Embedding: embedding}
	if entry, ok := catalogEntryFor(azureFoundryProviderID, name); ok {
		m.Pricing = entry.Pricing
		if !isModelRouterAlias(name) {
			if entry.ContextLimit > 0 {
				m.ContextLimit = entry.ContextLimit
			}
			if entry.MaxOutputTokens > 0 {
				m.MaxOutputTokens = entry.MaxOutputTokens
			}
		}
	}
	return m
}
