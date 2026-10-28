package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/service"
	"pad-core/models"
)

type ProviderHandler struct {
	providerSvc *service.ProviderService
	security    *SecurityConfig
	// audit receives the rows for configuration changes (records nothing on the desktop).
	audit *audit.Recorder
}

func NewProviderHandler(providerSvc *service.ProviderService, security *SecurityConfig, recorder *audit.Recorder) *ProviderHandler {
	return &ProviderHandler{providerSvc: providerSvc, security: security, audit: recorder}
}

// @Summary      List AI providers
// @Description  Returns a list of supported AI providers and their current status. With orgId, a provider the caller has not configured but that organisation has (a shared credential) is listed as configured and shared; the parameter is ignored unless the caller is an admin or member of that org (viewers, guests and non-members need a credential of their own).
// @Tags         providers
// @Param        orgId query string false "Organisation whose shared credentials to include"
// @Produce      json
// @Success      200 {object} []models.ProviderInfo "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/list [get]
func (h *ProviderHandler) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.providerSvc.ListProviders(r.Context(), h.security.KeyScope(r), h.listingOrg(r))
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, providers)
}

// @Summary      Provider catalogue
// @Description  Lists every registered AI provider of this deployment's profile in display order, configured or not — the generic settings panel renders from it (ADR 0025). Each entry carries id, name, authType (api_key|oauth), modes[] (the deployment profiles it is offered in), caps[] (chat, stream, embeddings, tools, reasoning, model_aliases, live_listing, org_shared, prompt_cache), credential {kind: api_key|oauth_device|document|none, fields[] {name, label, type: text|secret|url|select|number|json, required, placeholder?, help?, options? [{value, label}]}, helpUrl?}, endpoints {save?, test?, start?, poll?, revoke?, user?} (the routes that manage the credential), configured (the caller holds a credential, or — with orgId, as /api/providers/list — their organisation shares one), color?, icon?, pricing (catalog|free|unpriced) and configurable (the credential can be managed through endpoints; false for a document provider without a panel: "configured on the server"). Network-free: no live model listing.
// @Tags         providers
// @Param        orgId query string false "Organisation whose shared credentials count as configured"
// @Produce      json
// @Success      200 {object} providerCatalogOut "OK"
// @Failure      500 {object} render.ErrorResponse "Internal Server Error"
// @Router       /api/providers/catalog [get]
func (h *ProviderHandler) providerCatalog(ctx context.Context, req providerCatalogRequest) (providerCatalogOut, error) {
	entries, err := h.providerSvc.ProviderCatalog(ctx, h.security.KeyScopeFromContext(ctx), h.listingOrgFor(ctx, req.OrgID))
	if err != nil {
		return providerCatalogOut{}, err
	}
	if entries == nil {
		entries = []models.ProviderCatalogEntry{}
	}
	return providerCatalogOut{Providers: entries}, nil
}

type providerCatalogRequest struct {
	OrgID string `query:"orgId"`
}

type providerCatalogOut struct {
	Providers []models.ProviderCatalogEntry `json:"providers"`
}

// listingOrg returns the ?orgId= the caller may list shared credentials for: the parameter when the
// caller's role may run on the org's shared credential (admin or member — the rule the chat turn
// applies), else "" — a non-member learns nothing.
func (h *ProviderHandler) listingOrg(r *http.Request) string {
	return h.listingOrgFor(r.Context(), r.URL.Query().Get("orgId"))
}

// listingOrgFor is listingOrg for a typed endpoint.
func (h *ProviderHandler) listingOrgFor(ctx context.Context, orgID string) string {
	if orgID == "" || h.security.OrgSvc == nil {
		return ""
	}
	role, err := h.security.OrgSvc.MemberRole(ctx, orgID, h.security.CallerIDFromContext(ctx))
	if err != nil || !service.OrgRoleUsesSharedCredential(role) {
		return ""
	}
	return orgID
}

