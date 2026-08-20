package api

import (
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/service"
)

type ProviderHandler struct {
	providerSvc *service.ProviderService
	security    *SecurityConfig
}

func NewProviderHandler(providerSvc *service.ProviderService, security *SecurityConfig) *ProviderHandler {
	return &ProviderHandler{providerSvc: providerSvc, security: security}
}

// @Summary      List AI providers
// @Description  Returns a list of supported AI providers and their current status.
// @Tags         providers
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/providers/list [get]
func (h *ProviderHandler) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.providerSvc.ListProviders(r.Context(), h.security.KeyScope(r))
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, providers)
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
