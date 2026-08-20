package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
	"pad-core/parser"
)

// FlowNotifier broadcasts a flow-changed notification to connected WebSocket
// clients after a library flow is saved. In local mode (no WebSocket hub)
// the value is nil and the notification is skipped.
type FlowNotifier interface {
	NotifyFlowChanged(flowID string, version int)
}

type LibraryHandler struct {
	libSvc   *service.LibraryService
	security *SecurityConfig
	backend  storageif.StorageBackend
	notifier FlowNotifier
}

func NewLibraryHandler(libSvc *service.LibraryService, backend storageif.StorageBackend, security *SecurityConfig) *LibraryHandler {
	return &LibraryHandler{libSvc: libSvc, security: security, backend: backend}
}

// handlePortfolio returns the org-wide governance portfolio: every flow the
// caller can access (optionally filtered by ?orgId=), ranked worst-health-first.
// @Summary      Flow portfolio view
// @Description  handlePortfolio returns the org-wide governance portfolio: every flow the caller can access (optionally filtered by ?orgId=), ranked worst-health-first.
// @Tags         library
// @Produce      json
// @Success      200 {object} map[string]interface{} "Portfolio"
// @Router       /api/library/portfolio [get]
func (h *LibraryHandler) handlePortfolio(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	orgID := r.URL.Query().Get("orgId")
	p, err := h.libSvc.BuildPortfolio(r.Context(), userID, orgID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, p)
}

// SetFlowNotifier wires the WebSocket hub so that library saves broadcast
// a flow.changed event to all connected viewers.
func (h *LibraryHandler) SetFlowNotifier(n FlowNotifier) {
	h.notifier = n
}