// @Summary      Test provider connection
// @Description  Tests the connection to a specific AI provider with one tiny billed chat call per candidate model. Every outcome is a 200 {ok, latencyMs, error?, code?}; code names a refusal the client branches on: PROVIDER_UNKNOWN (no such provider), PROVIDER_NOT_ALLOWED (the provider policy refused it: disabled in this deployment — this route tests the caller's own credential, so the organisation's allowed-provider list applies to the org test route, POST /api/orgs/{id}/providers/azure-foundry/test, instead). For azure-foundry this form is deprecated (one release): it tests the caller's resource "default" (else their first resource) as a resource — an authenticated probe, no model call; use POST /api/providers/azure-foundry/test to test one resource or model.
// @Tags         providers
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/test [post]
func (h *ProviderHandler) handleTestProviderConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := h.providerSvc.TestProviderConnection(r.Context(), h.security.KeyScope(r), req.Provider)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

// @Summary      Start GitHub OAuth
// @Description  Initiates the GitHub device flow authentication.
// @Tags         providers
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/github/start [post]
func (h *ProviderHandler) handleStartGitHubAuth(w http.ResponseWriter, r *http.Request) {
	res, err := h.providerSvc.StartGitHubAuth(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

// @Summary      Poll GitHub OAuth
// @Description  Polls GitHub for the status of the device flow authentication.
// @Tags         providers
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} render.ErrorResponse "Internal Server Error; DEVICE_TOKEN_NOT_STORED when GitHub granted the token but the KeyStore refused it"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE: GitHub granted the token but the secret service is unreachable"
// @Router       /api/providers/github/poll [post]
func (h *ProviderHandler) handlePollGitHubAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"deviceCode"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := h.providerSvc.PollGitHubAuth(r.Context(), h.security.KeyScope(r), req.DeviceCode)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

// @Summary      Revoke GitHub OAuth
// @Description  Revokes the GitHub authentication for the current user.
// @Tags         providers
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE: the secret store cannot be reached (an OS keychain that is down); the token was not removed"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/github/revoke [post]
func (h *ProviderHandler) handleRevokeGitHubAuth(w http.ResponseWriter, r *http.Request) {
	if err := h.providerSvc.RevokeGitHubAuth(h.security.KeyScope(r)); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Get GitHub user info
// @Description  Returns information about the currently authenticated GitHub user.
// @Tags         providers
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      422 {object} render.ErrorResponse "KEY_UNREADABLE: a token is stored but the server cannot decrypt it (its encryption key changed); sign in again"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/github/user [get]
func (h *ProviderHandler) handleGetGitHubUser(w http.ResponseWriter, r *http.Request) {
	user, err := h.providerSvc.GetGitHubUser(r.Context(), h.security.KeyScope(r))
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, user)
}

// @Summary      Start Copilot OAuth
// @Description  Initiates the GitHub Copilot device flow authentication.
// @Tags         providers
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/copilot/start [post]
func (h *ProviderHandler) handleStartCopilotAuth(w http.ResponseWriter, r *http.Request) {
	res, err := h.providerSvc.StartCopilotAuth(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

// @Summary      Poll Copilot OAuth
// @Description  Polls GitHub for the status of the Copilot device flow authentication.
// @Tags         providers
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} render.ErrorResponse "Internal Server Error; DEVICE_TOKEN_NOT_STORED when GitHub granted the token but the KeyStore refused it"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE: GitHub granted the token but the secret service is unreachable"
// @Router       /api/providers/copilot/poll [post]
func (h *ProviderHandler) handlePollCopilotAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"deviceCode"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := h.providerSvc.PollCopilotAuth(r.Context(), h.security.KeyScope(r), req.DeviceCode)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

// @Summary      Revoke Copilot OAuth
// @Description  Revokes the GitHub Copilot authentication for the current user.
// @Tags         providers
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE: the secret store cannot be reached (an OS keychain that is down); the token was not removed"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/copilot/revoke [post]
func (h *ProviderHandler) handleRevokeCopilotAuth(w http.ResponseWriter, r *http.Request) {
	if err := h.providerSvc.RevokeCopilotAuth(h.security.KeyScope(r)); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Get Copilot user info
// @Description  Returns information about the currently authenticated Copilot user.
// @Tags         providers
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      422 {object} render.ErrorResponse "KEY_UNREADABLE: a token is stored but the server cannot decrypt it (its encryption key changed); sign in again"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/copilot/user [get]
func (h *ProviderHandler) handleGetCopilotUser(w http.ResponseWriter, r *http.Request) {
	user, err := h.providerSvc.GetCopilotUser(r.Context(), h.security.KeyScope(r))
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, user)
}

// @Summary      Get Azure AI Foundry configuration
// @Description  Deprecated (one release): use GET /api/providers/azure-foundry/connections and /models. Returns the caller's resource "default" — the single-document configuration it migrated to — without any secret: endpoint, auth mode, project endpoint and the deployment names, built from the registry's entries of that resource (never from the stored document's old fields). {configured:false} when there is none.
// @Tags         providers
// @Deprecated
// @Produce      json
// @Success      200 {object} ai.AzureFoundryPublicConfig "OK"
// @Failure      500 {object} render.ErrorResponse "Internal Server Error"
// @Router       /api/providers/azure-foundry/config [get]
func (h *ProviderHandler) handleGetAzureFoundryConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.providerSvc.GetAzureFoundryConfig(h.security.KeyScope(r))
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	render.JSON(w, cfg)
}

// @Summary      Save Azure AI Foundry configuration
// @Description  Deprecated (one release): use POST/PUT /api/providers/azure-foundry/connections and /models. Validates and stores the caller's Azure AI Foundry endpoint, credential (API key or Microsoft Entra ID service principal), optional project endpoint and deployment names as their resource "default": the credential part is saved (the stored document's old deployment fields carried over unchanged, for a rolled-back binary) and the deployment fields are mapped onto that resource's models — missing ones added, removed ones deleted. An omitted secret keeps the stored one while the endpoint host is unchanged. The body must be exactly one JSON object with known fields only (INVALID_CONFIG otherwise).
// @Tags         providers
// @Deprecated
// @Param        request body ai.AzureFoundryConfig true "The configuration"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_ENDPOINT, INVALID_PROJECT_ENDPOINT, INVALID_AUTH, MISSING_SECRET, MISSING_DEPLOYMENT, INVALID_DEPLOYMENT, CONFIG_TOO_LARGE or HOST_NOT_ALLOWED"
// @Failure      409 {object} render.ErrorResponse "The resource or model limit (TOO_MANY_CONNECTIONS, TOO_MANY_MODELS) or REGISTRY_READ_ONLY"
// @Failure      500 {object} render.ErrorResponse "Internal Server Error"
// @Router       /api/providers/azure-foundry/config [post]
func (h *ProviderHandler) handleSaveAzureFoundryConfig(w http.ResponseWriter, r *http.Request) {
	var req ai.AzureFoundryConfig
	if err := decodeAzureConfigBody(w, r, &req); err != nil {
		render.ErrorWithCode(w, fmt.Errorf("invalid Azure AI Foundry configuration: %w", err), http.StatusBadRequest, ai.AzureCfgErrInvalidConfig)
		return
	}
	// Normalised before the save AND the audit row: the row must carry the
	// canonical auth mode and hosts, not a client's case/whitespace variant.
	req.Normalize()
	if err := h.providerSvc.SaveAzureFoundryConfig(h.security.KeyScope(r), req); err != nil {
		if _, changed := service.AzureCredentialChanged(err); changed {
			// The save failed after the new document went in and the previous one could not be put back: the change stands.
			meta := azureConfigAuditMeta(req, !req.HasSecret())
			meta["outcome"] = "failed_after_credential_change"
			h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionKeysSave, ResourceType: "provider", ResourceID: ai.AzureFoundryProviderID, Meta: meta})
		}
		// A settings code (400), and — the save writes the registry's
		// resource "default" and its models too — the registry's own refusals.
		renderAzureRegistryError(w, err)
		return
	}
	// A saved document always holds a secret, so one the request omitted was
	// carried over from the stored document.
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionKeysSave, ResourceType: "provider", ResourceID: ai.AzureFoundryProviderID,
		Meta: azureConfigAuditMeta(req, !req.HasSecret())})
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Get local models configuration
// @Description  Returns whether a local OpenAI-compatible server (Foundry Local, Ollama, LM Studio) is configured, its loopback base URL and the stored context window in tokens (omitted when unset: 8192 applies). Desktop app only: 400 LOCAL_MODELS_UNAVAILABLE when the deployment does not allow loopback egress (cloud), so a client hides the feature there.
// @Tags         providers
// @Produce      json
// @Success      200 {object} ai.LocalPublicConfig "OK"
// @Failure      400 {object} map[string]string "LOCAL_MODELS_UNAVAILABLE"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/local/config [get]
func (h *ProviderHandler) handleGetLocalConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.providerSvc.GetLocalConfig(h.security.KeyScope(r))
	if err != nil {
		renderLocalConfigError(w, err)
		return
	}
	render.JSON(w, cfg)
}

