package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type ProviderHandler struct {
	providerSvc *service.ProviderService
	security    *SecurityConfig
	// backend receives the audit rows for configuration changes (nil on the
	// desktop, where logAudit is a no-op).
	backend storageif.StorageBackend
}

func NewProviderHandler(providerSvc *service.ProviderService, security *SecurityConfig, backend storageif.StorageBackend) *ProviderHandler {
	return &ProviderHandler{providerSvc: providerSvc, security: security, backend: backend}
}

// @Summary      List AI providers
// @Description  Returns a list of supported AI providers and their current status. With orgId, a provider the caller has not configured but that organisation has (a shared credential) is listed as configured and shared; the parameter is ignored unless the caller is an admin or member of that org (viewers, guests and non-members need a credential of their own).
// @Tags         providers
// @Param        orgId query string false "Organisation whose shared credentials to include"
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
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

// listingOrg returns the ?orgId= the caller may list shared credentials for: the parameter when the
// caller's role may run on the org's shared credential (admin or member — the rule the chat turn
// applies), else "" — a non-member learns nothing.
func (h *ProviderHandler) listingOrg(r *http.Request) string {
	orgID := r.URL.Query().Get("orgId")
	if orgID == "" || h.security.OrgSvc == nil {
		return ""
	}
	role, err := h.security.OrgSvc.MemberRole(r.Context(), orgID, h.security.CallerID(r))
	if err != nil || !service.OrgRoleUsesSharedCredential(role) {
		return ""
	}
	return orgID
}

// @Summary      Test provider connection
// @Description  Tests the connection to a specific AI provider. For azure-foundry this form is deprecated (one release): it tests the caller's resource "default" (else their first resource) as a resource — an authenticated probe, no model call; use POST /api/providers/azure-foundry/test to test one resource or model.
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
// @Failure      500 {object} map[string]string "Internal Server Error"
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
// @Failure      500 {object} map[string]string "Internal Server Error"
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
			logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "provider", ai.AzureFoundryProviderID, meta)
		}
		// A settings code (400), and — the save writes the registry's
		// resource "default" and its models too — the registry's own refusals.
		renderAzureRegistryError(w, err)
		return
	}
	// A saved document always holds a secret, so one the request omitted was
	// carried over from the stored document.
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "provider", ai.AzureFoundryProviderID,
		azureConfigAuditMeta(req, !req.HasSecret()))
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
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "provider", ai.LocalProviderID, meta)
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

// registerRoutes wires the /api/providers group: provider listing/testing and
// OAuth device flows here; the Azure Foundry registry and local-model config
// sub-groups live with their handlers.
func (h *ProviderHandler) registerRoutes(r chi.Router) {
	r.Route("/api/providers", func(r chi.Router) {
		r.Get("/list", h.handleListProviders)
		r.Post("/test", h.handleTestProviderConnection)
		r.Route("/github", func(r chi.Router) {
			r.Post("/start", h.handleStartGitHubAuth)
			r.Post("/poll", h.handlePollGitHubAuth)
			r.Post("/revoke", h.handleRevokeGitHubAuth)
			r.Get("/user", h.handleGetGitHubUser)
		})
		r.Route("/copilot", func(r chi.Router) {
			r.Post("/start", h.handleStartCopilotAuth)
			r.Post("/poll", h.handlePollCopilotAuth)
			r.Post("/revoke", h.handleRevokeCopilotAuth)
			r.Get("/user", h.handleGetCopilotUser)
		})
		h.registerAzureFoundryRoutes(r)
		// Local models (Foundry Local / Ollama / LM Studio): a loopback base
		// URL document, desktop only. has/delete use /api/keys.
		r.Route("/local", func(r chi.Router) {
			r.Get("/config", h.handleGetLocalConfig)
			r.Post("/config", h.handleSaveLocalConfig)
		})
	})
}
