package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/drain"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// ScanNowFunc starts an out-of-band governance sweep (a manual run of the scanner's periodic pass) in the
// background and reports whether it started (scanner.ScanNow): nil when it did, lifecycle.ErrJobHeld while a sweep
// runs on any replica, lifecycle.ErrJobStopped once the scanner stopped, another error when the sweep's lock or the
// scanner is unavailable. The sweep is the scanner's job: bounded, panic-safe and cancelled when the scanner stops.
// Nil when there is no server store to sweep (the endpoint reports 503).
type ScanNowFunc func(ctx context.Context) error

// scanNowLockWait bounds the request's wait for the sweep's cross-replica lock, normally one database round trip.
const scanNowLockWait = 5 * time.Second

// The codes of a scan-now that did not start.
const (
	// codeScanAlreadyRunning: a sweep runs on this or another replica; its results are the ones asked for.
	codeScanAlreadyRunning = "SCAN_ALREADY_RUNNING"
	// codeScanUnavailable: no store to sweep, or the sweep's lock could not be asked for; try again.
	codeScanUnavailable = "SCAN_UNAVAILABLE"
)

type AdminHandler struct {
	audit    *audit.Recorder
	security *SecurityConfig
	runner   *MigrationRunner
	scanNow  ScanNowFunc
	authz    *service.AuthzService
	users    *service.UserAdminService
}

func NewAdminHandler(
	recorder *audit.Recorder,
	security *SecurityConfig,
	runner *MigrationRunner,
	scanNow ScanNowFunc,
	authz *service.AuthzService,
	users *service.UserAdminService,
) *AdminHandler {
	return &AdminHandler{
		audit:    recorder,
		security: security,
		runner:   runner,
		scanNow:  scanNow,
		authz:    authz,
		users:    users,
	}
}

// maxUserSearchLen bounds the free-text user filter so a pathological query
// string never reaches the SQL planner.
const maxUserSearchLen = 200

// @Summary      List users
// @Description  Returns one page of registered users, newest first, with the number of users matching the filters (total). hasMore reports users after the page, and nextCursor (absent on the last page) is the ?cursor= of the next one, with the same filters; a page with a cursor ignores offset. Only available to system admins.
// @Tags         admin
// @Produce      json
// @Param        limit  query int    false "Page size (default 50, max 100)"
// @Param        offset query int    false "Rows to skip, at most 10 000 without a cursor (a larger one is a 400; ignored with a cursor)"
// @Param        cursor query string false "The previous page's nextCursor"
// @Param        q      query string false "Case-insensitive substring of the email or display name; without a run of 3 letters or digits, a prefix of either"
// @Param        role   query string false "Only this role: admin, member, viewer, guest, or none (accounts without a valid role)"
// @Success      200 {object} render.PagedResponse[storageif.User] "One page of users"
// @Failure      400 {object} map[string]string "Invalid limit, offset (above 10 000 without a cursor), q, role or cursor"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Failure      503 {object} map[string]string "No storage backend (local mode)"
// @Router       /api/admin/users/list [get]
func (h *AdminHandler) handleAdminUserList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := parseIntParam(w, q.Get("limit"), "limit", 50)
	if !ok {
		return
	}
	offset, ok := listOffset(w, q.Get("offset"), q.Get("cursor") != "")
	if !ok {
		return
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	search := strings.TrimSpace(q.Get("q"))
	if len(search) > maxUserSearchLen {
		render.Error(w, fmt.Errorf("invalid %q parameter: at most %d characters", "q", maxUserSearchLen), http.StatusBadRequest)
		return
	}
	role := q.Get("role")
	if role != "" && role != storageif.UserRoleNone && !auth.Role(role).IsValid() {
		render.Error(w, fmt.Errorf("invalid role filter: %s", role), http.StatusBadRequest)
		return
	}
	filters := listFilterDigest(search, role)
	after, err := decodeListCursor(q.Get("cursor"), adminUserListOrder, filters)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	// One more than the page, to tell whether another follows. An empty page is [] on the wire, never null: the admin
	// table maps it.
	users, total, err := h.users.QueryUsers(r.Context(), storageif.UserQuery{Q: search, Role: role, Limit: limit + 1, Offset: offset, After: after})
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	hasMore, next := len(users) > limit, ""
	if hasMore {
		users = users[:limit]
		last := users[len(users)-1]
		next = encodeListCursor(adminUserListOrder, filters, last.CreatedAt, last.ID)
	}

	render.JSON(w, render.PagedResponse[*storageif.User]{
		Items:      users,
		Total:      total,
		Offset:     offset,
		Limit:      limit,
		HasMore:    offsetPageHasMore(hasMore, next, after != nil, offset, limit),
		NextCursor: next,
	})
}

// Page sizes of the audit log, as the store bounds them: no limit is the default, one above the maximum the maximum.
const (
	adminAuditDefaultLimit = 100
	adminAuditMaxLimit     = 500
)

// roleCounts is the per-role breakdown of userStatsView; "none" counts the
// accounts whose stored role is not one of the four valid roles.
type roleCounts struct {
	Admin  int `json:"admin"`
	Member int `json:"member"`
	Viewer int `json:"viewer"`
	Guest  int `json:"guest"`
	None   int `json:"none"`
}