type libraryFlow struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description,omitempty"`
	OwnerID          string    `json:"ownerId"`
	OwnerDisplayName string    `json:"ownerDisplayName,omitempty"`
	OrganizationID   string    `json:"organizationId,omitempty"`
	IsSharedWithMe   bool      `json:"isSharedWithMe"`
	CanEdit          bool      `json:"canEdit"`
	CanDelete        bool      `json:"canDelete"`
	CanShare         bool      `json:"canShare"`
	BlockCount       int       `json:"blockCount"`
	SubflowCount     int       `json:"subflowCount"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Version          int       `json:"version"`
	// Health fields are populated only by the single-flow GET, not the list.
	HealthScore  *int `json:"healthScore,omitempty"`
	ErrorCount   *int `json:"errorCount,omitempty"`
	WarningCount *int `json:"warningCount,omitempty"`
}

func (h *LibraryHandler) toLibraryFlow(ctx context.Context, doc *storageif.FlowDocument, requestingUserID, ownerName string) libraryFlow {
	canEdit, canDelete, canShare := true, true, true
	if h.security != nil && h.security.JWTEnabled {
		// One pass instead of three separate CheckFlowAccess calls per flow.
		canEdit, canDelete, canShare = h.libSvc.FlowPermissions(ctx, doc, requestingUserID)
	}

	return libraryFlow{
		ID:               doc.ID,
		Name:             doc.Name,
		Description:      doc.Description,
		OwnerID:          doc.OwnerID,
		OwnerDisplayName: ownerName,
		OrganizationID:   doc.OrganizationID,
		IsSharedWithMe:   doc.OwnerID != requestingUserID,
		CanEdit:          canEdit,
		CanDelete:        canDelete,
		CanShare:         canShare,
		BlockCount:       doc.Metadata.BlockCount,
		SubflowCount:     doc.Metadata.SubflowCount,
		UpdatedAt:        doc.UpdatedAt,
		Version:          doc.Version,
	}
}

func (h *LibraryHandler) toLibraryFlowWithPerms(doc *storageif.FlowDocument, requestingUserID, ownerName string, perms service.PermFlags) libraryFlow {
	return libraryFlow{
		ID:               doc.ID,
		Name:             doc.Name,
		Description:      doc.Description,
		OwnerID:          doc.OwnerID,
		OwnerDisplayName: ownerName,
		OrganizationID:   doc.OrganizationID,
		IsSharedWithMe:   doc.OwnerID != requestingUserID,
		CanEdit:          perms.CanEdit,
		CanDelete:        perms.CanDelete,
		CanShare:         perms.CanShare,
		BlockCount:       doc.Metadata.BlockCount,
		SubflowCount:     doc.Metadata.SubflowCount,
		UpdatedAt:        doc.UpdatedAt,
		Version:          doc.Version,
	}
}

// @Summary      List library flows
// @Description  Returns a list of flow documents stored in the library, with optional filtering by organization and search query.
// @Tags         library
// @Param        orgId query string false "orgId"
// @Param        q query string false "q"
// @Param        limit query integer false "limit"
// @Param        offset query integer false "offset"
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/library [get]
func (h *LibraryHandler) handleLibraryList(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)

	q := r.URL.Query()
	orgID := q.Get("orgId")
	query := q.Get("q")
	sort := q.Get("sort")
	scope := service.LibraryScope(q.Get("scope"))
	limit, ok := clampListLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	offset, ok := clampListOffset(w, q.Get("offset"))
	if !ok {
		return
	}

	docs, err := h.libSvc.ListLibraryFlows(r.Context(), userID, orgID, scope, query, sort, limit, offset)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	// Resolve all owner display names in ONE backend round trip (avoids N+1).
	ownerIDs := make([]string, 0, len(docs))
	for _, d := range docs {
		if d.OwnerID != "" {
			ownerIDs = append(ownerIDs, d.OwnerID)
		}
	}
	ownerNames := h.libSvc.ResolveOwnerNames(r.Context(), ownerIDs)

	// Batch-resolve all per-flow permission flags in O(orgs + 1) queries
	// instead of O(flows).
	perms := h.libSvc.BatchFlowPermissions(r.Context(), docs, userID)

	items := make([]libraryFlow, len(docs))
	for i, d := range docs {
		p := perms[d.ID]
		items[i] = h.toLibraryFlowWithPerms(d, userID, ownerNames[d.OwnerID], p)
	}

	total, err := h.libSvc.CountLibraryFlows(r.Context(), userID, orgID, scope, query)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, render.PagedResponse[libraryFlow]{
		Items:  items,
		Total:  total,
		Offset: offset,
		Limit:  limit,
	})
}

// @Summary      Create library flow
// @Description  Saves a new flow document to the library.
// @Tags         library
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      201 {object} map[string]interface{} "Created"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/library [post]
func (h *LibraryHandler) handleLibraryCreate(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleMember) {
		return
	}

	userID := h.security.CallerID(r)

	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		OrgID       string          `json:"orgId"`
		Content     json.RawMessage `json:"content"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Name == "" {
		render.Error(w, fmt.Errorf("name is required"), http.StatusBadRequest)
		return
	}

	doc := storageif.FlowDocument{
		Name:        req.Name,
		Description: req.Description,
		Content:     req.Content,
	}
	saved, err := h.libSvc.CreateLibraryFlow(r.Context(), userID, req.OrgID, doc)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	if req.OrgID != "" {
		logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowSave, "flow", saved.ID, map[string]string{"orgId": req.OrgID})
	}
	render.JSONStatus(w, http.StatusCreated, h.toLibraryFlow(r.Context(), saved, userID, h.libSvc.ResolveOwnerName(r.Context(), saved.OwnerID)))
}

// @Summary      Get library flow metadata
// @Description  Returns the metadata for a specific library flow.
// @Tags         library
// @Param        id path string true "id"
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/library/{id} [get]
func (h *LibraryHandler) handleLibraryGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	doc, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	out := h.toLibraryFlow(r.Context(), doc, userID, h.libSvc.ResolveOwnerName(r.Context(), doc.OwnerID))
	// Best-effort: a missing or failed health lookup just omits the fields —
	// the detail view degrades to "not analyzed yet" rather than failing.
	if health, herr := h.libSvc.FlowHealth(r.Context(), doc.ID); herr == nil && health != nil {
		hs, e, wn := health.HealthScore, health.Errors, health.Warnings
		out.HealthScore = &hs
		out.ErrorCount = &e
		out.WarningCount = &wn
	}
	render.JSON(w, out)
}

// @Summary      Get library flow content
// @Description  Returns the raw JSON content of a specific library flow.
// @Tags         library
// @Param        id path string true "id"
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/library/{id}/content [get]
func (h *LibraryHandler) handleLibraryGetContent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	doc, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Flow-Version", strconv.Itoa(doc.Version))
	payload := doc.Content
	if len(payload) == 0 {
		payload = []byte("null")
	}
	_, _ = w.Write(payload)
}

