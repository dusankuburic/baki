package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/errreport"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// ScanNowFunc triggers an out-of-band governance sweep (a manual run of the
// scanner's periodic pass). Nil when scanning isn't available (non-cloud); the
// admin endpoint reports 503 in that case. The run is async — the func returns
// once the sweep completes, but the HTTP handler does not block on it.
type ScanNowFunc func(ctx context.Context)

// manualRunTimeout caps an admin-triggered background scan so a stalled
// sweep can't leak a goroutine forever. Generous by design — a full library
// sweep over thousands of flows can take many minutes, and each flow is itself
// bounded internally (perFlowScanTimeout / sweepTimeout).
const manualRunTimeout = 30 * time.Minute

// runDetached runs an admin-triggered job on its own goroutine, bounded by
// manualRunTimeout and guarded against panics. The scheduled scanner loop
// recovers its own panics; the manual trigger did not, so a panic the nightly
// run would have logged and survived took the whole process down when an
// admin pressed "scan now". location is the pad_panics_total label (a closed
// set: the callers here), label the log text.
func runDetached(location, label string, run func(ctx context.Context)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), manualRunTimeout)
		defer cancel()
		defer func() {
			if rec := recover(); rec != nil {
				errreport.CapturePanic(ctx, location, rec, debug.Stack(), errreport.Attrs{"job": label})
			}
		}()
		slog.Info("admin: manual " + label + " triggered")
		run(ctx)
		slog.Info("admin: manual " + label + " completed")
	}()
}

type AdminHandler struct {
	backend  storageif.StorageBackend
	security *SecurityConfig
	runner   *MigrationRunner
	scanNow  ScanNowFunc
}

func NewAdminHandler(
	backend storageif.StorageBackend,
	security *SecurityConfig,
	runner *MigrationRunner,
	scanNow ScanNowFunc,
) *AdminHandler {
	return &AdminHandler{
		backend:  backend,
		security: security,
		runner:   runner,
		scanNow:  scanNow,
	}
}

// @Summary      List all users
// @Description  Returns a list of all registered users. Only available to system admins.
// @Tags         admin
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/admin/users/list [get]
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

// @Summary      Set a user role
// @Description  Changes the role of the user with the given ID (admin only).
// @Tags         admin
// @Accept       json
// @Produce      json
// @Param        id path string true "User ID"
// @Param        request body object true "Role payload"
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      403 {object} map[string]string "Forbidden"
// @Router       /api/admin/users/{id}/role [put]
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

// @Summary      List audit action names
// @Description  Returns every audit action string the server can emit, so the admin filter offers the full set instead of a hand-maintained subset.
// @Tags         admin
// @Produce      json
// @Success      200 {array} string "Actions"
// @Failure      403 {object} map[string]string "Forbidden"
// @Router       /api/admin/audit/actions [get]
func (h *AdminHandler) handleAdminAuditActions(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	render.JSON(w, AllAuditActions())
}

// auditEventView is the wire shape of an audit entry. The storage struct stays
// untagged because the on-disk spill queue persists it as JSON lines, so the
// camelCase keys the admin table reads (action, email, createdAt, meta) are
// built here. Before this the table received Action/Email/CreatedAt and showed
// blank cells with "Invalid Date" on every row.
type auditEventView struct {
	ID           string            `json:"id"`
	UserID       string            `json:"userId"`
	Email        string            `json:"email"`
	Action       string            `json:"action"`
	ResourceType string            `json:"resourceType"`
	ResourceID   string            `json:"resourceId"`
	IP           string            `json:"ip"`
	Meta         map[string]string `json:"meta"`
	CreatedAt    time.Time         `json:"createdAt"`
}

func toAuditEventViews(events []*storageif.AuditEvent) []auditEventView {
	out := make([]auditEventView, 0, len(events))
	for _, e := range events {
		out = append(out, auditEventView{
			ID: e.ID, UserID: e.UserID, Email: e.Email, Action: e.Action,
			ResourceType: e.ResourceType, ResourceID: e.ResourceID, IP: e.IP,
			Meta: e.Meta, CreatedAt: e.CreatedAt,
		})
	}
	return out
}

// @Summary      List audit log entries
// @Description  Returns the audit trail (logins, settings changes, admin actions), newest first. GET /api/admin/audit/actions lists the action values.
// @Tags         admin
// @Produce      json
// @Param        action query string false "Only this action (e.g. auth.login)"
// @Param        userId query string false "Only events by this user ID"
// @Param        limit query int false "Page size"
// @Param        offset query int false "Rows to skip"
// @Success      200 {array} map[string]interface{} "Audit entries"
// @Failure      400 {object} map[string]string "Invalid limit or offset"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      503 {object} map[string]string "No storage backend (local mode)"
// @Router       /api/admin/audit [get]
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
	render.JSON(w, toAuditEventViews(events))
}

// @Summary      Start data migration
// @Description  Copies flows, settings and chat conversations from a legacy filesystem-backed server's data directory (PAD_DATA_DIR) into the database (admin only, cloud mode). Idempotent and non-blocking; the status result lists what is not migrated. Users are not migrated, so flows whose recorded owner is absent here are assigned to the admin who starts the run (reported as FlowsAdopted).
// @Tags         admin
// @Produce      json
// @Success      200 {object} map[string]string "Started"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      409 {object} map[string]string "Conflict"
// @Failure      503 {object} map[string]string "Migration not configured"
// @Router       /api/admin/migration/start [post]
func (h *AdminHandler) handleMigrationStart(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if !h.runner.Enabled() {
		render.Error(w, fmt.Errorf("migration not configured: set PAD_DATA_DIR (and PAD_STORAGE=database) in cloud mode"), http.StatusServiceUnavailable)
		return
	}
	if !h.runner.StartAs(h.security.CallerID(r)) {
		render.Error(w, fmt.Errorf("migration already in progress"), http.StatusConflict)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionMigrationStart, "migration", "start", nil)
	render.JSON(w, map[string]string{"status": "started"})
}

// @Summary      Data migration status
// @Description  Returns the status/result of the most recent data migration (admin only).
// @Tags         admin
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      403 {object} map[string]string "Forbidden"
// @Router       /api/admin/migration/status [get]
func (h *AdminHandler) handleMigrationStatus(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	render.JSON(w, h.runner.Status())
}

// --- Power Platform connector auth ---

// --- Background-loop manual triggers ---
//
// The scanner normally runs on its own periodic loop (gated by
// PAD_SCAN_INTERVAL). This endpoint lets an admin demand an immediate run — useful after a policy change, a known upstream
// update, or when the loop is configured with a long interval. Both are
// fire-and-forget: a full sweep can take minutes, so the handler acknowledges
// with {started: true} and runs the work on a detached, bounded context.

// @Summary      Run environment scanner
// @Tags         admin
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/admin/scanner/scan [post]
func (h *AdminHandler) handleScannerScan(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleAdmin) {
		return
	}
	if h.scanNow == nil {
		render.Error(w, fmt.Errorf("governance scanner not configured: set PAD_SCAN_INTERVAL"), http.StatusServiceUnavailable)
		return
	}
	runDetached("admin.scan-now", "governance scan", h.scanNow)
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionScanNow, "scanner", "", nil)
	render.JSON(w, map[string]bool{"started": true})
}
