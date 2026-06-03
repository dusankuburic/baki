package api

import (
	"encoding/json"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/service"
)

type SystemHandler struct {
	sysSvc   *service.SystemService
	security *SecurityConfig
}

func NewSystemHandler(sysSvc *service.SystemService, security *SecurityConfig) *SystemHandler {
	return &SystemHandler{sysSvc: sysSvc, security: security}
}

func (h *SystemHandler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.sysSvc.GetSettings()
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, settings)
}

func (h *SystemHandler) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	var req models.AppSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := h.sysSvc.UpdateSettings(req); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *SystemHandler) handleAppInfo(w http.ResponseWriter, r *http.Request) {
	info, err := h.sysSvc.AppInfo()
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, info)
}

func (h *SystemHandler) handleLogError(w http.ResponseWriter, r *http.Request) {
	var req models.FrontendError
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	h.sysSvc.LogError(req)
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *SystemHandler) handleSaveApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := h.sysSvc.SaveApiKey(h.security.KeyScope(r), req.Provider, req.Key); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *SystemHandler) handleHasApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	has, err := h.sysSvc.HasApiKey(h.security.KeyScope(r), req.Provider)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]bool{"has": has})
}

func (h *SystemHandler) handleDeleteApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := h.sysSvc.DeleteApiKey(h.security.KeyScope(r), req.Provider); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *SystemHandler) handleLocalConfig(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		http.NotFound(w, r)
		return
	}
	render.JSON(w, map[string]string{
		"token": h.security.Token,
	})
}

func (h *SystemHandler) handleLiveness(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *SystemHandler) handleReadiness(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, map[string]string{"status": "ok"})
}
