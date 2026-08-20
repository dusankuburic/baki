package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"

	"github.com/go-chi/chi/v5"
)

var validProviders = map[string]bool{
	"openai":        true,
	"claude":        true,
	"gemini":        true,
	"xai":           true,
	"glm":           true,
	"github-models": true,
	"copilot":       true,
	"demo":          true,
}

// readinessFailureThreshold is the number of consecutive failed readiness
// probes required before the handler reports 503. This tolerates transient
// Azure latency spikes so a single slow Ping doesn't flap the pod out of
// rotation. State is per-instance (sufficient: a flapping replica reports its
// own readiness independently).
const readinessFailureThreshold = 3

// blobCheckCacheTTL is how long a readiness probe reuses the last SUCCESSFUL
// blob reachability result. Each fresh check is a network call to Azure;
// probes fire every few seconds per replica, and blob health doesn't change at
// that granularity. Failures are never cached: the failure-streak threshold
// exists to tolerate one transient blip, and replaying a single cached failure
// across probes would count it readinessFailureThreshold times.
const blobCheckCacheTTL = 15 * time.Second

type SystemHandler struct {
	sysSvc   *service.SystemService
	security *SecurityConfig
	backend  storageif.StorageBackend // may be nil in local/filesystem mode

	// redisPinger is a health-check interface onto the optional Redis backplane
	// (rate limiter, hub presence, chat-stream resume). When non-nil, readiness
	// gates on it too — a Redis outage silently degrades multi-replica
	// correctness (all three subsystems fail open), so without this check the
	// pod stays in rotation reporting "ready" while losing shared state.
	redisPinger RedisPinger

	readyMu       sync.Mutex
	readyFailures int

	blobCheckMu   sync.Mutex
	blobCheckedAt time.Time // zero when the last check failed (never cache failures)
}

// RedisPinger is the subset of the Redis client the readiness probe needs.
// Implemented by *redis.Client; defined here so the handler can accept a nil
// sentinel in single-replica mode without pulling the redis package into API.
type RedisPinger interface {
	Ping(ctx context.Context) error
}

func NewSystemHandler(sysSvc *service.SystemService, security *SecurityConfig, backend storageif.StorageBackend, redisPinger RedisPinger) *SystemHandler {
	return &SystemHandler{sysSvc: sysSvc, security: security, backend: backend, redisPinger: redisPinger}
}

// @Summary      Get app settings
// @Description  Returns the current application settings.
// @Tags         system
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/system/settings [get]
// @Router       /api/system/settings/user [get]
func (h *SystemHandler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	settings, err := h.sysSvc.GetUserSettings(r.Context(), userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, settings)
}

// @Summary      Update app settings
// @Description  Updates the application settings.
// @Tags         system
// @Param        settings body object true "settings"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/system/settings [post]
// @Router       /api/system/settings [put]
// @Router       /api/system/settings/user [post]
func (h *SystemHandler) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req models.AppSettings
	if !decodeBody(w, r, &req) {
		return
	}

	userID := h.security.CallerID(r)
	if err := h.sysSvc.UpdateUserSettings(r.Context(), userID, req); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleGetOrgSettings returns org-wide settings. Reading requires org
// membership — settings carry operational details (budgets, provider
// configuration) that must not leak across tenants.
// @Summary      Get org settings
// @Description  handleGetOrgSettings returns org-wide settings. Reading requires org membership — settings carry operational details (budgets, provider configuration) that must not leak across tenants.
// @Tags         system
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Settings"
// @Router       /api/system/settings/org/{id} [get]
func (h *SystemHandler) handleGetOrgSettings(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "id")
	if !requireOrgMember(w, r, h.security, orgID) {
		return
	}
	settings, err := h.sysSvc.GetOrgSettings(r.Context(), orgID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, settings)
}

// handleUpdateOrgSettings overwrites org-wide settings. Writing requires org
// admin.
// @Summary      Update org settings
// @Description  handleUpdateOrgSettings overwrites org-wide settings. Writing requires org admin.
// @Tags         system
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Updated"
// @Router       /api/system/settings/org/{id} [post]
func (h *SystemHandler) handleUpdateOrgSettings(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "id")
	if !requireOrgAdmin(w, r, h.security, orgID) {
		return
	}
	var req models.AppSettings
	if !decodeBody(w, r, &req) {
		return
	}
	if err := h.sysSvc.UpdateOrgSettings(r.Context(), orgID, req); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionSettingsChange, "org", orgID, nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Get app info
// @Description  Returns information about the application.
// @Tags         system
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/system/info [get]
func (h *SystemHandler) handleAppInfo(w http.ResponseWriter, r *http.Request) {
	info, err := h.sysSvc.AppInfo()
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, info)
}

