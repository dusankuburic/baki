package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
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
	"azure-foundry": true,
	"local":         true,
}

// readinessFailureThreshold is the consecutive failed readiness probes before
// the handler reports 503, so one slow Ping doesn't flap the pod.
const readinessFailureThreshold = 3

// blobCheckCacheTTL is how long a probe reuses the last SUCCESSFUL blob reachability result (each
// fresh check is a network call). Failures are never cached: replaying one would count it N times.
const blobCheckCacheTTL = 15 * time.Second

type SystemHandler struct {
	sysSvc   *service.SystemService
	security *SecurityConfig
	backend  storageif.StorageBackend // may be nil in local/filesystem mode

	// redisPinger pings the optional Redis backplane (rate limiter, hub presence, stream resume).
	// When non-nil, readiness gates on it: all three subsystems fail open, so a Redis outage
	// would silently degrade multi-replica correctness.
	redisPinger RedisPinger

	// ruleProfiles is invalidated when an org's settings change, because AppSettings.Analysis.Rules
	// IS the org's rule profile — without this a toggle waits out the resolver's TTL.
	ruleProfiles *service.RuleProfileResolver

	readyMu       sync.Mutex
	readyFailures int

	blobCheckMu   sync.Mutex
	blobCheckedAt time.Time // zero when the last check failed (never cache failures)
}

// RedisPinger is the subset of the Redis client the readiness probe needs;
// defined here so a nil sentinel works without pulling redis into API.
type RedisPinger interface {
	Ping(ctx context.Context) error
}

func NewSystemHandler(sysSvc *service.SystemService, security *SecurityConfig, backend storageif.StorageBackend, redisPinger RedisPinger, ruleProfiles *service.RuleProfileResolver) *SystemHandler {
	return &SystemHandler{sysSvc: sysSvc, security: security, backend: backend, redisPinger: redisPinger, ruleProfiles: ruleProfiles}
}

// @Summary      Get app settings
// @Description  Returns the current application settings.
// @Tags         system
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/system/settings [get]
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
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionSettingsChange, "settings", userID, map[string]string{"scope": "user"})
	render.JSON(w, map[string]string{"status": "ok"})
}

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
	if h.ruleProfiles != nil {
		// Deferred so it runs AFTER the write: invalidating before would let a
		// resolve in the gap re-cache the stale profile.
		defer h.ruleProfiles.Invalidate(orgID)
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

// handleFeatures returns the active feature flags. Public (pre-auth) so the
// login page can hide the register button without a session first.
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
	// The body used to ride the router's 10 MiB global cap straight into the
	// retained ERROR log; 64 KiB holds any real stack trace.
	r.Body = http.MaxBytesReader(w, r.Body, maxFrontendErrorBytes)
	var req models.FrontendError
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			render.Error(w, fmt.Errorf("error report exceeds %d bytes", maxFrontendErrorBytes), http.StatusRequestEntityTooLarge)
			return
		}
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	h.sysSvc.LogError(r.Context(), req)
	render.JSON(w, map[string]string{"status": "ok"})
}