// renderLocalConfigError answers a refused local models read or save (*ai.LocalConfigError) as a
// 400 carrying its client code, anything else as a 500.
func renderLocalConfigError(w http.ResponseWriter, err error) {
	var cfgErr *ai.LocalConfigError
	if errors.As(err, &cfgErr) {
		render.ErrorWithCode(w, err, http.StatusBadRequest, cfgErr.Code)
		return
	}
	render.Error(w, err, http.StatusInternalServerError)
}

// localConfigSaveRequest is the local models save body. ContextWindow is the context length, in
// tokens, the local server runs its models with (Ollama num_ctx): omitted or 0 = 8192, else 1024..2000000.
type localConfigSaveRequest struct {
	BaseURL       string `json:"baseUrl" example:"http://127.0.0.1:11434/v1"`
	ContextWindow int    `json:"contextWindow,omitempty" example:"32768"`
}

// @Summary      Save local models configuration
// @Description  Validates and stores the base URL of a locally hosted OpenAI-compatible server and, optionally, the context window history is trimmed to (omitted or 0 = 8192 tokens; else 1024..2000000). Loopback http(s) only (127.0.0.1 or [::1]; a trailing /v1 is accepted); desktop app only. 400 LOCAL_MODELS_UNAVAILABLE when the deployment does not allow loopback egress (cloud), INVALID_BASE_URL for a bad URL or body, INVALID_CONTEXT_WINDOW for a window out of range or not a whole number. Remove with POST /api/keys/delete; test with POST /api/providers/test.
// @Tags         providers
// @Param        request body localConfigSaveRequest true "The base URL and optional context window"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "LOCAL_MODELS_UNAVAILABLE, INVALID_BASE_URL or INVALID_CONTEXT_WINDOW"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE: the secret store cannot be reached (an OS keychain that is down); nothing changed"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/local/config [post]
func (h *ProviderHandler) handleSaveLocalConfig(w http.ResponseWriter, r *http.Request) {
	// The cloud refusal comes first, whatever the body: the client drops the
	// section on it.
	if err := ai.CheckLocalModelsAvailable(); err != nil {
		renderLocalConfigError(w, err)
		return
	}
	var req localConfigSaveRequest
	if err := decodeAzureConfigBody(w, r, &req); err != nil {
		code := ai.LocalCfgErrInvalidBaseURL
		// A fractional, string or overflowing window names the field.
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field == "contextWindow" {
			code = ai.LocalCfgErrInvalidContextWindow
		}
		render.ErrorWithCode(w, fmt.Errorf("invalid local models configuration: %w", err), http.StatusBadRequest, code)
		return
	}
	cfg, err := h.providerSvc.SaveLocalConfig(h.security.KeyScope(r), req.BaseURL, req.ContextWindow)
	if err != nil {
		renderLocalConfigError(w, err)
		return
	}
	meta := map[string]string{"op": "save", "baseUrlHost": auditURLHost(cfg.BaseURL)}
	// A token count, never a secret.
	if cfg.ContextWindow != 0 {
		meta["contextWindow"] = strconv.Itoa(cfg.ContextWindow)
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionKeysSave, ResourceType: "provider", ResourceID: ai.LocalProviderID, Meta: meta})
	render.JSON(w, map[string]string{"status": "ok"})
}

