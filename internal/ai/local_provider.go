package ai

// The local models provider (audit #9 F-50): a locally hosted,
// OpenAI-compatible server — Foundry Local, Ollama, LM Studio, llama.cpp —
// for the desktop app's offline / confidential-flow story. One generic
// provider instead of a Foundry-Local-specific one (that REST API is
// preview and may change without notice); every one of these servers
// speaks the OpenAI v1 wire format.
//
// The stored "key" is a small JSON document (ProviderFactory.For refuses
// empty keys, the same trick the azure-foundry provider uses): the base URL
// and an optional context window. The chat model is picked per request from
// the server's own listing; the provider is not embedding-capable.
//
// Egress: the provider is constructible only when the runtime policy is
// egress.AllowLoopback (the desktop/local single-user app; see
// ConfigureLocalModels), and the base URL must be a LOOPBACK http(s) URL —
// a cloud deployment's users must not point a "credential" at the server's
// own loopback services, and the desktop user's own machine is the only
// place these servers run anyway.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"pad-analyzer/internal/egress"
	"pad-core/logger"
)

const (
	localProviderID = "local"
	// LocalProviderID is the provider id (KeyStore key, request field).
	LocalProviderID = localProviderID

	// localConfigMaxBytes bounds the stored document (see azureConfigMaxBytes).
	localConfigMaxBytes = 2048

	// LocalDefaultContextWindow is the context window, in tokens, assumed
	// when the document names none. Ollama and LM Studio run a model with a
	// 2k–8k context by default whatever the model itself supports, and a
	// server silently drops the start of a prompt that does not fit — so
	// history is trimmed against the window the server really runs, not the
	// model's advertised maximum.
	LocalDefaultContextWindow = 8192
	// localMinContextWindow and localMaxContextWindow bound a configured
	// window: no chat turn fits in less, and no local model runs more.
	localMinContextWindow = 1024
	localMaxContextWindow = 2000000
)

// LocalConfig is the stored per-user document.
type LocalConfig struct {
	// BaseURL is the server's root, e.g. http://127.0.0.1:11434 (Ollama),
	// http://127.0.0.1:1234 (LM Studio), http://127.0.0.1:5272 (Foundry
	// Local) — no path; /v1/... is appended per call.
	BaseURL string `json:"baseUrl"`
	// ContextWindow (optional) is the context length, in tokens, the server
	// runs its models with (Ollama num_ctx, LM Studio's context length);
	// 0 = LocalDefaultContextWindow. Non-zero values are within
	// localMinContextWindow..localMaxContextWindow.
	ContextWindow int `json:"contextWindow,omitempty"`
}

// Configured reports whether the document names a base URL.
func (c LocalConfig) Configured() bool { return c.BaseURL != "" }

// EffectiveContextWindow is the window history is trimmed against: the
// configured one, else LocalDefaultContextWindow.
func (c LocalConfig) EffectiveContextWindow() int {
	if c.ContextWindow == 0 {
		return LocalDefaultContextWindow
	}
	return c.ContextWindow
}

// validLocalContextWindow reports whether n is a storable window: 0 (the
// default) or within localMinContextWindow..localMaxContextWindow.
func validLocalContextWindow(n int) bool {
	return n == 0 || (n >= localMinContextWindow && n <= localMaxContextWindow)
}

// Encode serialises the document for the KeyStore, enforcing the size cap.
func (c LocalConfig) Encode() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode local models configuration: %w", err)
	}
	if len(raw) > localConfigMaxBytes {
		return "", fmt.Errorf("local models configuration exceeds %d bytes", localConfigMaxBytes)
	}
	return string(raw), nil
}

