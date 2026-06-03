package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"

	"github.com/go-chi/chi/v5"
)

type SystemHandler struct {
	sysSvc   *service.SystemService
	security *SecurityConfig
	backend  storageif.StorageBackend // may be nil in local/filesystem mode
}

func NewSystemHandler(sysSvc *service.SystemService, security *SecurityConfig, backend storageif.StorageBackend) *SystemHandler {
	return &SystemHandler{sysSvc: sysSvc, security: security, backend: backend}
}

func (h *SystemHandler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "id")
	if orgID != "" {
		settings, err := h.sysSvc.GetOrgSettings(orgID)
		if err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		render.JSON(w, settings)
		return
	}

	userID := h.security.CallerID(r)
	settings, err := h.sysSvc.GetUserSettings(userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, settings)
}

func (h *SystemHandler) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "id")
	var req models.AppSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	if orgID != "" {
		if err := h.sysSvc.UpdateOrgSettings(orgID, req); err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		render.JSON(w, map[string]string{"status": "ok"})
		return
	}

	userID := h.security.CallerID(r)
	if err := h.sysSvc.UpdateUserSettings(userID, req); err != nil {
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
	// Liveness is intentionally cheap — it only proves the process is alive.
	// Do NOT add DB checks here; a slow DB would cause the container to restart
	// in a loop, which is worse than leaving it running and letting readiness handle it.
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *SystemHandler) handleReadiness(w http.ResponseWriter, r *http.Request) {
	// Readiness checks that the service can serve traffic.
	// The orchestrator (AKS / ACA) uses this to gate traffic routing.
	// Return 503 if the database is unreachable so the pod is removed from
	// the load-balancer rotation until connectivity is restored.
	if h.backend != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := h.backend.Ping(ctx); err != nil {
			render.Error(w, errors.New("database unavailable"), http.StatusServiceUnavailable)
			return
		}
	}
	render.JSON(w, map[string]string{"status": "ok"})
}
