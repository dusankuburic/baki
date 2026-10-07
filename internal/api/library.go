package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// FlowNotifier broadcasts a flow-changed notification to connected WebSocket
// clients after a library flow is saved; nil in local mode (no hub).
type FlowNotifier interface {
	NotifyFlowChanged(flowID string, version int)
}

type LibraryHandler struct {
	libSvc   *service.LibraryService
	security *SecurityConfig
	backend  storageif.StorageBackend
	audit    *audit.Recorder
	notifier FlowNotifier
}

func NewLibraryHandler(libSvc *service.LibraryService, backend storageif.StorageBackend, security *SecurityConfig, recorder *audit.Recorder) *LibraryHandler {
	return &LibraryHandler{libSvc: libSvc, security: security, backend: backend, audit: recorder}
}

// SetFlowNotifier wires the WebSocket hub: library saves broadcast a
// flow.changed event to connected viewers.
func (h *LibraryHandler) SetFlowNotifier(n FlowNotifier) {
	h.notifier = n
}

// flowOrgID is the organisation of a flow header already in hand, for its audit row ("" without one).
func flowOrgID(f *storageif.FlowDocument) string {
	if f == nil {
		return ""
	}
	return f.OrganizationID
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
	// Tags ride on both the list and the GET: they were stored and filterable (?tags=) but
	// never serialized, so every reload showed "No tags".
	Tags []string `json:"tags,omitempty"`
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
		Tags:             doc.Tags,
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
		Tags:             doc.Tags,
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

	// Tag ANY-of filter: ?tags=critical,prod — empty element ignored.
	var tagFilter []string
	if raw := q.Get("tags"); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tagFilter = append(tagFilter, t)
			}
		}
	}

	// One page call resolves the caller's org memberships once and reuses
	// them for the list, total and per-flow permission flags.
	page, err := h.libSvc.ListLibraryFlowsPage(r.Context(), userID, orgID, scope, query, sort, limit, offset, tagFilter)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	docs := page.Docs

	// Resolve all owner display names in ONE backend round trip (avoids N+1).
	ownerIDs := make([]string, 0, len(docs))
	for _, d := range docs {
		if d.OwnerID != "" {
			ownerIDs = append(ownerIDs, d.OwnerID)
		}
	}
	ownerNames := h.libSvc.ResolveOwnerNames(r.Context(), ownerIDs)

	// Batch-resolve per-flow permission flags with the roles the page
	// already loaded: zero role queries when the memberships are complete.
	perms := h.libSvc.BatchFlowPermissionsWithRoles(r.Context(), docs, userID, page.Memberships)

	items := make([]libraryFlow, len(docs))
	for i, d := range docs {
		p := perms[d.ID]
		items[i] = h.toLibraryFlowWithPerms(d, userID, ownerNames[d.OwnerID], p)
	}

	render.JSON(w, render.PagedResponse[libraryFlow]{
		Items:  items,
		Total:  page.Total,
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
	if !requireInstancePerm(w, r, h.security, service.InstanceWrite) {
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
		h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowSave, ResourceType: "flow", ResourceID: saved.ID, OrgID: saved.OrganizationID, Meta: map[string]string{"orgId": req.OrgID}})
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
	// The read check and the capability flags (toLibraryFlow) are two decisions about one flow:
	// one role lookup and one collaborator read serve both (service.WithFlowAuthzMemo).
	ctx := service.WithFlowAuthzMemo(r.Context())

	// Metadata only: the detail panel never renders the content, so this reads
	// the header instead of the whole document.
	doc, err := h.libSvc.GetLibraryFlowMetaForUser(ctx, id)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	out := h.toLibraryFlow(ctx, doc, userID, h.libSvc.ResolveOwnerName(ctx, doc.OwnerID))
	// Best-effort: a missing or failed health lookup just omits the fields —
	// the detail view degrades to "not analyzed yet" rather than failing.
	if health, herr := h.libSvc.FlowHealth(ctx, doc.ID); herr == nil && health != nil {
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
	ctx := service.WithFlowAuthzMemo(r.Context())

	// Header first: the ETag is the flow's version, so a client holding current content gets a 304
	// without the content read or its compression (brotli was two thirds of CPU under a 40-user read
	// mix). Authorization still happens before the cached body is confirmed; one (flow, version)'s
	// content is the same for every reader.
	meta, err := h.libSvc.GetLibraryFlowMetaForUser(ctx, id)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	if etag := flowContentETag(meta); etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("ETag", etag)
		w.Header().Set("X-Flow-Version", strconv.Itoa(meta.Version))
		w.WriteHeader(http.StatusNotModified)
		return
	}

	doc, err := h.libSvc.GetLibraryFlowForUser(ctx, id)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Tag the body actually sent (a save between the two reads bumps the
	// version; the newer body must not travel under the older tag).
	w.Header().Set("ETag", flowContentETag(doc))
	w.Header().Set("X-Flow-Version", strconv.Itoa(doc.Version))
	payload := doc.Content
	if len(payload) == 0 {
		payload = []byte("null")
	}
	_, _ = w.Write(payload)
}

// flowContentETag is the strong validator of a flow's content: the version bumps on every save,
// and the content of one (flow, version) never changes (the document cache relies on this too).
func flowContentETag(doc *storageif.FlowDocument) string {
	return `"` + doc.ID + "-v" + strconv.Itoa(doc.Version) + `"`
}

// etagMatches reports whether an If-None-Match header names etag: a list of quoted tags (weak
// prefixes ignored — the content is byte-identical for a given version) or "*".
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	for _, part := range strings.Split(ifNoneMatch, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			return true
		}
		part = strings.TrimPrefix(part, "W/")
		if part == etag {
			return true
		}
	}
	return false
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

	deleted, err := h.libSvc.DeleteLibraryFlow(r.Context(), id)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowDelete, ResourceType: "flow", ResourceID: id, OrgID: flowOrgID(deleted)})
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
	// Three decisions about one flow (read gate, write gate, capability flags) would issue the
	// role lookup and collaborator read three times each; the memo makes them one of each.
	ctx := service.WithFlowAuthzMemo(r.Context())

	// Read-gate before the write-gate: callers without read access get a 403 without learning the
	// flow's name. A full load, not the header — a header would blank the content of a name-only
	// update since SaveFlow rewrites every column.
	existing, err := h.libSvc.GetLibraryFlowForUser(ctx, id)
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
	// OCC: in cloud mode the client MUST send the version they loaded; version=0 on an existing
	// flow bypasses OCC and could silently overwrite another user's changes.
	if h.security.JWTEnabled && existing.Version > 0 && req.Version == 0 {
		render.Error(w, fmt.Errorf("version is required for updates — reload the flow and try again"), http.StatusConflict)
		return
	}
	existing.Version = req.Version

	if err := h.libSvc.UpdateLibraryFlow(ctx, existing); err != nil {
		render.Error(w, err, 0)
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(existing.ID, existing.Version)
	}
	render.JSON(w, h.toLibraryFlow(ctx, existing, userID, h.libSvc.ResolveOwnerName(ctx, existing.OwnerID)))
}

// Mount registers the /api/library group: library CRUD here; version
// history and the portfolio view live with their handlers.
func (h *LibraryHandler) Mount(rb *route.Builder) {
	rb.Route("/api/library", func(rb *route.Builder) {
		rb.Get("/", h.handleLibraryList)
		rb.Post("/", h.handleLibraryCreate, route.Idempotent())
		rb.Get("/portfolio", h.handlePortfolio)
		rb.Route("/{id}", func(rb *route.Builder) {
			rb.Get("/", h.handleLibraryGet)
			rb.Put("/", h.handleLibraryUpdate)
			rb.Delete("/", h.handleLibraryDelete)
			// Streams the stored blob after the authz check: outside the request transaction.
			rb.Get("/content", h.handleLibraryGetContent, route.NoTx())
			h.registerVersionRoutes(rb)
		})
	})
}