// userStatsView is the wire shape of GET /api/admin/users/stats.
type userStatsView struct {
	Total      int        `json:"total"`
	ByRole     roleCounts `json:"byRole"`
	Unverified int        `json:"unverified"`
	Locked     int        `json:"locked"`
}

func toUserStatsView(s *storageif.UserStats) userStatsView {
	return userStatsView{
		Total: s.Total,
		ByRole: roleCounts{
			Admin:  s.ByRole[string(auth.RoleAdmin)],
			Member: s.ByRole[string(auth.RoleMember)],
			Viewer: s.ByRole[string(auth.RoleViewer)],
			Guest:  s.ByRole[string(auth.RoleGuest)],
			None:   s.ByRole[storageif.UserRoleNone],
		},
		Unverified: s.Unverified,
		Locked:     s.Locked,
	}
}

// @Summary      User statistics
// @Description  Counts every registered user by role (admin, member, viewer, guest, none) and by state (unverified email, locked out) for the admin overview. Only available to system admins.
// @Tags         admin
// @Produce      json
// @Success      200 {object} userStatsView "Counts"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Failure      503 {object} map[string]string "No storage backend (local mode)"
// @Router       /api/admin/users/stats [get]
func (h *AdminHandler) handleAdminUserStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.users.Stats(r.Context())
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, toUserStatsView(stats))
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

	// The last admin cannot be demoted; a changed user's refresh tokens are revoked so the new role takes effect.
	if err := h.users.ChangeRole(r.Context(), id, auth.Role(req.Role)); err != nil {
		if errors.Is(err, service.ErrLastAdmin) {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionRoleChange, ResourceType: "user", ResourceID: id, Meta: map[string]string{"new_role": req.Role}})
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
	render.JSON(w, audit.AllActions())
}

// auditEventView is the wire shape of an audit entry: the storage struct stays untagged (the
// on-disk spill queue persists it as JSON lines), so the camelCase keys are built here.
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
// @Param        userId query string false "Only events by this user ID, or whose subject is this user (sign-ins, failed sign-ins, lockouts)"
// @Param        limit query int false "Page size (default 100, max 500)"
// @Param        offset query int false "Rows to skip, at most 10 000 without a cursor (a larger one is a 400; ignored with a cursor)"
// @Param        cursor query string false "The X-Next-Cursor of the previous page"
// @Success      200 {array} map[string]interface{} "Audit entries"
// @Header       200 {string} X-Next-Cursor "When events follow the page: the ?cursor= of the next page, with the same filters"
// @Failure      400 {object} map[string]string "Invalid limit, offset (above 10 000 without a cursor) or cursor"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      503 {object} map[string]string "No storage backend (local mode)"
// @Router       /api/admin/audit [get]
func (h *AdminHandler) handleAdminAuditList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := parseIntParam(w, q.Get("limit"), "limit", 0)
	if !ok {
		return
	}
	offset, ok := listOffset(w, q.Get("offset"), q.Get("cursor") != "")
	if !ok {
		return
	}
	switch {
	case limit <= 0:
		limit = adminAuditDefaultLimit
	case limit > adminAuditMaxLimit:
		limit = adminAuditMaxLimit
	}
	filters := listFilterDigest(q.Get("userId"), q.Get("action"))
	after, err := decodeListCursor(q.Get("cursor"), adminAuditOrder, filters)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	filter := storageif.AuditFilter{
		UserID: q.Get("userId"),
		Action: q.Get("action"),
		Limit:  limit,
		Offset: offset,
		After:  after,
		// The operator asking "what happened to this account" wants the
		// sign-ins and lockouts too, which carry the user as the subject.
		IncludeSubject: true,
	}

	events, err := h.users.AuditEvents(r.Context(), filter)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// The answer stays a bare array (the admin page reads one), so the next cursor rides a header: after the last event
	// of a full page, when one event at least follows it (a one-event read after it tells; the store's largest page has
	// no room for one more).
	if len(events) == limit {
		last := events[len(events)-1]
		if next := encodeListCursor(adminAuditOrder, filters, last.CreatedAt, last.ID); next != "" {
			probe := filter
			probe.Limit, probe.Offset, probe.After = 1, 0, &storageif.PageCursor{At: last.CreatedAt, ID: last.ID}
			more, err := h.users.AuditEvents(r.Context(), probe)
			if err != nil {
				render.Error(w, err, http.StatusInternalServerError)
				return
			}
			if len(more) > 0 {
				w.Header().Set(nextCursorHeader, next)
			}
		}
	}
	render.JSON(w, toAuditEventViews(events))
}

