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
		providerID:            azureFoundryProviderID,
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
		markRerouted(ctx)
		recordRouteFallback(p.providerID, fallbackResponsesReroute)
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
	// retrying marks the recorder of a withTokenRetry: a nested withTokenRetry (the Responses reroute inside the
	// turn's own) runs once and leaves the replay to it (R-provider-c5).
	retrying bool
	// rerouted records that chat completions rejected the turn's tools with reasoning and the turn went to the
	// Responses route: the replay goes there directly instead of paying the rejection again.
	rerouted bool
}

// markRerouted records the reroute on ctx's recorder, if any.
func markRerouted(ctx context.Context) {
	if use, _ := ctx.Value(entraTokenUseKey{}).(*entraTokenUse); use != nil {
		use.mu.Lock()
		use.rerouted = true
		use.mu.Unlock()
	}
}

// reroutedIn reports a reroute recorded on ctx's recorder.
func reroutedIn(ctx context.Context) bool {
	use, _ := ctx.Value(entraTokenUseKey{}).(*entraTokenUse)
	if use == nil {
		return false
	}
	use.mu.Lock()
	defer use.mu.Unlock()
	return use.rerouted
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
// (signing-key rollover, a gateway hiccup); the same token coming back means the rejection is real. Re-entrant:
// inside another withTokenRetry fn runs once on the outer recorder, so the outer level sees the token the inner
// call sent and owns the one replay of the turn.
func (p *AzureFoundryProvider) withTokenRetry(ctx context.Context, fn func(ctx context.Context) error) error {
	if outer, _ := ctx.Value(entraTokenUseKey{}).(*entraTokenUse); outer != nil && outer.retrying {
		return fn(ctx)
	}
	callCtx, use := withEntraTokenUse(ctx)
	use.retrying = true
	err := fn(callCtx)
	if err == nil || p.tokens == nil || !errors.Is(err, ErrApiKeyInvalid) ||
		errors.Is(err, errEntraToken) || errors.Is(err, ErrProviderPermissionDenied) {
		return err
	}
	failed := use.get()
	if failed == "" {
		return err // no token was sent: nothing to replace
	}
	recordEntraTokenEvent(entraTokenRejected)
	p.tokens.Invalidate(failed)
	fresh, ferr := p.tokens.Token(ctx)
	if ferr != nil || fresh == failed {
		return err
	}
	logger.FromContext(ctx).Warn("azure ai foundry rejected a cached entra id token; retrying with a fresh one")
	// The replay keeps the recorder (its reroute mark included): only the token is new.
	use.set("")
	return fn(callCtx)
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

func (p *AzureFoundryProvider) ID() string        { return azureFoundrySpec.ID }
func (p *AzureFoundryProvider) Name() string      { return azureFoundrySpec.Name }
func (p *AzureFoundryProvider) ContextLimit() int { return 128000 }
func (p *AzureFoundryProvider) FreeModel() string { return "" }

// SupportsTools: every Foundry chat model family exposes native function calling; models that don't reject the request and the loop degrades.
func (p *AzureFoundryProvider) SupportsTools() bool { return true }

// DefaultModel is the configured chat deployment, else the first additional one, else the first Claude one;
// "" (a document naming none, or a registry provider) lets the picker decide — never the first in sort order.
func (p *AzureFoundryProvider) DefaultModel() string {
	if p.fromRegistry {
		return ""
	}
	if !p.cfg.Configured() {
		return azureFoundrySpec.DefaultModel
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
	// A replay after a reroute goes straight to the Responses route: chat completions already refused the turn.
	return p.withTokenRetry(ctx, func(ctx context.Context) error {
		if reroutedIn(ctx) {
			return p.modelStream(ctx, req, onChunk)
		}
		return p.stream(ctx, req, onChunk)
	})
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

// azureModelsRetryAfter is the least wait before the project is asked again after a listing cut short by a
// transient failure (network, 5xx, 429, an unobtainable token, the flight's budget); a throttled listing waits
// for the project's Retry-After when that is longer (storeAzureListing), up to azureModelsRetryMax.
const azureModelsRetryAfter = 30 * time.Second

// azureModelsRetryMax bounds the project's Retry-After (any number of seconds, or a date): a throttled listing is
// asked again within ten minutes however long the project asked for, as the shortest TTL it would replace allows.
const azureModelsRetryMax = 10 * time.Minute

const azureModelsCacheMax = 512

// azureModelsFlight collapses concurrent listings of one document into one pair of project calls; see Models.
var azureModelsFlight singleflight.Group

// azureProjectExtrasTimeout is the fallback budget of a listing flight whose caller brings no deadline (a document
// provider's Models): the Entra ID token and both paginated listings, every page. A refresh carries its caller's
// deadline instead (listingBudget). The project probe is bounded by it too. A variable so tests can shorten it.
var azureProjectExtrasTimeout = 10 * time.Second

// azureListingMaxBudget caps the budget a caller's deadline gives a listing flight; the background refresher's
// 45 s fits under it.
const azureListingMaxBudget = time.Minute

type azureModelsEntry struct {
	models []ModelInfo
	// expires ends the listing's TTL: azureModelsCacheTTL for a complete one, the retry wait for an incomplete one.
	expires time.Time
	// complete: both project listings answered (or definitively refused). deployments: the deployments listing
	// did, so the entries carry its decorations — an incomplete refresh never replaces such a listing.
	complete    bool
	deployments bool
	// fetchedAt is when the listing's oldest part was fetched: a listing that kept the previous agents is as old as they are.
	fetchedAt time.Time
	// retryAt pushes the next attempt past expires: a kept listing whose refresh failed (storeAzureListing).
	retryAt time.Time
}

// nextAttempt is when a refresh may ask the project again: the end of the TTL, or a kept listing's retry.
func (e azureModelsEntry) nextAttempt() time.Time {
	if e.retryAt.After(e.expires) {
		return e.retryAt
	}
	return e.expires
}

// tooOld reports whether the listing's oldest part is past azureModelsStaleGrace beyond its TTL at now: it is served
// no longer, whatever retry is pending. An entry without a fetch time (tests seed some) has no such bound.
func (e azureModelsEntry) tooOld(now time.Time) bool {
	return !e.fetchedAt.IsZero() && !now.Before(e.fetchedAt.Add(azureModelsCacheTTL+azureModelsStaleGrace))
}

// ModelsListing is the state of a resource's cached project listing, read without the network.
type ModelsListing struct {
	// Complete: the listing served answered in full — deployments and agents (or a definitive refusal). A
	// listing kept over a failed refresh stays complete; one with the previous agents kept is not.
	Complete bool
	// AsOf is when the oldest part of the listing served was fetched; zero when none is served.
	AsOf time.Time
	// Live: within its TTL. Due: a refresh would ask the project now (not live, no retry pending).
	Live bool
	Due  bool
}

var azureModelsCache = struct {
	mu sync.Mutex
	m  map[string]azureModelsEntry
}{m: map[string]azureModelsEntry{}}

// pruneAzureModelsCacheLocked makes room for one new entry WITHOUT wiping the map: expired listings go first,
// else the one closest to expiry — the whole-map wipe re-paid the project listing for every active user at once.
// A listing whose retry is pending is not expired: dropping it would ask a throttled project again at once.
func pruneAzureModelsCacheLocked(now time.Time) {
	for k, e := range azureModelsCache.m {
		if !now.Before(e.nextAttempt()) {
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
	return azureModelsCacheKeyFor(cfg.Fingerprint(), cfg)
}

// azureModelsCacheKeyFor is azureModelsCacheKey with the credential set's fingerprint given: a caller that
// remembers it keys the listing without the credential.
func azureModelsCacheKeyFor(fingerprint string, cfg AzureFoundryConfig) string {
	parts := append([]string{fingerprint, cfg.ChatDeployment, cfg.EmbeddingDeployment}, cfg.Deployments...)
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
	if cached, ok := cachedAzureModelsUntilDue(key); ok {
		return cached, nil
	}

	// One listing per document at a time: the flight is detached from the first caller's cancellation (its
	// result fills the cache for everyone) and bounded by the fallback budget; each caller waits under its own
	// ctx, else falls back to the document's deployments. What is cached: storeAzureListing.
	ch := azureModelsFlight.DoChan(key, p.listingFlight(ctx, key, azureProjectExtrasTimeout))
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
// the caller's cancellation — context.WithoutCancel drops its deadline too, so the flight runs within budget,
// which the caller derives (listingBudget, or the fallback) — and cached by storeAzureListing. It returns the
// listing now cached, which an incomplete attempt may have left as the earlier one.
func (p *AzureFoundryProvider) listingFlight(ctx context.Context, key string, budget time.Duration) func() (any, error) {
	return func() (any, error) {
		listed := p.projectListing(withoutEntraTokenUse(context.WithoutCancel(ctx)), p.configuredDeployments(), budget)
		return storeAzureListing(key, listed), nil
	}
}

// listingBudget is the budget a refresh carries into its detached flight: the caller's remaining time less a
// margin, so the flight ends — and caches what it read — before the caller stops waiting (capped by
// azureListingMaxBudget); a caller without a deadline (or past it) gets the fallback, azureProjectExtrasTimeout.
func listingBudget(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return azureProjectExtrasTimeout
	}
	left := time.Until(deadline)
	if left <= 0 {
		return azureProjectExtrasTimeout
	}
	return min(left-min(left/10, 2*time.Second), azureListingMaxBudget)
}

// cachedAzureModels returns a copy of the cached listing for key while it is live.
func cachedAzureModels(key string) ([]ModelInfo, bool) {
	return cachedAzureModelsWithin(key, 0)
}

// cachedAzureModelsUntilDue returns a copy of the cached listing for key until a refresh is due: while live, or
// while a kept listing's retry is pending — a document provider never asks the project before then — but never
// past the listing's age bound (tooOld).
func cachedAzureModelsUntilDue(key string) ([]ModelInfo, bool) {
	now := time.Now()
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	e, ok := azureModelsCache.m[key]
	if !ok || !now.Before(e.nextAttempt()) || e.tooOld(now) {
		return nil, false
	}
	out := make([]ModelInfo, len(e.models))
	copy(out, e.models)
	return out, true
}

// azureListingDue reports whether a refresh of key would ask the project now: nothing cached, or the TTL and any
// pending retry are over.
func azureListingDue(key string) bool {
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	e, ok := azureModelsCache.m[key]
	return !ok || !time.Now().Before(e.nextAttempt())
}

// azureListingState is ModelsListing for key; grace is how long past its expiry the reader serves a listing.
func azureListingState(key string, grace time.Duration) ModelsListing {
	now := time.Now()
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	e, ok := azureModelsCache.m[key]
	if !ok {
		return ModelsListing{Due: true}
	}
	out := ModelsListing{Due: !now.Before(e.nextAttempt())}
	if (now.Before(e.expires.Add(grace)) || !out.Due) && !e.tooOld(now) {
		out.Complete, out.AsOf, out.Live = e.complete, e.fetchedAt, now.Before(e.expires)
	}
	return out
}

// ModelsListing reports the state of the listing this provider serves (see ModelsListing): a registry provider
// serves one through azureModelsStaleGrace. A provider that lists no project has nothing to refresh: complete,
// never due.
func (p *AzureFoundryProvider) ModelsListing() ModelsListing {
	if p.configErr != nil || p.isolated || !p.cfg.Configured() || !p.cfg.listsProjectDeployments() {
		return ModelsListing{Complete: true}
	}
	grace := time.Duration(0)
	if p.fromRegistry {
		grace = azureModelsStaleGrace
	}
	return azureListingState(azureModelsCacheKey(p.cfg), grace)
}

// withCachedAzureModels runs fn on the cached listing IN PLACE under the cache mutex; fn must not retain the
// slice — it is the cache's own backing array, valid only while the lock is held.
func withCachedAzureModels[T any](key string, grace time.Duration, fn func([]ModelInfo) T) (T, bool) {
	now := time.Now()
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	e, ok := azureModelsCache.m[key]
	if !ok || !now.Before(e.expires.Add(grace)) || e.tooOld(now) {
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

// storeAzureModels caches models for key for ttl as they are (tests seed the cache with it); a listing attempt is
// cached by storeAzureListing.
func storeAzureModels(key string, models []ModelInfo, ttl time.Duration) {
	now := time.Now()
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	if len(azureModelsCache.m) >= azureModelsCacheMax {
		pruneAzureModelsCacheLocked(now)
	}
	complete := ttl == azureModelsCacheTTL
	azureModelsCache.m[key] = azureModelsEntry{models: models, expires: now.Add(ttl), complete: complete, deployments: complete, fetchedAt: now}
}

// storeAzureListing caches one listing attempt for key and returns the listing now cached (the cache's own slice:
// callers copy it). A complete attempt replaces the entry for azureModelsCacheTTL. An incomplete one never
// replaces a listing whose deployments answered while that listing is within azureModelsStaleGrace of its TTL:
// when the attempt's own deployments answered (its agents did not), they are cached with the earlier listing's
// agents; otherwise the earlier listing is kept as it is. Either way the next attempt waits the project's
// Retry-After, at least azureModelsRetryAfter and at most azureModelsRetryMax — and so does an incomplete attempt
// cached on its own, with nothing better to keep. A kept listing is still served no longer than its age bound
// (tooOld). An empty listing is not cached.
func storeAzureListing(key string, l azureProjectListing) []ModelInfo {
	now := time.Now()
	wait := min(max(azureModelsRetryAfter, l.retryAfter), azureModelsRetryMax)
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	prev, had := azureModelsCache.m[key]
	keep := had && prev.deployments && now.Before(prev.fetchedAt.Add(azureModelsCacheTTL+azureModelsStaleGrace))
	var e azureModelsEntry
	switch {
	case l.complete():
		e = azureModelsEntry{models: l.models, expires: now.Add(azureModelsCacheTTL), complete: true, deployments: true, fetchedAt: now}
	case keep && l.deployments:
		e = azureModelsEntry{models: withListedAgents(l.models, prev.models), expires: now.Add(wait), deployments: true, fetchedAt: prev.fetchedAt}
	case keep:
		e = prev
		e.retryAt = now.Add(wait)
	default:
		e = azureModelsEntry{models: l.models, expires: now.Add(wait), deployments: l.deployments, fetchedAt: now}
	}
	if len(e.models) == 0 {
		return e.models
	}
	if !had && len(azureModelsCache.m) >= azureModelsCacheMax {
		pruneAzureModelsCacheLocked(now)
	}
	azureModelsCache.m[key] = e
	return e.models
}

// withListedAgents appends to models the agent entries of an earlier listing (prev) it lacks.
func withListedAgents(models, prev []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models)+len(prev))
	out = append(out, models...)
	for _, m := range prev {
		if IsAgentModel(m.ID) && listedIndex(out, m.ID, true) < 0 {
			out = append(out, m)
		}
	}
	return out
}

// projectListing runs one listing attempt within budget — the Entra ID token and both project listings — on
// ctx, which the flight detached from its caller; an unobtainable token skips the extras.
func (p *AzureFoundryProvider) projectListing(ctx context.Context, models []ModelInfo, budget time.Duration) azureProjectListing {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if _, err := p.entraToken(ctx); err != nil {
		logger.FromContext(ctx).Warn("azure ai foundry: entra id token unavailable; listing the configured deployments only", "error", err)
		return azureProjectListing{models: models}
	}
	return p.projectExtrasListing(ctx, models)
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
