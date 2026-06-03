package api

import (
	"encoding/json"
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

func (h *ProviderHandler) handleListProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.providerSvc.ListProviders(h.security.KeyScope(r))
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, providers)
}

func (h *ProviderHandler) handleTestProviderConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.providerSvc.TestProviderConnection(r.Context(), h.security.KeyScope(r), req.Provider)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

func (h *ProviderHandler) handleStartGitHubAuth(w http.ResponseWriter, r *http.Request) {
	res, err := h.providerSvc.StartGitHubAuth(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

func (h *ProviderHandler) handlePollGitHubAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.providerSvc.PollGitHubAuth(r.Context(), req.DeviceCode)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

func (h *ProviderHandler) handleRevokeGitHubAuth(w http.ResponseWriter, r *http.Request) {
	if err := h.providerSvc.RevokeGitHubAuth(); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *ProviderHandler) handleGetGitHubUser(w http.ResponseWriter, r *http.Request) {
	user, err := h.providerSvc.GetGitHubUser(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, user)
}

func (h *ProviderHandler) handleStartCopilotAuth(w http.ResponseWriter, r *http.Request) {
	res, err := h.providerSvc.StartCopilotAuth(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

func (h *ProviderHandler) handlePollCopilotAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	res, err := h.providerSvc.PollCopilotAuth(r.Context(), req.DeviceCode)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

func (h *ProviderHandler) handleRevokeCopilotAuth(w http.ResponseWriter, r *http.Request) {
	if err := h.providerSvc.RevokeCopilotAuth(); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *ProviderHandler) handleGetCopilotUser(w http.ResponseWriter, r *http.Request) {
	user, err := h.providerSvc.GetCopilotUser(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, user)
}
