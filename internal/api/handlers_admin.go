package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/connector/padcloud"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// PadCloudAuth is the optional Power Platform device-flow auth capability. The
// admin uses it to connect the tenant once (device-code flow); the cached token
// then unblocks the periodic ingester. When nil (connector not configured) the
// admin endpoints report 503.
type PadCloudAuth interface {
	StartDeviceFlow(ctx context.Context) (*padcloud.DeviceAuthResponse, error)
	PollToken(ctx context.Context, deviceCode string) (*padcloud.AuthResult, error)
	AccessToken() string
}

// ScanNowFunc triggers an out-of-band governance sweep (a manual run of the
// scanner's periodic pass). Nil when scanning isn't available (non-cloud); the
// admin endpoint reports 503 in that case. The run is async — the func returns
// once the sweep completes, but the HTTP handler does not block on it.
type ScanNowFunc func(ctx context.Context)

// IngestNowFunc triggers an out-of-band PAD-cloud ingest pass. Nil when the
// connector isn't configured; the admin endpoint reports 503 in that case.
type IngestNowFunc func(ctx context.Context)

// manualRunTimeout caps an admin-triggered background scan/ingest so a stalled
// sweep can't leak a goroutine forever. Generous by design — a full library
// sweep over thousands of flows can take many minutes, and each flow is itself
// bounded internally (perFlowScanTimeout / sweepTimeout).
const manualRunTimeout = 30 * time.Minute

type AdminHandler struct {
	backend   storageif.StorageBackend
	security  *SecurityConfig
	runner    *MigrationRunner
	ppAuth    PadCloudAuth
	scanNow   ScanNowFunc
	ingestNow IngestNowFunc
}

func NewAdminHandler(
	backend storageif.StorageBackend,
	security *SecurityConfig,
	runner *MigrationRunner,
	ppAuth PadCloudAuth,
	scanNow ScanNowFunc,
	ingestNow IngestNowFunc,
) *AdminHandler {
	return &AdminHandler{
		backend:   backend,
		security:  security,
		runner:    runner,
		ppAuth:    ppAuth,
		scanNow:   scanNow,
		ingestNow: ingestNow,
	}
}