// handleFeatures returns the active product feature flags. Public (pre-auth) so
// the login page can hide the register button when DisableSignUp is set without
// needing a session first.
// @Summary      Feature flags
// @Description  Which optional capabilities are enabled in this deployment.
// @Tags         system
// @Produce      json
// @Success      200 {object} map[string]interface{} "Features"
// @Router       /api/system/features [get]
func (h *SystemHandler) handleFeatures(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, map[string]bool{
		"disableSignUp": h.security.Features.DisableSignUp,
	})
}

// @Summary      Log a frontend error
// @Description  Records a client-side error for server-side observability.
// @Tags         system
// @Param        error body object true "error"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Router       /api/system/log-error [post]
func (h *SystemHandler) handleLogError(w http.ResponseWriter, r *http.Request) {
	var req models.FrontendError
	if !decodeBody(w, r, &req) {
		return
	}
	h.sysSvc.LogError(req)
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Save API key
// @Description  Saves an API key for a given provider.
// @Tags         keys
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/keys/save [post]
func (h *SystemHandler) handleSaveApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Provider == "" || !validProviders[req.Provider] {
		render.Error(w, fmt.Errorf("invalid or unsupported provider: %q", req.Provider), http.StatusBadRequest)
		return
	}
	if err := h.sysSvc.SaveApiKey(h.security.KeyScope(r), req.Provider, req.Key); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Check if API key exists
// @Description  Checks if an API key for a given provider is saved.
// @Tags         keys
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} bool "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/keys/has [post]
func (h *SystemHandler) handleHasApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	has, err := h.sysSvc.HasApiKey(h.security.KeyScope(r), req.Provider)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]bool{"has": has})
}

// @Summary      Delete API key
// @Description  Deletes an API key for a given provider.
// @Tags         keys
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/keys/delete [post]
func (h *SystemHandler) handleDeleteApiKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Provider == "" || !validProviders[req.Provider] {
		render.Error(w, fmt.Errorf("invalid or unsupported provider: %q", req.Provider), http.StatusBadRequest)
		return
	}
	if err := h.sysSvc.DeleteApiKey(h.security.KeyScope(r), req.Provider); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Local backend config
// @Description  Returns the pre-shared token in local (non-JWT) mode so a web client can self-configure. 404 in cloud mode.
// @Tags         system
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      404 {object} map[string]string "Not Found"
// @Router       /api/local-config [get]
func (h *SystemHandler) handleLocalConfig(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		// Cloud/JWT mode: there is no local session token. Return 200 with an
		// explicit "cloud" marker (rather than 404) so the web client can probe
		// this endpoint at startup without a noisy console error, then fall back
		// to the login flow because no token is present.
		render.JSON(w, map[string]string{"mode": "cloud"})
		return
	}
	render.JSON(w, map[string]string{
		"mode":  "local",
		"token": h.security.Token,
	})
}

