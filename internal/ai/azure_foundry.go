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

// AzureFoundryProvider serves the models a user DEPLOYED on an Azure AI
// Foundry (Microsoft Foundry / Azure OpenAI) resource through the resource's
// OpenAI-compatible v1 endpoint — GPT-4.1/4o/5, o-series, gpt-oss, DeepSeek,
// Grok, Llama, Mistral, Phi, and the model router, all behind user-chosen
// deployment names — and, when a project endpoint and Entra ID credentials
// are configured, the project's prompt agents (see azure_agents.go /
// responses.go).
//
// Wire format: {endpoint}/openai/v1/chat/completions and /embeddings speak
// the OpenAI shape, so openaiBase does the work; the differences are the
// per-instance base URL, the `api-key` header, max_completion_tokens, and
// Azure's content-filter and retry-after-ms semantics (handled in the base
// so every OpenAI-compatible provider benefits). /openai/v1/models is NOT
// used: it lists the resource's model catalogue, not its deployments (see
// Models).
type AzureFoundryProvider struct {
	openaiBase
	cfg AzureFoundryConfig
	// configErr is set when the stored document failed to parse or validate.
	// The factory's constructor signature cannot return an error, so every
	// network method reports it (wrapped in ErrProviderConfigInvalid) instead
	// of dialling a broken endpoint.
	configErr error
	// tokens is the Entra ID token source (AzureAuthModeEntra), nil for keys.
	tokens *entraTokenSource

	// claude is the Anthropic-wire provider for the resource's Claude
	// deployments ("claude:<name>" ids), built on first use (claudeProvider).
	claudeOnce sync.Once
	claude     *ClaudeProvider
}

// NewAzureFoundryProvider builds the provider from the stored document (see
// ParseAzureFoundryConfig). An empty document yields the unconfigured
// metadata provider: static catalogue, no network.
func NewAzureFoundryProvider(raw string) *AzureFoundryProvider {
	cfg, err := ParseAzureFoundryConfig(raw)
	if err == nil && cfg.Configured() {
		err = cfg.Validate(currentAzureHostPolicy())
	}
	return newAzureFoundryProvider(cfg, err, azureHTTPClient())
}

// newAzureFoundryProvider is the constructor proper; tests hand it a parsed
// (unvalidated) config pointing at a local server and that server's client.
func newAzureFoundryProvider(cfg AzureFoundryConfig, cfgErr error, client *http.Client) *AzureFoundryProvider {
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
	switch cfg.Auth.Mode {
	case AzureAuthModeAPIKey:
		// Azure accepts the key either as `api-key` or as a bearer token;
		// send both so API Management fronts that only inspect one still work.
		p.apiKey = cfg.Auth.Key
		key := cfg.Auth.Key
		p.extraHeaders = func(req *http.Request, _ string) {
			req.Header.Set("api-key", key)
		}
	case AzureAuthModeEntra:
		src, err := entraSourceFor(cfg.Auth.TenantID, cfg.Auth.ClientID, cfg.Auth.ClientSecret)
		if err != nil {
			p.configErr = fmt.Errorf("%w: %w", ErrProviderConfigInvalid, err)
			return p
		}
		p.tokens = src
		p.tokenFn = p.entraToken
		// A 401/403 in Entra ID mode is a rejected ACCESS TOKEN (audience,
		// cloud, Entra auth disabled, revoked) with Azure's explanation —
		// not an API key the principal does not have. PermissionDenied keeps
		// its own path; API-key mode keeps the bare invalid-key mapping.
		p.handle401 = entraUnauthorized
	}
	return p
}

// entraToken is the openaiBase tokenFn: it also records the token on the
// call's entraTokenUse (withEntraTokenUse), so a 401 can invalidate exactly
// the token THAT call sent.
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

// entraTokenUse records the Entra ID token one call sent. It replaces a
// provider-wide "last token": the detached project listing (Models' flight)
// and a chat call share one provider, and a token the listing fetched in
// between was invalidated instead of the one the service refused — a
// harmless-looking cached token was dropped and the refused one kept.
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

// withoutEntraTokenUse detaches ctx from a caller's recorder (the shared
// listing flight must not record into the chat call that started it).
func withoutEntraTokenUse(ctx context.Context) context.Context {
	return context.WithValue(ctx, entraTokenUseKey{}, (*entraTokenUse)(nil))
}