// azureConfigBodyLimit bounds a configuration save body: far above the 2 KiB
// document cap, far below anything that costs memory.
const azureConfigBodyLimit = 64 << 10

// decodeAzureConfigBody decodes strictly: unknown fields are refused (the stored document is parsed
// with DisallowUnknownFields — a misspelled field must fail here, not vanish), and so is anything
// after the JSON value (`{…}{…}` used to save the first object and silently drop the rest).
func decodeAzureConfigBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, azureConfigBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

// azureConfigAuditMeta is the audit meta of a configuration save: where the credential goes and
// whose it is (hosts, auth mode, Entra IDs) and secretCarried. Never a secret.
func azureConfigAuditMeta(cfg ai.AzureFoundryConfig, secretCarried bool) map[string]string {
	meta := map[string]string{
		"op":            "save",
		"authMode":      cfg.Auth.Mode,
		"endpointHost":  auditURLHost(cfg.Endpoint),
		"secretCarried": strconv.FormatBool(secretCarried),
	}
	if host := auditURLHost(cfg.ProjectEndpoint); host != "" {
		meta["projectHost"] = host
	}
	if cfg.Auth.Mode == ai.AzureAuthModeEntra {
		meta["tenantId"] = cfg.Auth.TenantID
		meta["clientId"] = cfg.Auth.ClientID
	}
	return meta
}