// ParseLocalConfig decodes the stored document. An empty string is a valid,
// unconfigured config; unknown fields are rejected like azure-foundry's. A
// stored context window outside the valid range reads as the default (with a
// warning) instead of making the provider unusable.
func ParseLocalConfig(raw string) (LocalConfig, error) {
	if strings.TrimSpace(raw) == "" {
		return LocalConfig{}, nil
	}
	if len(raw) > localConfigMaxBytes {
		return LocalConfig{}, fmt.Errorf("local models configuration exceeds %d bytes", localConfigMaxBytes)
	}
	// The first draft of the document declared a chat and an embedding model
	// id that no save ever wrote; the keys stay accepted (and ignored), so a
	// document carrying them cannot fail DisallowUnknownFields.
	var doc struct {
		LocalConfig
		LegacyModel          json.RawMessage `json:"model,omitempty"`
		LegacyEmbeddingModel json.RawMessage `json:"embeddingModel,omitempty"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return LocalConfig{}, fmt.Errorf("local models configuration is not valid JSON: %w", err)
	}
	cfg := doc.LocalConfig
	cfg.normalize()
	if !validLocalContextWindow(cfg.ContextWindow) {
		logger.Warn("local models: stored context window out of range, using the default",
			"contextWindow", cfg.ContextWindow, "default", LocalDefaultContextWindow)
		cfg.ContextWindow = 0
	}
	return cfg, nil
}

func (c *LocalConfig) normalize() {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	// Servers advertise their OpenAI endpoint as ".../v1" (Ollama, LM Studio,
	// Foundry Local); the provider appends /v1/... itself, so a pasted "/v1"
	// root is accepted and dropped instead of failing the no-path rule.
	if n := len(c.BaseURL); n >= 3 && strings.EqualFold(c.BaseURL[n-3:], "/v1") {
		c.BaseURL = strings.TrimRight(c.BaseURL[:n-3], "/")
	}
	// Users type "localhost"; the validator only accepts IP literals (a name
	// could resolve elsewhere), so the literal name is pinned to IPv4 loopback.
	if i := strings.Index(c.BaseURL, "://"); i > 0 {
		rest := c.BaseURL[i+3:]
		if len(rest) >= 9 && strings.EqualFold(rest[:9], "localhost") && (len(rest) == 9 || rest[9] == ':' || rest[9] == '/') {
			c.BaseURL = c.BaseURL[:i+3] + "127.0.0.1" + rest[9:]
		}
	}
}

// Validate checks the document's base URL: loopback-only http(s) (the
// deliberate, explicit local/offline opt-in — the desktop user's own
// machine, never a cloud server's loopback). The context window is checked
// by NewLocalConfigForSave (its own client code) and ParseLocalConfig.
// Deliberately separate from validateAzureEndpoint: the Azure allow-list
// must stay https-only and Microsoft-suffixed.
func (c LocalConfig) Validate() error {
	if !c.Configured() {
		return fmt.Errorf("the local models base URL is required")
	}
	u, err := parseStrictURL(c.BaseURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("the local models base URL must be an absolute http:// or https:// URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("the local models base URL must use http:// or https://")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("the local models base URL must not carry credentials, a query string or a fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("the local models base URL must be the server root, without a path (http://127.0.0.1:11434)")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("the local models base URL must be a loopback address (http://127.0.0.1:<port> or http://[::1]:<port>): local servers run on your own machine")
	}
	return nil
}

// parseStrictURL is url.Parse rejecting control characters (net/http
// rejects them later anyway; a clear error beats a transport one).
func parseStrictURL(raw string) (*url.URL, error) {
	if strings.ContainsAny(raw, " \t\r\n") {
		return nil, fmt.Errorf("whitespace in URL")
	}
	return url.Parse(raw)
}

// localModelsRuntime holds the operator policy (ConfigureLocalModels, DI at
// boot). Unconfigured DISABLES the provider: the zero egress.Policy is
// BlockInternal, so an entrypoint that never wires DI fails closed like a
// cloud server — unlike the Azure runtime, whose unconfigured client behaves
// like local mode. Only the desktop/local wiring's AllowLoopback enables it.
var localModelsRuntime struct {
	mu     sync.RWMutex
	egress egress.Policy
}

// LocalModelsOptions is the operator-level configuration.
type LocalModelsOptions struct {
	// Egress is the outbound policy. The provider is constructible only
	// under egress.AllowLoopback: a cloud deployment must not let users
	// point a "credential" at the server's own loopback services.
	Egress egress.Policy
}

// ConfigureLocalModels installs the operator policy (DI at boot).
func ConfigureLocalModels(opts LocalModelsOptions) {
	localModelsRuntime.mu.Lock()
	localModelsRuntime.egress = opts.Egress
	localModelsRuntime.mu.Unlock()
}

func localModelsAllowed() bool {
	localModelsRuntime.mu.RLock()
	defer localModelsRuntime.mu.RUnlock()
	return localModelsRuntime.egress == egress.AllowLoopback
}

// LocalModelsAvailable reports whether this process may use the local models
// provider at all (the desktop/local egress policy; false in cloud). The
// provider list hides the provider and the configuration routes refuse
// reads and saves when it is false.
func LocalModelsAvailable() bool { return localModelsAllowed() }

// CheckLocalModelsAvailable is nil where LocalModelsAvailable, else the
// *LocalConfigError (LocalCfgErrUnavailable) both configuration routes
// answer.
func CheckLocalModelsAvailable() error {
	if localModelsAllowed() {
		return nil
	}
	return &LocalConfigError{Code: LocalCfgErrUnavailable, Message: "local models are available in the desktop app only"}
}

// Client codes of a refused local models configuration read or save.
const (
	// LocalCfgErrInvalidBaseURL: the base URL is not an absolute loopback
	// http(s) server root.
	LocalCfgErrInvalidBaseURL = "INVALID_BASE_URL"
	// LocalCfgErrInvalidContextWindow: the context window is neither 0 (the
	// default) nor within 1024..2000000 tokens.
	LocalCfgErrInvalidContextWindow = "INVALID_CONTEXT_WINDOW"
	// LocalCfgErrUnavailable: the runtime egress policy is not AllowLoopback.
	LocalCfgErrUnavailable = "LOCAL_MODELS_UNAVAILABLE"
)

// LocalConfigError is a refused local models configuration save; Code is one
// of the LocalCfgErr* client codes.
type LocalConfigError struct {
	Code    string
	Message string
}

func (e *LocalConfigError) Error() string { return e.Message }

// LocalPublicConfig is the view GET /api/providers/local/config returns.
type LocalPublicConfig struct {
	Configured bool   `json:"configured"`
	BaseURL    string `json:"baseUrl"`
	// ContextWindow is the STORED window in tokens, omitted when unset (the
	// provider then uses LocalDefaultContextWindow, 8192).
	ContextWindow int `json:"contextWindow,omitempty"`
}

// Public is the client view of the stored document.
func (c LocalConfig) Public() LocalPublicConfig {
	return LocalPublicConfig{Configured: c.Configured(), BaseURL: c.BaseURL, ContextWindow: c.ContextWindow}
}

// NewLocalConfigForSave builds the document a save stores from the client's
// base URL and context window (0 = the default): normalised, and refused
// (with a *LocalConfigError) when local models are unavailable in this
// deployment, the URL is not a loopback http(s) server root, or the window
// is out of range.
func NewLocalConfigForSave(baseURL string, contextWindow int) (LocalConfig, error) {
	if err := CheckLocalModelsAvailable(); err != nil {
		return LocalConfig{}, err
	}
	cfg := LocalConfig{BaseURL: baseURL, ContextWindow: contextWindow}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return LocalConfig{}, &LocalConfigError{Code: LocalCfgErrInvalidBaseURL, Message: err.Error()}
	}
	if !validLocalContextWindow(contextWindow) {
		return LocalConfig{}, &LocalConfigError{Code: LocalCfgErrInvalidContextWindow, Message: fmt.Sprintf(
			"the context window must be a whole number of tokens between %d and %d (leave it empty for %d)",
			localMinContextWindow, localMaxContextWindow, LocalDefaultContextWindow)}
	}
	return cfg, nil
}

// LocalProvider serves one locally hosted OpenAI-compatible server.
type LocalProvider struct {
	openaiBase
	cfg       LocalConfig
	configErr error
}

// NewLocalProvider builds the provider from the stored document.
func NewLocalProvider(raw string) *LocalProvider {
	cfg, err := ParseLocalConfig(raw)
	if err == nil && cfg.Configured() {
		err = cfg.Validate()
	}
	p := &LocalProvider{cfg: cfg}
	if err != nil {
		p.configErr = fmt.Errorf("%w: %w", ErrProviderConfigInvalid, err)
	}
	if !localModelsAllowed() {
		p.configErr = fmt.Errorf("%w: local models are available in the desktop app only — this deployment's egress policy does not allow loopback connections", ErrProviderConfigInvalid)
	}
	base := cfg.BaseURL + "/v1/chat/completions"
	// No embedding model: the provider is not embedding-capable
	// (SupportsEmbeddings), so Embed answers "not supported".
	p.openaiBase = openaiBase{
		client:         localModelsClient(),
		baseURL:        &base,
		providerLabel:  "local models",
		maxTokensParam: maxTokensParamLegacy,
	}
	return p
}

// localModelsClient is the local server client: the shared transport's
// pool/proxy settings with the raised header timeout (a laptop-class model
// can think for minutes before the first header on the non-streaming path)
// and redirects refused, like the Azure client. Built once: the factory
// builds a provider per request, and a transport cloned per provider meant a
// fresh connection pool — and a fresh dial to the local server — per turn.
var localModelsClient = sync.OnceValue(func() *http.Client {
	return &http.Client{Timeout: 0, Transport: azureTransport(sharedTransportBase()), CheckRedirect: egress.NoRedirects}
})

func (p *LocalProvider) ID() string   { return localProviderID }
func (p *LocalProvider) Name() string { return "Local models" }

// ContextLimit is the configured window (LocalDefaultContextWindow when
// unset), never the model's advertised maximum: see LocalDefaultContextWindow.
func (p *LocalProvider) ContextLimit() int { return p.cfg.EffectiveContextWindow() }
func (p *LocalProvider) FreeModel() string { return "" }

// SupportsTools: most local servers implement function calling; one that
// does not rejects the request and the tool loop degrades.
func (p *LocalProvider) SupportsTools() bool { return true }

// PricePerMillionTokens is zero on purpose: local models cost nothing to
// run, so the budget meter stays at $0 (Pricing{}).
func (p *LocalProvider) PricePerMillionTokens() Pricing { return Pricing{} }

// DefaultModel is empty: the document names no model, and the listing's
// first chat model is the default (chooseDefaultModel).
func (p *LocalProvider) DefaultModel() string { return "" }

func (p *LocalProvider) EstimateTokens(text string) int { return EstimateTokensOpenAI(text) }

// Models lists the server's chat models from GET {base}/v1/models — local
// servers expose their own ids, so there is no catalogue fallback. Every
// entry carries the configured context window.
func (p *LocalProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	if !p.cfg.Configured() {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("local models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("local models: model listing failed (status %d)", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBodyBytes)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse local models listing: %w", err)
	}
	out := make([]ModelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		// Embedding models (nomic-embed-text, mxbai-embed-large,
		// text-embedding-*) share the listing but cannot chat: offered in the
		// picker, or picked as the default, every turn on one fails.
		if m.ID == "" || strings.Contains(strings.ToLower(m.ID), "embed") {
			continue
		}
		out = append(out, ModelInfo{ID: m.ID, DisplayName: m.ID, ContextLimit: p.ContextLimit()})
	}
	return out, nil
}

func (p *LocalProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	return p.chat(ctx, req)
}

func (p *LocalProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	if p.configErr != nil {
		return p.configErr
	}
	return p.stream(ctx, req, onChunk)
}

func (p *LocalProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	if p.configErr != nil {
		return nil, p.configErr
	}
	return p.embed(ctx, text)
}

func init() {
	// Registered here so factory.go keeps a single registry; the desktop
	// entrypoint (DI) decides through ConfigureLocalModels whether the
	// provider is usable at all.
	RegisterProvider(localProviderID, func(key string) Provider { return NewLocalProvider(key) })
}