// @Summary      Start data migration
// @Description  Copies flows, settings and chat conversations from a legacy filesystem-backed server's data directory (PAD_DATA_DIR) into the database (admin only, deployments with a server store). Idempotent and non-blocking; the status result lists what is not migrated. Users are not migrated, so flows whose recorded owner is absent here are assigned to the admin who starts the run (reported as FlowsAdopted).
// @Tags         admin
// @Produce      json
// @Success      200 {object} map[string]string "Started"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      409 {object} map[string]string "A migration is already running (on any replica)"
// @Failure      503 {object} map[string]string "Migration not configured, or its cross-replica lock is unavailable (try again)"
// @Router       /api/admin/migration/start [post]
func (h *AdminHandler) handleMigrationStart(w http.ResponseWriter, r *http.Request) {
	if !h.runner.Enabled() {
		render.Error(w, fmt.Errorf("migration not configured: set PAD_DATA_DIR on a deployment with a server store"), http.StatusServiceUnavailable)
		return
	}
	switch err := h.runner.StartAs(h.security.CallerID(r)); {
	case errors.Is(err, ErrMigrationRunning):
		render.Error(w, fmt.Errorf("migration already in progress"), http.StatusConflict)
		return
	case err != nil:
		// A database error is not "already in progress": the admin should retry, not wait for a run that is not there.
		render.Error(w, fmt.Errorf("migration could not start: its cross-replica lock is unavailable; try again"), http.StatusServiceUnavailable)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionMigrationStart, ResourceType: "migration", ResourceID: "start"})
	render.JSON(w, map[string]string{"status": "started"})
}

// @Summary      Data migration status
// @Description  Returns the status/result of the most recent data migration on any replica (admin only): the run and its result are recorded in the job state, so every replica answers alike.
// @Tags         admin
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      403 {object} map[string]string "Forbidden"
// @Router       /api/admin/migration/status [get]
func (h *AdminHandler) handleMigrationStatus(w http.ResponseWriter, r *http.Request) {
	render.JSON(w, h.runner.StatusContext(r.Context()))
}

// --- Power Platform connector auth ---

// --- Background-loop manual triggers ---
//
// The scanner normally runs on its periodic loop (PAD_SCAN_INTERVAL); this endpoint lets an admin
// demand an immediate run — after a policy change, or with a long interval configured.
// A full sweep can take minutes, so the handler takes the sweep's lock, starts it in the background
// and answers {started: true}, or says why it did not start.

// @Summary      Run environment scanner
// @Description  Starts a governance sweep now (admin only): the scanner's periodic pass, run in the background; its alerts arrive as a scheduled sweep's do. 200 {started: true} once it has started. A sweep already running on this or another replica is a 409 with code SCAN_ALREADY_RUNNING (its results are the ones asked for). A server that is stopping answers 503 SERVER_RESTARTING with Retry-After; no server store to sweep, or the sweep's cross-replica lock unavailable, is a 503 SCAN_UNAVAILABLE (try again).
// @Tags         admin
// @Produce      json
// @Success      200 {object} map[string]bool "Started: {started: true}"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      409 {object} render.ErrorResponse "A sweep is already running (code SCAN_ALREADY_RUNNING)"
// @Failure      503 {object} render.ErrorResponse "The sweep could not start (code SCAN_UNAVAILABLE), or the server is stopping (code SERVER_RESTARTING)"
// @Router       /api/admin/scanner/scan [post]
func (h *AdminHandler) handleScannerScan(w http.ResponseWriter, r *http.Request) {
	if h.scanNow == nil {
		render.ErrorWithCode(w, errors.New("governance scanner unavailable: no server store to sweep"), http.StatusServiceUnavailable, codeScanUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), scanNowLockWait)
	defer cancel()
	switch err := h.scanNow(ctx); {
	case errors.Is(err, lifecycle.ErrJobHeld):
		render.ErrorWithCode(w, errors.New("a governance scan is already running (on this or another server); its results arrive when it finishes"), http.StatusConflict, codeScanAlreadyRunning)
		return
	case errors.Is(err, lifecycle.ErrJobStopped):
		drain.Refuse(w)
		return
	case err != nil:
		// Not a sweep in progress: the lock could not be asked for (a database error), or the scanner cannot sweep.
		render.ErrorWithCode(w, fmt.Errorf("governance scan could not start: %w", err), http.StatusServiceUnavailable, codeScanUnavailable)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionScanNow, ResourceType: "scanner"})
	render.JSON(w, map[string]bool{"started": true})
}

// Mount registers the deployment administration routes (mountAdmin: the global admin role, and the admin scope
// for a scoped token). The system-health page in this tree is the SystemHandler's. The user and audit routes read
// the server store.
func (h *AdminHandler) Mount(rb *route.Builder) {
	mountAdmin(rb, h.authz, h.security, func(rb *route.Builder) {
		rb.Group(func(rb *route.Builder) {
			rb.Get("/users/list", h.handleAdminUserList)
			rb.Get("/users/stats", h.handleAdminUserStats)
			rb.Put("/users/{id}/role", h.handleAdminUserRole)
			rb.Get("/audit", h.handleAdminAuditList)
		}, route.Needs(capability.ServerStore))
		rb.Post("/migration/start", h.handleMigrationStart)
		rb.Get("/migration/status", h.handleMigrationStatus)
		rb.Get("/audit/actions", h.handleAdminAuditActions)
		rb.Post("/scanner/scan", h.handleScannerScan)
	})
}