// withTokenRetry runs fn and, when the service rejected an Entra ID token
// (401 → ErrApiKeyInvalid) that has not expired, drops it and tries once more
// with a fresh one — an issued token can be refused before it expires (a
// signing-key rollover, a gateway validation hiccup); a role assignment or
// secret rotation does NOT invalidate issued tokens (RBAC is evaluated per
// request, and a client-credentials token stays valid until exp). The fresh
// token comes from a rebuilt credential (entraTokenSource.Invalidate), since
// MSAL's own cache would otherwise hand back the refused one; the same token
// coming back means the rejection is real.
//
// Two auth failures return at once: a token that could not be OBTAINED
// (errEntraToken — Entra ID refused the credentials; asking again costs
// another failing sign-in), and a principal the service accepted but denied
// (ErrProviderPermissionDenied — a new token cannot grant a missing role).
//
// fn receives the context to use: it carries this call's token recorder
// (withEntraTokenUse), so the token invalidated is the one fn sent, never one
// a concurrent listing or chat on the same provider fetched meanwhile.
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

// authorize sets the auth headers on a request outside openaiBase (model and
// deployment listings, the Responses API).
func (p *AzureFoundryProvider) authorize(ctx context.Context, req *http.Request) error {
	switch p.cfg.Auth.Mode {
	case AzureAuthModeAPIKey:
		req.Header.Set("api-key", p.cfg.Auth.Key)
		req.Header.Set("Authorization", "Bearer "+p.cfg.Auth.Key)
	case AzureAuthModeEntra:
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

// ProbeAzureFoundryProject explains an empty listing for an Entra ID + project
// document that names no chat deployment (the project listing is its only
// source): the token request's refusal, or the project deployments route's
// answer. nil when the document cannot list anything for a benign reason.
// Models() deliberately never fails for these (a failing listing would make
// the provider list fall back to the catalogue); the connection test calls
// this when it has no candidate to try, so "name a chat deployment" is not
// the answer to a rejected secret, a missing role or a wrong project.
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

// ForgetAzureFoundryCredential releases what the process keeps for a document
// beyond the KeyStore: its Entra ID token source (credential object and
// cached bearer token) and its cached project listing. Called when the
// document is deleted or replaced by one with another credential. Memos keyed
// by resource (served models, learned windows) hold no secret and stay.
func ForgetAzureFoundryCredential(cfg AzureFoundryConfig) {
	if !cfg.Configured() {
		return
	}
	if cfg.Auth.Mode == AzureAuthModeEntra {
		forgetEntraSource(cfg.Auth.TenantID, cfg.Auth.ClientID, cfg.Auth.ClientSecret)
	}
	InvalidateAzureFoundryModels(cfg)
}

// servedMemoResource scopes the served-model memo to this resource (and
// project, whose agents are memoised by name): deployment names are only
// unique within one resource, so after the user switches resources a
// same-named deployment starts from the listing, not the old mapping. Both
// endpoints are already normalised (lower-case, no trailing slash).
func (p *AzureFoundryProvider) servedMemoResource() string {
	if p.cfg.ProjectEndpoint == "" {
		return p.cfg.Endpoint
	}
	return p.cfg.Endpoint + "\x00" + p.cfg.ProjectEndpoint
}

// breakerScope keys circuit-breaker state by the resource (and project) the
// user configured (breakerScoper): a 5xx from one tenant's resource says
// nothing about another tenant's, even when both named a deployment "gpt-4o".
// Endpoints, not the credential fingerprint: users of one resource share its
// health, and the key holds no secret. Both are already normalised
// (lower-case host, no trailing slash); lower-casing again keeps the key
// canonical for documents that bypassed normalisation.
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

// SupportsTools: every Foundry chat model family exposes native function
// calling; models that don't reject the request and the loop degrades.
func (p *AzureFoundryProvider) SupportsTools() bool { return true }

// PricePerMillionTokens is zero on purpose: the deployment name says nothing
// about cost. Usage is priced by the model the response reports serving
// (auditedProvider.record → catalogEntryFor); a served model the catalogue
// does not know is metered at $0 and counted by the pricing-fallback metric.
func (p *AzureFoundryProvider) PricePerMillionTokens() Pricing { return Pricing{} }

// DefaultModel is the configured chat deployment, else the first additional
// deployment, else the first Claude deployment. A configured document that
// names none (Entra ID + project endpoint, whose deployments come from the
// project listing) has no default: "" lets ProviderService pick the first
// listed chat deployment instead of a catalogue name the resource almost
// certainly has not deployed. The unconfigured metadata provider keeps the
// catalogue's reference default.
func (p *AzureFoundryProvider) DefaultModel() string {
	if !p.cfg.Configured() {
		return "gpt-4.1"
	}
	if names := p.cfg.chatDeployments(); len(names) > 0 {
		// The project listing may have re-listed it as a Claude deployment
		// ("claude:<name>"), which /openai/v1 cannot serve.
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

// setEmbeddingModel honours the deployment-wide embedding-model override
// (AI.EmbeddingModel) only when the user's document names no embedding
// deployment: a resource-specific deployment name in the document is the
// more specific setting.
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
	// Claude deployments answer on the Anthropic Messages route, never on
	// /openai/v1 (see azure_claude.go).
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
	// A 401 arrives as the HTTP status, before any chunk, so a retry never
	// replays a partially delivered stream.
	return p.withTokenRetry(ctx, func(ctx context.Context) error { return p.stream(ctx, req, onChunk) })
}

// claudeRoute reports whether model is served on the Anthropic Messages
// route and the "claude:<name>" id to call it by: a "claude:" id, or a bare
// deployment name the cached project listing re-listed as a Claude
// deployment (a document that named it as a chat deployment — the default
// model, or a client that kept the bare id) — /openai/v1 would answer 404.
func (p *AzureFoundryProvider) claudeRoute(model string) (string, bool) {
	if IsClaudeModel(model) {
		return model, true
	}
	return p.relistedClaude(model)
}

// cachedListing is this document's cached project listing while one is live
// (Entra ID + project endpoint). It never fetches: the chat path consults it
// for routing and the sampling policy without paying for a listing.
func (p *AzureFoundryProvider) cachedListing() ([]ModelInfo, bool) {
	if p.configErr != nil || !p.cfg.Configured() || !p.cfg.listsProjectDeployments() {
		return nil, false
	}
	return cachedAzureModels(azureModelsCacheKey(p.cfg))
}

// relistedClaude reports whether the cached listing offers the bare
// deployment name only as a Claude deployment, and the id it lists.
func (p *AzureFoundryProvider) relistedClaude(name string) (string, bool) {
	if name == "" || IsAgentModel(name) || IsClaudeModel(name) {
		return "", false
	}
	models, ok := p.cachedListing()
	if !ok {
		return "", false
	}
	relisted := ""
	for _, m := range models {
		switch {
		case strings.EqualFold(m.ID, name):
			return "", false // still listed as a chat deployment
		case relisted == "" && IsClaudeModel(m.ID) && strings.EqualFold(claudeDeploymentName(m.ID), name):
			relisted = m.ID
		}
	}
	return relisted, relisted != ""
}

// claudeServedModel names the model behind a Claude deployment for the
// sampling policy: the cached project listing's modelName, else the model
// the deployment last answered with on this resource (served-model memo); ""
// when neither knows.
func (p *AzureFoundryProvider) claudeServedModel(deployment string) string {
	id := claudeModelPrefix + deployment
	if models, ok := p.cachedListing(); ok {
		for _, m := range models {
			if strings.EqualFold(m.ID, id) && m.ServedModel != "" {
				return m.ServedModel
			}
		}
	}
	for _, requested := range []string{id, deployment} {
		if served, ok := lookupServedModelForResource(p.servedMemoResource(), azureFoundryProviderID, requested); ok {
			return served
		}
	}
	return ""
}

// ErrEmbeddingDeploymentMissing reports an Azure AI Foundry configuration
// that names no embedding deployment (the field is optional): the provider is
// keyed but cannot embed. ProviderFactory.ForEmbedding returns it (via
// embeddingUnavailable) so the knowledge base's embedder resolution moves on
// and ends in the machine-readable "embedding not configured" error instead
// of a masked 500 at Embed time.
var ErrEmbeddingDeploymentMissing = errors.New("azure ai foundry: no embedding deployment configured — set one under Settings → AI Accounts → Azure AI Foundry")

// embeddingUnavailable satisfies embeddingReadiness (see ForEmbedding). It
// runs after the deployment-wide embedding-model override was applied, so an
// AI.EmbeddingModel override still counts as a deployment.
func (p *AzureFoundryProvider) embeddingUnavailable() error {
	if p.configErr != nil {
		return p.configErr
	}
	if p.embeddingModel == "" {
		return ErrEmbeddingDeploymentMissing
	}
	return nil
}

// azureAdaName matches "ada" as a standalone token of a deployment name
// ("text-embedding-ada-002", "ada002") but not inside a word ("canada").
var azureAdaName = regexp.MustCompile(`(^|[^a-z])ada([^a-z]|$)`)

// azureEmbeddingDimensions decides `dimensions` for an embedding DEPLOYMENT
// name, which need not reveal the model behind it ("kb-embed"). A
// text-embedding-3 name follows the OpenAI rule (never above the native
// width); an ada name gets none (text-embedding-ada-002 has a fixed width);
// any other name gets the configured width, and embed() retries once without
// it when the model turns out to refuse the parameter.
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

// azureModelsCacheTTL bounds how long a COMPLETE project listing (Entra ID +
// project endpoint: the project's deployments and agents) is reused. The chat
// path consults Models() on every turn (context window, output cap, pricing)
// and the factory builds a fresh provider per request, so an instance-level
// memo would not survive; the cache is keyed per credential set AND the
// document's deployment names (azureModelsCacheKey), never shared between
// users or resources. Listings built from the document alone need no network
// and are not cached.
const azureModelsCacheTTL = 5 * time.Minute

// azureModelsRetryAfter is how long a listing cut short by a transient
// project failure (network, 5xx, 429, a hung project host, a token that could
// not be obtained, the extras bound) is served before the project is asked
// again. A deliberate trade: the agents can be missing for up to 30 s after
// such a failure, instead of every chat turn paying the whole listing (up to
// the 10 s extras bound) for as long as the failure lasts.
const azureModelsRetryAfter = 30 * time.Second

const azureModelsCacheMax = 512

// azureModelsFlight collapses concurrent listings of one document (chat
// turns, provider listings, embedding records started at the same time) into
// one pair of project calls; see Models.
var azureModelsFlight singleflight.Group

// azureProjectExtrasTimeout bounds the project listings (deployments and
// agents, every page) as a whole, so a slow or unreachable project endpoint
// costs the chat path at most this much — it only decorates the listing. A
// variable so tests can shorten it.
var azureProjectExtrasTimeout = 10 * time.Second

type azureModelsEntry struct {
	models  []ModelInfo
	expires time.Time
}

var azureModelsCache = struct {
	mu sync.Mutex
	m  map[string]azureModelsEntry
}{m: map[string]azureModelsEntry{}}

// pruneAzureModelsCacheLocked makes room for one new entry WITHOUT wiping the
// map: expired listings go first, and when none are expired the one closest to
// expiry — the least valuable, it would be dropped by the next read anyway —
// mirroring the served-model memo's pruning. The old whole-map wipe also
// discarded every listing the active users were about to read, re-paying the
// project listing (two paginated API calls each) for all of them at once.
// Bounded work: one pass over the map (≤ azureModelsCacheMax entries) per
// insert while full.
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

// azureModelsCacheKey scopes a cached listing to the credential set (which
// includes both endpoints) AND the document's deployment names, which the
// listing is built from: two users of one shared resource and key with
// different deployments must never see each other's listing. Fingerprint()
// itself stays credential-only (it also keys the Entra token sources).
func azureModelsCacheKey(cfg AzureFoundryConfig) string {
	parts := append([]string{cfg.Fingerprint(), cfg.ChatDeployment, cfg.EmbeddingDeployment}, cfg.Deployments...)
	for _, d := range cfg.ClaudeDeployments {
		parts = append(parts, claudeModelPrefix+d)
	}
	return strings.Join(parts, "\x00")
}

// ResetAzureFoundryCaches drops the model-listing cache, the served-model
// memo, the learned context windows, the learned parameter repairs, the
// embedding-dimensions refusals and the Entra token sources (tests). A
// configuration save only invalidates that document's listing
// (InvalidateAzureFoundryModels) and, when the credential changed, its token
// source (ForgetAzureFoundryCredential); the memos need no reset because
// they are keyed by resource (servedMemoResource). In-flight listings
// (azureModelsFlight) hold nothing to clear.
func ResetAzureFoundryCaches() {
	azureModelsCache.mu.Lock()
	azureModelsCache.m = map[string]azureModelsEntry{}
	azureModelsCache.mu.Unlock()
	resetServedModels()
	resetLearnedWindows()
	resetLearnedRepairs()
	resetEmbedDimsRefusals()
	resetEntraSources()
}

// InvalidateAzureFoundryModels drops the cached listing for one document
// (same key as Models uses).
func InvalidateAzureFoundryModels(cfg AzureFoundryConfig) {
	azureModelsCache.mu.Lock()
	delete(azureModelsCache.m, azureModelsCacheKey(cfg))
	azureModelsCache.mu.Unlock()
}

// Models lists what the user can call. No data-plane route lists an API-key
// caller's deployments — GET /openai/v1/models returns the resource's model
// CATALOGUE (hundreds of versioned base and fine-tuned ids such as
// "dall-e-3-3.0"), not deployment names — so the listing is the document's
// own deployments: the chat deployment, the additional deployments and the
// embedding deployment (flagged Embedding), each sized and priced from the
// catalogue when its name is a catalogued model (the portal names deployments
// after their model by default). With Entra ID and a project endpoint the
// project's deployments listing is authoritative on top of that — it names
// the model behind each deployment and adds the ones the document does not —
// followed by the project's agents. Nothing named and no project yields an
// empty list. Unconfigured (metadata) providers return the static reference
// catalogue without touching the network. A bad key is reported by the
// connection test's chat call (401 → ErrApiKeyInvalid), not by the listing.
func (p *AzureFoundryProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	if !p.cfg.Configured() {
		return catalogModels(azureFoundryProviderID), nil
	}
	models := p.configuredDeployments()
	if !p.cfg.listsProjectDeployments() {
		return models, nil
	}

	key := azureModelsCacheKey(p.cfg)
	if cached, ok := cachedAzureModels(key); ok {
		return cached, nil
	}

	// One listing per document at a time: callers that arrive while it runs
	// share the result instead of each running the two paginated project
	// calls. The flight is detached from the first caller's cancellation (its
	// result fills the cache for everyone; projectListing bounds itself — the
	// token fetch 30 s, the extras azureProjectExtrasTimeout) and each caller
	// waits under its own ctx, falling back to the document's deployments
	// when that ends first.
	//
	// A complete listing is cached for azureModelsCacheTTL; one cut short by
	// a transient project failure, a token failure or the extras bound is
	// cached for azureModelsRetryAfter only — served (the document's
	// deployments keep chat working) but asked again soon instead of hiding
	// the agents for the full TTL, and never re-fetched on every turn. An
	// empty listing is not cached.
	ch := azureModelsFlight.DoChan(key, func() (any, error) {
		listed, complete := p.projectListing(withoutEntraTokenUse(context.WithoutCancel(ctx)), p.configuredDeployments())
		if len(listed) > 0 {
			ttl := azureModelsCacheTTL
			if !complete {
				ttl = azureModelsRetryAfter
			}
			storeAzureModels(key, listed, ttl)
		}
		return listed, nil
	})
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

// cachedAzureModels returns a copy of the cached listing for key while it is
// live.
func cachedAzureModels(key string) ([]ModelInfo, bool) {
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	e, ok := azureModelsCache.m[key]
	if !ok || !time.Now().Before(e.expires) {
		return nil, false
	}
	out := make([]ModelInfo, len(e.models))
	copy(out, e.models)
	return out, true
}

// storeAzureModels caches a listing for ttl, making room first when the
// cache is full.
func storeAzureModels(key string, models []ModelInfo, ttl time.Duration) {
	now := time.Now()
	azureModelsCache.mu.Lock()
	defer azureModelsCache.mu.Unlock()
	if len(azureModelsCache.m) >= azureModelsCacheMax {
		pruneAzureModelsCacheLocked(now)
	}
	azureModelsCache.m[key] = azureModelsEntry{models: models, expires: now.Add(ttl)}
}

// projectListing runs the project extras (Entra ID + project endpoint) and
// reports whether the result is complete. The token is obtained first under
// the caller's full context: the token fetch is shared (singleflight) with
// concurrent chat turns, which must not inherit the extras' short bound, and a
// token that cannot be obtained skips the extras instead of being requested
// again for every project call.
func (p *AzureFoundryProvider) projectListing(ctx context.Context, models []ModelInfo) ([]ModelInfo, bool) {
	if _, err := p.entraToken(ctx); err != nil {
		logger.Warn("azure ai foundry: entra id token unavailable; listing the configured deployments only", "error", err)
		return models, false
	}
	ectx, cancel := context.WithTimeout(ctx, azureProjectExtrasTimeout)
	defer cancel()
	return p.projectExtras(ectx, models)
}

// configuredDeployments is the listing the document itself names: the chat
// deployment, the additional deployments, the Claude deployments, then the
// embedding deployment.
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

// deploymentEntry is the listing entry for one deployment name. When the
// name is itself a catalogued model — the portal's default deployment names
// ("gpt-4o", "Phi-4", "text-embedding-3-small") — the entry carries that
// model's price, context window and output cap up front, so a small-window
// deployment is sized correctly before any call succeeded. A model-router
// name keeps the conservative default window (each call may be routed to a
// different model). Anything else starts at the provider-wide window and is
// enriched later from the project listing or the served-model memo.
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
