package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"pad-analyzer/internal/api/render"
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

	readyMu       sync.Mutex
	readyFailures int

	blobCheckMu   sync.Mutex
	blobCheckedAt time.Time // zero when the last check failed (never cache failures)
}

func NewSystemHandler(sysSvc *service.SystemService, security *SecurityConfig, backend storageif.StorageBackend) *SystemHandler {
	return &SystemHandler{sysSvc: sysSvc, security: security, backend: backend}
}

func (h *SystemHandler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	settings, err := h.sysSvc.GetUserSettings(r.Context(), userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, settings)
}

func (h *SystemHandler) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req models.AppSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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
func (h *SystemHandler) handleUpdateOrgSettings(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "id")
	if !requireOrgAdmin(w, r, h.security, orgID) {
		return
	}
	var req models.AppSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := h.sysSvc.UpdateOrgSettings(r.Context(), orgID, req); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionSettingsChange, "org", orgID, nil)
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

func (h *SystemHandler) handleLiveness(w http.ResponseWriter, r *http.Request) {
	// Liveness is intentionally cheap — it only proves the process is alive.
	// Do NOT add DB checks here; a slow DB would cause the container to restart
	// in a loop, which is worse than leaving it running and letting readiness handle it.
	render.JSON(w, map[string]string{"status": "ok"})
}

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

		if checkErr != nil {
			h.readyMu.Lock()
			h.readyFailures++
			failed := h.readyFailures >= readinessFailureThreshold
			h.readyMu.Unlock()
			if failed {
				render.Error(w, errors.New("database unavailable"), http.StatusServiceUnavailable)
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
