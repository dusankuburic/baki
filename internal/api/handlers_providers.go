package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

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

// listingOrg returns the ?orgId= the caller may list shared credentials for:
// the parameter when the caller's role in that organisation may run on its
// shared credential (admin or member, service.OrgRoleUsesSharedCredential —
// the rule the chat turn applies), else "" (listed without an org: a
// non-member learns nothing, not even that the org exists, and a viewer or
// guest is not offered a provider their turns could not use).
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
// @Description  Tests the connection to a specific AI provider.
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
// @Description  Returns the caller's Azure AI Foundry settings (endpoint, auth mode, project endpoint, deployments) without any secret.
// @Tags         providers
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/azure-foundry/config [get]
func (h *ProviderHandler) handleGetAzureFoundryConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.providerSvc.GetAzureFoundryConfig(h.security.KeyScope(r))
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, cfg)
}

// @Summary      Save Azure AI Foundry configuration
// @Description  Validates and stores the caller's Azure AI Foundry endpoint, credential (API key or Microsoft Entra ID service principal), optional project endpoint and deployment names. An omitted secret keeps the stored one.
// @Tags         providers
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/azure-foundry/config [post]
func (h *ProviderHandler) handleSaveAzureFoundryConfig(w http.ResponseWriter, r *http.Request) {
	// Strict decode: the stored document is parsed with DisallowUnknownFields
	// (ParseAzureFoundryConfig), so a misspelled field must fail here rather
	// than vanish. 64 KiB is far above the 2 KiB document cap.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	var req ai.AzureFoundryConfig
	if err := dec.Decode(&req); err != nil {
		render.ErrorWithCode(w, fmt.Errorf("invalid Azure AI Foundry configuration: %w", err), http.StatusBadRequest, ai.AzureCfgErrInvalidConfig)
		return
	}
	// Normalised before the save AND the audit row: the row must carry the
	// canonical auth mode, not a client's case/whitespace variant.
	req.Normalize()
	if err := h.providerSvc.SaveAzureFoundryConfig(h.security.KeyScope(r), req); err != nil {
		var cfgErr *ai.AzureConfigError
		if errors.As(err, &cfgErr) {
			render.ErrorWithCode(w, err, http.StatusBadRequest, cfgErr.Code)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Never the secrets — only that the provider changed and how it authenticates.
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "provider", ai.AzureFoundryProviderID, map[string]string{"op": "save", "authMode": req.Auth.Mode})
	render.JSON(w, map[string]string{"status": "ok"})
}
