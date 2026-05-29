package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/storage/filesystem"
)

// @Summary Get app settings
// @Description Returns the current application settings.
// @Tags system
// @Produce json
// @Success 200 {object} models.AppSettings
// @Failure 500 {object} map[string]string
// @Router /api/system/settings [get]
func (rt *Router) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := rt.app.GetSettings()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, settings)
}

// @Summary Update app settings
// @Description Updates the application settings.
// @Tags system
// @Accept json
// @Produce json
// @Param settings body models.AppSettings true "App Settings"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/system/settings [post]
func (rt *Router) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}
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

// @Summary Get app info
// @Description Returns information about the application.
// @Tags system
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 500 {object} map[string]string
// @Router /api/system/info [get]
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

// @Summary Save API key
// @Description Saves an API key for a given provider.
// @Tags keys
// @Accept json
// @Produce json
// @Param request body object{provider=string,key=string} true "API Key Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/keys/save [post]
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
	if err := rt.app.SaveApiKey(rt.keyScope(r), req.Provider, req.Key); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// handleHealth returns 200 if the server is running and storage (if configured) is reachable.
// @Summary Check API health
// @Description Returns the status of the API and its storage backend.
// @Tags system
// @Produce json
// @Success 200 {object} map[string]string
// @Failure 503 {object} map[string]string
// @Router /api/health [get]
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
		// A panic in a goroutine bypasses the Recovery middleware and would crash
		// the whole process, so recover here, record it, and always clear the flag.
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("migration panicked", "panic", rec)
				rt.migrationMu.Lock()
				if rt.migrationRes == nil {
					rt.migrationRes = &migration.Result{}
				}
				rt.migrationRes.Errors = append(rt.migrationRes.Errors, migration.MigrationError{
					Message: fmt.Sprintf("migration panicked: %v", rec),
				})
				rt.migrationMu.Unlock()
			}
			rt.migrationMu.Lock()
			rt.migrationRunning = false
			rt.migrationMu.Unlock()
		}()

		// Bound the migration so it cannot run unbounded or block shutdown forever.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		// Assume source is local data dir and destination is current postgres DB
		src, err := filesystem.NewLocalStorageBackend("data")
		if err != nil {
			logger.Error("migration start: failed to init source", "error", err)
			return
		}

		dst := rt.app.StorageBackend()
		m := migration.New(src, dst)
		res, err := m.Migrate(ctx)

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

// @Summary Check if API key exists
// @Description Checks if an API key for a given provider is saved.
// @Tags keys
// @Accept json
// @Produce json
// @Param request body object{provider=string} true "Check API Key Request"
// @Success 200 {object} bool
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/keys/has [post]
func (rt *Router) handleHasApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	has, err := rt.app.HasApiKey(rt.keyScope(r), req.Provider)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, has)
}

// @Summary Delete API key
// @Description Deletes an API key for a given provider.
// @Tags keys
// @Accept json
// @Produce json
// @Param request body object{provider=string} true "Delete API Key Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/keys/delete [post]
func (rt *Router) handleDeleteApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.DeleteApiKey(rt.keyScope(r), req.Provider); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}