// @Summary      Delete library flow
// @Description  Deletes a specific flow document from the library. Only the owner can delete.
// @Tags         library
// @Param        id path string true "id"
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/library/{id} [delete]
func (h *LibraryHandler) handleLibraryDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	if err := h.libSvc.DeleteLibraryFlow(r.Context(), id, userID); err != nil {
		render.Error(w, err, 0)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowDelete, "flow", id, nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Update library flow
// @Description  Updates the name, description, or content of a specific library flow. Only the owner can update.
// @Tags         library
// @Param        id path string true "id"
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/library/{id} [put]
func (h *LibraryHandler) handleLibraryUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	// Read-gate before the write-gate inside UpdateLibraryFlow: callers without
	// read access get a 403 without learning the flow's name/description.
	existing, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Content     json.RawMessage `json:"content"`
		Version     int             `json:"version"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if len(req.Content) > 0 {
		existing.Content = req.Content
	}
	// OCC: in cloud mode, if the flow already has a version (existing.Version > 0
	// from the DB load), the client MUST send the version they loaded. version=0
	// on an existing flow means the client is bypassing OCC and could silently
	// overwrite another user's changes.
	if h.security.JWTEnabled && existing.Version > 0 && req.Version == 0 {
		render.Error(w, fmt.Errorf("version is required for updates — reload the flow and try again"), http.StatusConflict)
		return
	}
	existing.Version = req.Version

	if err := h.libSvc.UpdateLibraryFlow(r.Context(), existing, userID); err != nil {
		render.Error(w, err, 0)
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(existing.ID, existing.Version)
	}
	render.JSON(w, h.toLibraryFlow(r.Context(), existing, userID, h.libSvc.ResolveOwnerName(r.Context(), existing.OwnerID)))
}

// handleListFlowVersions returns the version history for a library flow.
// @Summary      List flow versions
// @Description  handleListFlowVersions returns the version history for a library flow.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Versions"
// @Router       /api/library/{id}/versions [get]
func (h *LibraryHandler) handleListFlowVersions(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.JSON(w, []storageif.FlowVersion{})
		return
	}

	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	if _, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID); err != nil {
		render.Error(w, err, 0)
		return
	}

	limit, ok := parseIntParam(w, r.URL.Query().Get("limit"), "limit", 0)
	if !ok {
		return
	}
	versions, err := h.backend.ListFlowVersions(r.Context(), id, limit)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, versions)
}

// handleSaveFlowVersion snapshots the current flow content as a new version.
// @Summary      Snapshot a flow version
// @Description  handleSaveFlowVersion snapshots the current flow content as a new version.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Accept       json
// @Produce      json
// @Success      201 {object} map[string]interface{} "Created"
// @Router       /api/library/{id}/versions [post]
func (h *LibraryHandler) handleSaveFlowVersion(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("versioning requires cloud storage"), http.StatusServiceUnavailable)
		return
	}

	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	doc, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	if err := h.libSvc.CanWrite(r.Context(), doc, userID); err != nil {
		render.Error(w, err, 0)
		return
	}

	var req struct {
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, fmt.Errorf("invalid request body: %w", err), http.StatusBadRequest)
		return
	}

	if len(req.Comment) > 500 {
		render.Error(w, fmt.Errorf("comment must be 500 characters or fewer"), http.StatusBadRequest)
		return
	}

	// The backend assigns v.Version atomically (Postgres: FOR UPDATE + MAX+1),
	// so it is not pre-computed here — a read-then-write would just race.
	v := &storageif.FlowVersion{
		ID:        uuid.NewString(),
		FlowID:    id,
		Comment:   req.Comment,
		Content:   doc.Content,
		Metadata:  doc.Metadata,
		CreatedBy: userID,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.backend.SaveFlowVersion(r.Context(), v); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowVersion, "flow", id, map[string]string{"version": strconv.Itoa(v.Version)})
	render.JSONStatus(w, http.StatusCreated, v)
}

// handleGetFlowVersion loads a specific historical version.
// @Summary      Get a flow version
// @Description  handleGetFlowVersion loads a specific historical version.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Param        vn path integer true "Version number"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Version"
// @Router       /api/library/{id}/versions/{vn} [get]
func (h *LibraryHandler) handleGetFlowVersion(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	if _, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID); err != nil {
		render.Error(w, err, 0)
		return
	}
	if h.backend == nil {
		render.Error(w, fmt.Errorf("versioning requires cloud storage"), http.StatusServiceUnavailable)
		return
	}

	vn, ok := parseIntParam(w, chi.URLParam(r, "vn"), "version", 0)
	if !ok {
		return
	}
	if vn <= 0 {
		render.Error(w, fmt.Errorf("invalid version: must be a positive integer"), http.StatusBadRequest)
		return
	}
	v, err := h.backend.LoadFlowVersion(r.Context(), id, vn)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, v)
}

// handleRestoreFlowVersion reverts the current flow's content to a historical
// version's content. The historical content is written as a new current
// version (SaveFlow bumps the version atomically), so the restore is itself
// recoverable via the version history. Requires editor access.
// @Summary      Restore a flow version
// @Description  handleRestoreFlowVersion reverts the current flow's content to a historical version's content. The historical content is written as a new current version (SaveFlow bumps the version atomically), so the restore is itself recoverable via the version history. Requires editor access.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Param        vn path integer true "Version number"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Restored"
// @Router       /api/library/{id}/versions/{vn}/restore [post]
func (h *LibraryHandler) handleRestoreFlowVersion(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("versioning requires cloud storage"), http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	doc, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	if err := h.libSvc.CanWrite(r.Context(), doc, userID); err != nil {
		render.Error(w, err, 0)
		return
	}

	vn, ok := parseIntParam(w, chi.URLParam(r, "vn"), "version", 0)
	if !ok {
		return
	}
	if vn <= 0 {
		render.Error(w, fmt.Errorf("invalid version: must be a positive integer"), http.StatusBadRequest)
		return
	}
	historical, err := h.backend.LoadFlowVersion(r.Context(), id, vn)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	if len(historical.Content) == 0 {
		render.Error(w, fmt.Errorf("version %d has no content to restore", vn), http.StatusBadRequest)
		return
	}

	// Overwrite the current content with the historical content. SaveFlow's OCC
	// guards against a concurrent edit; UpdateLibraryFlow re-checks editor
	// access and invalidates the cached search index.
	doc.Content = historical.Content
	if err := h.libSvc.UpdateLibraryFlow(r.Context(), doc, userID); err != nil {
		render.Error(w, err, 0)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionFlowRestore, "flow", id, map[string]string{"restored_from": strconv.Itoa(vn), "version": strconv.Itoa(doc.Version)})
	render.JSON(w, h.toLibraryFlow(r.Context(), doc, userID, h.libSvc.ResolveOwnerName(r.Context(), doc.OwnerID)))
}

// handleDiffFlowVersion returns the structural diff between a historical
// version and the current flow content. Read-only (viewer access). Uses
// parser.DiffFlows on the two unmarshaled FlowDocuments.
// @Summary      Diff version vs. current
// @Description  handleDiffFlowVersion returns the structural diff between a historical version and the current flow content. Read-only (viewer access). Uses parser.DiffFlows on the two unmarshaled FlowDocuments.
// @Tags         library
// @Param        id path string true "Flow ID"
// @Param        vn path integer true "Version number"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Diff"
// @Router       /api/library/{id}/versions/{vn}/diff [get]
func (h *LibraryHandler) handleDiffFlowVersion(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("versioning requires cloud storage"), http.StatusServiceUnavailable)
		return
	}
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	current, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	vn, ok := parseIntParam(w, chi.URLParam(r, "vn"), "version", 0)
	if !ok {
		return
	}
	if vn <= 0 {
		render.Error(w, fmt.Errorf("invalid version: must be a positive integer"), http.StatusBadRequest)
		return
	}
	historical, err := h.backend.LoadFlowVersion(r.Context(), id, vn)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	oldDoc, err := unmarshalFlowContent(historical.Content)
	if err != nil {
		render.Error(w, fmt.Errorf("parse historical version: %w", err), http.StatusInternalServerError)
		return
	}
	newDoc, err := unmarshalFlowContent(current.Content)
	if err != nil {
		render.Error(w, fmt.Errorf("parse current flow: %w", err), http.StatusInternalServerError)
		return
	}
	// DiffFlows(old, new): old = historical, new = current, so + = added since
	// the snapshot, - = removed since the snapshot.
	render.JSON(w, parser.DiffFlows(oldDoc, newDoc))
}

// unmarshalFlowContent deserializes a stored flow Content (a serialized
// models.FlowDocument) into the parser's FlowDocument for diffing.
func unmarshalFlowContent(content json.RawMessage) (*models.FlowDocument, error) {
	if len(content) == 0 {
		return &models.FlowDocument{}, nil
	}
	var doc models.FlowDocument
	if err := json.Unmarshal(content, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}