// auditURLHost is the lower-cased host[:port] of a URL, "" when it has none.
func auditURLHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// Mount registers the /api/providers group: provider listing/testing and
// OAuth device flows here; the Azure Foundry registry and local-model config
// sub-groups live with their handlers. Routes that dial out while the handler
// runs (live model listings, the billed connection test, the device-flow
// steps) touch no RLS-scoped table and run outside the request transaction.
func (h *ProviderHandler) Mount(rb *route.Builder) {
	rb.Route("/api/providers", func(rb *route.Builder) {
		rb.Get("/list", h.handleListProviders, route.NoTx())
		// The catalogue reads the keystore only (no live listing): the same policy as /list.
		rb.Get("/catalog", Query(h.providerCatalog), route.NoTx())
		// Up to three billed completions per call: the chat bucket.
		rb.Post("/test", h.handleTestProviderConnection, route.RateLimit(route.RateChat), route.Billed(), route.NoTx())
		rb.Route("/github", func(rb *route.Builder) {
			rb.Post("/start", h.handleStartGitHubAuth, route.NoTx())
			rb.Post("/poll", h.handlePollGitHubAuth, route.NoTx())
			rb.Post("/revoke", h.handleRevokeGitHubAuth)
			rb.Get("/user", h.handleGetGitHubUser, route.NoTx())
		})
		rb.Route("/copilot", func(rb *route.Builder) {
			rb.Post("/start", h.handleStartCopilotAuth, route.NoTx())
			rb.Post("/poll", h.handlePollCopilotAuth, route.NoTx())
			rb.Post("/revoke", h.handleRevokeCopilotAuth)
			rb.Get("/user", h.handleGetCopilotUser, route.NoTx())
		})
		h.registerAzureFoundryRoutes(rb)
		// Local models (Foundry Local / Ollama / LM Studio): a loopback base
		// URL document, desktop only. has/delete use /api/keys.
		rb.Route("/local", func(rb *route.Builder) {
			rb.Get("/config", h.handleGetLocalConfig)
			rb.Post("/config", h.handleSaveLocalConfig)
		})
	})
}