// @Summary      Liveness probe
// @Description  Returns 200 if the process is up. Does not touch the database.
// @Tags         system
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Router       /healthz [get]
func (h *SystemHandler) handleLiveness(w http.ResponseWriter, r *http.Request) {
	// Liveness is intentionally cheap — it only proves the process is alive.
	// Do NOT add DB checks here; a slow DB would cause the container to restart
	// in a loop, which is worse than leaving it running and letting readiness handle it.
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Readiness probe
// @Description  Returns 200 when downstream dependencies (DB) are reachable, 503 otherwise.
// @Tags         system
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      503 {object} map[string]string "Service Unavailable"
// @Router       /readyz [get]
// @Router       /api/health [get]
func (h *SystemHandler) handleReadiness(w http.ResponseWriter, r *http.Request) {
	// Readiness checks that the service can serve traffic.
	// The orchestrator (AKS / ACA) uses this to gate traffic routing.
	// Return 503 only after N consecutive failures so a transient Azure
	// latency spike doesn't flap the pod out of rotation; a healthy probe
	// resets the streak.
	if h.backend != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		checkErr := h.backend.Ping(ctx)
		// When the backend offloads flow content to blob storage, broken blob
		// auth/config would let the pod serve traffic while returning empty
		// flows. Gate readiness on blob reachability too (no-op when unconfigured).
		if checkErr == nil {
			if bc, ok := h.backend.(blobHealthChecker); ok {
				checkErr = h.checkBlobCached(ctx, bc)
			}
		}
		// H21: when the Redis backplane is configured (PAD_REDIS_URL set), gate
		// readiness on its reachability too. The rate limiter, hub presence,
		// and chat-stream resume ALL fail open on Redis errors, so an outage
		// silently degrades multi-replica correctness (the effective rate-limit
		// multiplies by replica count, cross-replica presence disappears,
		// stream-resume returns partial buffers) without taking the pod out of
		// rotation. Operators who explicitly opt into Redis have declared it a
		// hard dependency — surface its loss.
		if checkErr == nil && h.redisPinger != nil {
			redisCtx, redisCancel := context.WithTimeout(r.Context(), 2*time.Second)
			checkErr = h.redisPinger.Ping(redisCtx)
			redisCancel()
		}

		if checkErr != nil {
			h.readyMu.Lock()
			h.readyFailures++
			failed := h.readyFailures >= readinessFailureThreshold
			h.readyMu.Unlock()
			if failed {
				render.Error(w, fmt.Errorf("backend unavailable: %w", checkErr), http.StatusServiceUnavailable)
			} else {
				render.JSON(w, map[string]string{"status": "ok"})
			}
			return
		}

		h.readyMu.Lock()
		h.readyFailures = 0
		h.readyMu.Unlock()
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// blobHealthChecker is implemented by storage backends that offload content to
// blob storage (currently the PostgreSQL backend). Optional: backends without
// blob storage simply don't implement it and the readiness check is skipped.
type blobHealthChecker interface {
	CheckBlob(ctx context.Context) error
}

// componentStatus is one subsystem's health verdict in the admin breakdown.
// "skipped" means the component isn't configured (e.g. no Redis, no blob).
type componentStatus struct {
	Status string `json:"status"`          // "ok" | "error" | "skipped"
	Error  string `json:"error,omitempty"` // present only when Status == "error"
}

// adminHealthResponse is the structured payload for GET /api/admin/system/health.
// Unlike /readyz (a single 200/503 verdict for the orchestrator), this returns a
// per-subsystem breakdown so an admin can see WHICH dependency is degraded
// without grepping logs.
type adminHealthResponse struct {
	Database componentStatus `json:"database"`
	Blob     componentStatus `json:"blob"`
	Redis    componentStatus `json:"redis"`
	Overall  string          `json:"overall"` // "ok" | "degraded" | "down"
}

// handleAdminSystemHealth returns a structured per-subsystem health breakdown.
// Admin-only (the public /readyz is the orchestrator-facing verdict). Reuses the
// same probes as readiness but reports each independently rather than collapsing
// to one pass/fail, and does NOT apply the consecutive-failure smoothing — an
// admin querying health wants the live picture, not a flap-dampened one.
// @Summary      Detailed system health
// @Description  handleAdminSystemHealth returns a structured per-subsystem health breakdown. Admin-only (the public /readyz is the orchestrator-facing verdict). Reuses the same probes as readiness but reports each independently rather than collapsing to one pass/fail, and does NOT apply the consecutive-failure smoothing — an admin querying health wants the live picture, not a flap-dampened one.
// @Tags         admin
// @Produce      json
// @Success      200 {object} map[string]interface{} "Health detail"
// @Failure      403 {object} map[string]string "Forbidden"
// @Router       /api/admin/system/health [get]
func (h *SystemHandler) handleAdminSystemHealth(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	resp := adminHealthResponse{Overall: "ok"}

	if h.backend == nil {
		// Local/filesystem mode: no DB, no blob, no Redis.
		resp.Database = componentStatus{Status: "skipped"}
		resp.Blob = componentStatus{Status: "skipped"}
		resp.Redis = componentStatus{Status: "skipped"}
		resp.Overall = "ok"
		render.JSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	// Database
	if err := h.backend.Ping(ctx); err != nil {
		resp.Database = componentStatus{Status: "error", Error: err.Error()}
		resp.Overall = "down"
	} else {
		resp.Database = componentStatus{Status: "ok"}
	}

	// Blob (optional)
	if bc, ok := h.backend.(blobHealthChecker); ok {
		if err := h.checkBlobCached(ctx, bc); err != nil {
			resp.Blob = componentStatus{Status: "error", Error: err.Error()}
			if resp.Overall == "ok" {
				resp.Overall = "degraded"
			}
		} else {
			resp.Blob = componentStatus{Status: "ok"}
		}
	} else {
		resp.Blob = componentStatus{Status: "skipped"}
	}

	// Redis (optional)
	if h.redisPinger != nil {
		redisCtx, redisCancel := context.WithTimeout(r.Context(), 2*time.Second)
		if err := h.redisPinger.Ping(redisCtx); err != nil {
			resp.Redis = componentStatus{Status: "error", Error: err.Error()}
			if resp.Overall == "ok" {
				resp.Overall = "degraded"
			}
		} else {
			resp.Redis = componentStatus{Status: "ok"}
		}
		redisCancel()
	} else {
		resp.Redis = componentStatus{Status: "skipped"}
	}

	render.JSON(w, resp)
}

// checkBlobCached returns the blob reachability status, reusing a successful
// result for blobCheckCacheTTL. Failures are returned but not cached (see
// blobCheckCacheTTL). The mutex also collapses concurrent probes into a
// single Azure call.
func (h *SystemHandler) checkBlobCached(ctx context.Context, bc blobHealthChecker) error {
	h.blobCheckMu.Lock()
	defer h.blobCheckMu.Unlock()
	if !h.blobCheckedAt.IsZero() && time.Since(h.blobCheckedAt) < blobCheckCacheTTL {
		return nil
	}
	if err := bc.CheckBlob(ctx); err != nil {
		h.blobCheckedAt = time.Time{}
		return err
	}
	h.blobCheckedAt = time.Now()
	return nil
}