// maxFrontendErrorBytes bounds POST /api/system/log-error.
const maxFrontendErrorBytes = 64 << 10

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
	// Azure AI Foundry stores a validated configuration document, not a bare
	// key; raw text here would bypass the host allow-list.
	if req.Provider == ai.AzureFoundryProviderID {
		render.ErrorWithCode(w, fmt.Errorf("this provider is configured through /api/providers/azure-foundry/connections"), http.StatusBadRequest, "USE_PROVIDER_CONFIG")
		return
	}
	// The local models provider likewise stores a validated document (a loopback base URL).
	if req.Provider == ai.LocalProviderID {
		render.ErrorWithCode(w, fmt.Errorf("this provider is configured through /api/providers/local/config"), http.StatusBadRequest, "USE_PROVIDER_CONFIG")
		return
	}
	if err := h.sysSvc.SaveApiKey(h.security.KeyScope(r), req.Provider, req.Key); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// The key itself never reaches the audit row — only which provider changed.
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "provider", req.Provider, map[string]string{"op": "save"})
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Check if API key exists
// @Description  Checks if an API key for a given provider is saved. For azure-foundry (deprecated form, one release): whether the caller has at least one Azure AI Foundry resource of their own — GET /api/providers/azure-foundry/connections lists them.
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
// @Description  Deletes an API key for a given provider. For azure-foundry (deprecated form, one release): removes the caller's resource "default" — the migrated single-document configuration — with its models and its stored credential; the other resources stay (DELETE /api/providers/azure-foundry/connections/{connectionId} removes one).
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
		if req.Provider == ai.AzureFoundryProviderID {
			// Resource "default" with its models: the registry's refusals (REGISTRY_READ_ONLY, …).
			renderAzureRegistryError(w, err)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "provider", req.Provider, map[string]string{"op": "delete"})
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
		// Cloud/JWT mode: 200 with a "cloud" marker (not 404) so the client can
		// probe startup without a console error, then fall back to login.
		render.JSON(w, map[string]string{"mode": "cloud"})
		return
	}
	// The token is served ONLY when explicitly enabled: this route is public and, in local mode,
	// CORS reflects any loopback Origin — returning the token let any localhost page read it
	// cross-origin. Only browser-based local dev opts in with PAD_LOCAL_CONFIG_ENDPOINT=true.
	if !h.security.Features.LocalConfigEndpoint {
		render.JSON(w, map[string]string{"mode": "local"})
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
	// Liveness only proves the process is alive. Do NOT add DB checks: a slow
	// DB restarting the container in a loop is worse than leaving it to readiness.
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
	// Readiness gates traffic routing (AKS / ACA). 503 only after N consecutive failures so a
	// transient latency spike doesn't flap the pod out of rotation; a healthy probe resets it.
	if h.backend != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		checkErr := h.backend.Ping(ctx)
		// When the backend offloads flow content to blob storage, broken blob auth/config would
		// serve traffic with empty flows. Gate readiness on it (no-op when unconfigured).
		if checkErr == nil {
			if bc, ok := h.backend.(blobHealthChecker); ok {
				checkErr = h.checkBlobCached(ctx, bc)
			}
		}
		// H21: when Redis is configured, gate readiness on it — the rate limiter, hub presence and
		// stream resume all fail open, so an outage silently degrades multi-replica correctness.
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
// blob storage; backends without it skip the readiness check.
type blobHealthChecker interface {
	CheckBlob(ctx context.Context) error
}

// classifyHealthErr reduces a dependency error to a coarse, non-leaking
// class for the admin health payload (B1.8): timeout / refused / auth / ….
func classifyHealthErr(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no route"), strings.Contains(msg, "unreachable"):
		return "connection refused"
	case strings.Contains(msg, "reset by peer"), strings.Contains(msg, "broken pipe"):
		return "connection reset"
	case strings.Contains(msg, "auth"), strings.Contains(msg, "credential"), strings.Contains(msg, "permission"):
		return "auth failed"
	case strings.Contains(msg, "no rows"):
		return "not found"
	default:
		return "unavailable"
	}
}

type componentStatus struct {
	Status string `json:"status"`          // "ok" | "error" | "skipped"
	Error  string `json:"error,omitempty"` // present only when Status == "error"
}

// adminHealthResponse is the /api/admin/system/health payload: a
// per-subsystem breakdown so an admin can see WHICH dependency is degraded.
type adminHealthResponse struct {
	Database componentStatus `json:"database"`
	Blob     componentStatus `json:"blob"`
	Redis    componentStatus `json:"redis"`
	Overall  string          `json:"overall"` // "ok" | "degraded" | "down"
}

// handleAdminSystemHealth returns a per-subsystem health breakdown. Admin-only; each probe is
// reported independently, without the consecutive-failure smoothing — the live picture.
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

	// Database. B1.8: dependency errors embed hostnames/DSN fragments/SQL
	// state — log the full error, return a coarse class only.
	if err := h.backend.Ping(ctx); err != nil {
		slog.Warn("health: database ping failed", "err", err)
		resp.Database = componentStatus{Status: "error", Error: classifyHealthErr(err)}
		resp.Overall = "down"
	} else {
		resp.Database = componentStatus{Status: "ok"}
	}

	// Blob (optional)
	if bc, ok := h.backend.(blobHealthChecker); ok {
		if err := h.checkBlobCached(ctx, bc); err != nil {
			slog.Warn("health: blob check failed", "err", err)
			resp.Blob = componentStatus{Status: "error", Error: classifyHealthErr(err)}
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
			slog.Warn("health: redis ping failed", "err", err)
			resp.Redis = componentStatus{Status: "error", Error: classifyHealthErr(err)}
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

// checkBlobCached returns the blob reachability status, reusing a successful result for
// blobCheckCacheTTL (failures never cached; the mutex collapses concurrent probes into one).
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

// --- System & Keys ---
// registerRoutes wires the system/settings and API-key route groups.
func (h *SystemHandler) registerRoutes(r chi.Router) {
	r.Route("/api/system", func(r chi.Router) {
		r.Get("/settings", h.handleGetSettings)
		r.Post("/settings", h.handleUpdateSettings)
		r.Get("/settings/org/{id}", h.handleGetOrgSettings)     // member-only
		r.Post("/settings/org/{id}", h.handleUpdateOrgSettings) // admin-only
		r.Get("/info", h.handleAppInfo)
		r.Post("/log-error", h.handleLogError)
	})

	r.Route("/api/keys", func(r chi.Router) {
		r.Post("/save", h.handleSaveApiKey)
		r.Post("/has", h.handleHasApiKey)
		r.Post("/delete", h.handleDeleteApiKey)
	})
}