func (h *AdminHandler) handleAdminUserList(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	limit, ok := parseIntParam(w, q.Get("limit"), "limit", 50)
	if !ok {
		return
	}
	offset, ok := parseIntParam(w, q.Get("offset"), "offset", 0)
	if !ok {
		return
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	users, err := h.backend.ListUsers(r.Context(), limit, offset)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	// Fall back to a page-size approximation when a count query is unavailable.
	total, err := h.backend.CountUsers(r.Context())
	if err != nil {
		total = offset + len(users)
	}

	render.JSON(w, render.PagedResponse[*storageif.User]{
		Items:  users,
		Total:  total,
		Offset: offset,
		Limit:  limit,
	})
}

func (h *AdminHandler) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	id := chi.URLParam(r, "id")

	var req struct {
		Role string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	if !auth.Role(req.Role).IsValid() {
		render.Error(w, fmt.Errorf("invalid role: %s", req.Role), http.StatusBadRequest)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	// Prevent demoting the last admin
	if auth.Role(req.Role) != auth.RoleAdmin {
		admins, err := h.backend.ListAdmins(r.Context())
		if err != nil {
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		if len(admins) == 1 && admins[0].ID == id {
			render.Error(w, fmt.Errorf("cannot demote the last administrator"), http.StatusConflict)
			return
		}
	}

	if err := h.backend.UpdateUserRole(r.Context(), id, auth.Role(req.Role)); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Revoke the user's refresh tokens so they must re-authenticate and pick up
	// their new role. Without this, their existing access JWT (up to 15 min TTL)
	// retains the old role — a demoted admin keeps admin access until the token
	// expires. Revoking refresh tokens ensures no new access tokens can be
	// minted with the stale role.
	if rs, ok := h.backend.(RefreshTokenStore); ok {
		if err := rs.RevokeUserRefreshTokens(r.Context(), id); err != nil {
			slog.Warn("failed to revoke refresh tokens after role change", "userID", id, "error", err)
		}
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionRoleChange, "user", id, map[string]string{"new_role": req.Role})
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *AdminHandler) handleAdminAuditList(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if h.backend == nil {
		render.Error(w, fmt.Errorf("storage backend not available"), http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	limit, ok := parseIntParam(w, q.Get("limit"), "limit", 0)
	if !ok {
		return
	}
	offset, ok := parseIntParam(w, q.Get("offset"), "offset", 0)
	if !ok {
		return
	}
	filter := storageif.AuditFilter{
		UserID: q.Get("userId"),
		Action: q.Get("action"),
		Limit:  limit,
		Offset: offset,
	}

	events, err := h.backend.ListAuditEvents(r.Context(), filter)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, events)
}

func (h *AdminHandler) handleMigrationStart(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if !h.runner.Enabled() {
		render.Error(w, fmt.Errorf("migration not configured: set PAD_STORAGE_DATA_DIR (and PAD_STORAGE=database) in cloud mode"), http.StatusServiceUnavailable)
		return
	}
	if !h.runner.Start() {
		render.Error(w, fmt.Errorf("migration already in progress"), http.StatusConflict)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionMigrationStart, "migration", "start", nil)
	render.JSON(w, map[string]string{"status": "started"})
}

func (h *AdminHandler) handleMigrationStatus(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	render.JSON(w, h.runner.Status())
}

// --- Power Platform connector auth ---

func (h *AdminHandler) handlePPStartAuth(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if h.ppAuth == nil {
		render.Error(w, fmt.Errorf("power Platform connector not configured: set PAD_PP_TENANT_ID, PAD_PP_CLIENT_ID, PAD_PP_DATAVERSE_URL, PAD_PP_INGEST_INTERVAL"), http.StatusServiceUnavailable)
		return
	}
	res, err := h.ppAuth.StartDeviceFlow(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{
		"deviceCode":      res.DeviceCode,
		"userCode":        res.UserCode,
		"verificationUri": res.VerificationURI,
		"message":         res.Message,
	})
}

func (h *AdminHandler) handlePPPollAuth(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if h.ppAuth == nil {
		render.Error(w, fmt.Errorf("power Platform connector not configured"), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		DeviceCode string `json:"deviceCode"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := h.ppAuth.PollToken(r.Context(), req.DeviceCode)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Never expose the access token to the client (same as GitHub/Copilot) —
	// it is cached server-side; the client only needs status.
	render.JSON(w, map[string]string{
		"status": res.Status,
		"error":  res.Error,
	})
}

func (h *AdminHandler) handlePPStatus(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	connected := h.ppAuth != nil && h.ppAuth.AccessToken() != ""
	render.JSON(w, map[string]bool{"connected": connected})
}

// --- Background-loop manual triggers ---
//
// The scanner and ingester normally run on their own periodic loops (gated by
// PAD_SCAN_INTERVAL / PAD_PP_INGEST_INTERVAL). These endpoints let an admin
// demand an immediate run — useful after a policy change, a known upstream
// update, or when the loop is configured with a long interval. Both are
// fire-and-forget: a full sweep can take minutes, so the handler acknowledges
// with {started: true} and runs the work on a detached, bounded context.

func (h *AdminHandler) handleScannerScan(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if h.scanNow == nil {
		render.Error(w, fmt.Errorf("governance scanner not configured: set PAD_SCAN_INTERVAL"), http.StatusServiceUnavailable)
		return
	}
	run := h.scanNow
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), manualRunTimeout)
		defer cancel()
		slog.Info("admin: manual governance scan triggered")
		run(ctx)
		slog.Info("admin: manual governance scan completed")
	}()
	render.JSON(w, map[string]bool{"started": true})
}

func (h *AdminHandler) handleIngesterIngest(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if h.ingestNow == nil {
		render.Error(w, fmt.Errorf("PAD-cloud connector not configured: set PAD_PP_TENANT_ID, PAD_PP_CLIENT_ID, PAD_PP_DATAVERSE_URL, PAD_PP_INGEST_INTERVAL"), http.StatusServiceUnavailable)
		return
	}
	run := h.ingestNow
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), manualRunTimeout)
		defer cancel()
		slog.Info("admin: manual PAD-cloud ingest triggered")
		run(ctx)
		slog.Info("admin: manual PAD-cloud ingest completed")
	}()
	render.JSON(w, map[string]bool{"started": true})
}
