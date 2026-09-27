package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

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
// Wire format: {endpoint}/openai/v1/chat/completions, /embeddings and /models
// speak the OpenAI shape, so openaiBase does the work; the differences are the
// per-instance base URL, the `api-key` header, max_completion_tokens, and
// Azure's content-filter and retry-after-ms semantics (handled in the base
// so every OpenAI-compatible provider benefits).
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

	mu        sync.Mutex
	lastToken string
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
	}
	return p
}

// entraToken is the openaiBase tokenFn: it also remembers the token used so a
// 401 can invalidate exactly that one.
func (p *AzureFoundryProvider) entraToken(ctx context.Context) (string, error) {
	tok, err := p.tokens.Token(ctx)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.lastToken = tok
	p.mu.Unlock()
	return tok, nil
}

// withTokenRetry runs fn and, when the service rejected an Entra ID token
// (401 → ErrApiKeyInvalid) that has not expired, drops it and tries once more
// with a fresh one — a role assignment or secret rotation invalidates tokens
// out of band. The same token coming back means the rejection is real.
func (p *AzureFoundryProvider) withTokenRetry(ctx context.Context, fn func() error) error {
	err := fn()
	if err == nil || p.tokens == nil || !errors.Is(err, ErrApiKeyInvalid) {
		return err
	}
	p.mu.Lock()
	failed := p.lastToken
	p.mu.Unlock()
	p.tokens.Invalidate(failed)
	fresh, ferr := p.tokens.Token(ctx)
	if ferr != nil || fresh == failed {
		return err
	}
	logger.Warn("azure ai foundry rejected a cached entra id token; retrying with a fresh one")
	return fn()
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

// DefaultModel is the configured chat deployment, else the catalogue's
// reference default (only meaningful for the unconfigured metadata provider —
// ProviderService substitutes the first real deployment for the UI).
func (p *AzureFoundryProvider) DefaultModel() string {
	if p.cfg.ChatDeployment != "" {
		return p.cfg.ChatDeployment
	}
	return "gpt-4.1"
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
	if IsAgentModel(req.Model) {
		var resp *Response
		err := p.withTokenRetry(ctx, func() error {
			var err error
			resp, err = p.agentChat(ctx, req)
			return err
		})
		return resp, err
	}
	var resp *Response
	err := p.withTokenRetry(ctx, func() error {
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
	if IsAgentModel(req.Model) {
		return p.withTokenRetry(ctx, func() error { return p.agentStream(ctx, req, onChunk) })
	}
	// A 401 arrives as the HTTP status, before any chunk, so a retry never
	// replays a partially delivered stream.
	return p.withTokenRetry(ctx, func() error { return p.stream(ctx, req, onChunk) })
}

func (p *AzureFoundryProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	if p.embeddingModel == "" {
		return nil, errors.New("azure ai foundry: no embedding deployment configured — set one under Settings → AI Accounts → Azure AI Foundry")
	}
	var vecs [][]float32
	err := p.withTokenRetry(ctx, func() error {
		var err error
		vecs, err = p.embed(ctx, text)
		return err
	})
	return vecs, err
}

// --- model listing ---

// azureModelsCacheTTL bounds how long a listing is reused per credential
// set. The chat path consults Models() on every turn (context window,
// output cap, pricing) and the factory builds a fresh provider per request,
// so an instance-level memo would not survive; the cache is keyed by the
// credential fingerprint, never shared between users or resources.
const azureModelsCacheTTL = 5 * time.Minute

const azureModelsCacheMax = 512

type azureModelsEntry struct {
	models  []ModelInfo
	expires time.Time
}

var azureModelsCache = struct {
	mu sync.Mutex
	m  map[string]azureModelsEntry
}{m: map[string]azureModelsEntry{}}

// ResetAzureFoundryCaches drops the model-listing cache, the served-model memo
// and the Entra token sources (tests, and after a configuration save so a
// new deployment shows up without waiting for the TTL).
func ResetAzureFoundryCaches() {
	azureModelsCache.mu.Lock()
	azureModelsCache.m = map[string]azureModelsEntry{}
	azureModelsCache.mu.Unlock()
	resetServedModels()
	resetEntraSources()
}

// InvalidateAzureFoundryModels drops the cached listing for one credential set.
func InvalidateAzureFoundryModels(cfg AzureFoundryConfig) {
	azureModelsCache.mu.Lock()
	delete(azureModelsCache.m, cfg.Fingerprint())
	azureModelsCache.mu.Unlock()
}

// Models lists what the resource can serve: the deployments from
// GET /openai/v1/models (their ids are the `model` values requests must
// use), annotated with the underlying model when the project deployments
// listing is reachable, followed by the project's agents (Entra ID only).
// Unconfigured (metadata) providers return the static reference catalogue
// without touching the network. A listing failure other than bad
// credentials falls back to the deployments named in the document so chat
// keeps working; bad credentials are reported so the connection test can say
// "invalid API key".
func (p *AzureFoundryProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	if !p.cfg.Configured() {
		return catalogModels(azureFoundryProviderID), nil
	}
	key := p.cfg.Fingerprint()
	azureModelsCache.mu.Lock()
	if e, ok := azureModelsCache.m[key]; ok && time.Now().Before(e.expires) {
		out := make([]ModelInfo, len(e.models))
		copy(out, e.models)
		azureModelsCache.mu.Unlock()
		return out, nil
	}
	azureModelsCache.mu.Unlock()

	var models []ModelInfo
	err := p.withTokenRetry(ctx, func() error {
		var err error
		models, err = p.fetchDeployments(ctx)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, ErrRateLimited) {
			return nil, err
		}
		logger.Warn("azure ai foundry: list deployments failed; using the configured deployment names", "error", err)
		models = p.configuredDeployments()
	}
	models = p.projectExtras(ctx, models)
	if len(models) == 0 {
		models = p.configuredDeployments()
	}

	// An empty listing (listing failed and the document names no
	// deployment) is not cached: the next call retries instead of pinning
	// "no models" for the TTL after a transient outage.
	if len(models) > 0 {
		azureModelsCache.mu.Lock()
		if len(azureModelsCache.m) >= azureModelsCacheMax {
			azureModelsCache.m = map[string]azureModelsEntry{}
		}
		azureModelsCache.m[key] = azureModelsEntry{models: models, expires: time.Now().Add(azureModelsCacheTTL)}
		azureModelsCache.mu.Unlock()
	}

	out := make([]ModelInfo, len(models))
	copy(out, models)
	return out, nil
}

// configuredDeployments is the listing when the resource cannot be asked:
// whatever the document names.
func (p *AzureFoundryProvider) configuredDeployments() []ModelInfo {
	var out []ModelInfo
	for _, d := range []string{p.cfg.ChatDeployment, p.cfg.EmbeddingDeployment} {
		if d != "" {
			out = append(out, ModelInfo{ID: d, DisplayName: d, ContextLimit: p.ContextLimit()})
		}
	}
	return out
}

// fetchDeployments calls GET {endpoint}/openai/v1/models. The response is the
// OpenAI list shape whose ids are the resource's deployment names.
func (p *AzureFoundryProvider) fetchDeployments(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.Endpoint+"/openai/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if err := p.authorize(ctx, req); err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azure ai foundry: list deployments: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrApiKeyInvalid
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, rateLimitErr(resp)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("azure ai foundry: list deployments: status %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("azure ai foundry: list deployments: %w", err)
	}
	out := make([]ModelInfo, 0, len(parsed.Data))
	seen := map[string]bool{}
	for _, d := range parsed.Data {
		id := strings.TrimSpace(d.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ModelInfo{ID: id, DisplayName: id, ContextLimit: p.ContextLimit()})
	}
	return out, nil
}
