package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/storage/filesystem"
)

func (rt *Router) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := rt.app.GetSettings()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, settings)
}

func (rt *Router) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var s models.AppSettings
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		logger.Error("failed to decode settings", "error", err)
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.UpdateSettings(s); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleAppInfo(w http.ResponseWriter, r *http.Request) {
	info, err := rt.app.AppInfo()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, info)
}

func (rt *Router) handleLogError(w http.ResponseWriter, r *http.Request) {
	var payload models.FrontendError
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rt.app.LogError(payload)
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// allowedProviders is the set of known provider identifiers for API key storage.
var allowedProviders = map[string]bool{
	"github": true, "openai": true, "claude": true,
	"gemini": true, "copilot": true,
}

func (rt *Router) handleSaveApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !allowedProviders[req.Provider] {
		rt.sendError(w, errors.New("unknown provider"), http.StatusBadRequest)
		return
	}
	if err := rt.app.SaveApiKey(req.Provider, req.Key); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// handleHealth returns 200 if the server is running and storage (if configured) is reachable.
func (rt *Router) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if backend := rt.app.StorageBackend(); backend != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*1e9) // 2s
		defer cancel()
		if err := backend.Ping(ctx); err != nil {
			logger.Warn("health check: storage ping failed", "error", err)
			status = "degraded"
		}
	}
	code := http.StatusOK
	if status != "ok" {
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	rt.sendJSON(w, map[string]string{"status": status})
}

func (rt *Router) handleMigrationStart(w http.ResponseWriter, r *http.Request) {
	if !rt.requireRole(w, r, auth.RoleAdmin) {
		return
	}

	rt.migrationMu.Lock()
	if rt.migrationRunning {
		rt.migrationMu.Unlock()
		http.Error(w, "migration already running", http.StatusConflict)
		return
	}
	rt.migrationRunning = true
	rt.migrationRes = nil
	rt.migrationMu.Unlock()

	go func() {
		defer func() {
			rt.migrationMu.Lock()
			rt.migrationRunning = false
			rt.migrationMu.Unlock()
		}()

		// Assume source is local data dir and destination is current postgres DB
		src, err := filesystem.NewLocalStorageBackend("data")
		if err != nil {
			logger.Error("migration start: failed to init source", "error", err)
			return
		}

		dst := rt.app.StorageBackend()
		m := migration.New(src, dst)
		res, err := m.Migrate(context.Background())
		
		rt.migrationMu.Lock()
		rt.migrationRes = &res
		if err != nil {
			rt.migrationRes.Errors = append(rt.migrationRes.Errors, migration.MigrationError{
				Message: err.Error(),
			})
		}
		rt.migrationMu.Unlock()
	}()

	rt.sendJSON(w, map[string]string{"status": "started"})
}

func (rt *Router) handleMigrationStatus(w http.ResponseWriter, r *http.Request) {
	if !rt.requireRole(w, r, auth.RoleAdmin) {
		return
	}

	rt.migrationMu.Lock()
	defer rt.migrationMu.Unlock()

	if rt.migrationRunning {
		rt.sendJSON(w, map[string]string{"status": "running"})
		return
	}

	if rt.migrationRes == nil {
		rt.sendJSON(w, map[string]string{"status": "idle"})
		return
	}

	rt.sendJSON(w, map[string]any{
		"status": "completed",
		"result": rt.migrationRes,
	})
}

func (rt *Router) handleHasApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	has, err := rt.app.HasApiKey(req.Provider)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, has)
}

func (rt *Router) handleDeleteApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.DeleteApiKey(req.Provider); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}
