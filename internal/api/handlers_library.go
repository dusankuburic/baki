package api

import (
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
)

type LibraryHandler struct {
	libSvc   *service.LibraryService
	security *SecurityConfig
	backend  storageif.StorageBackend
}

func NewLibraryHandler(libSvc *service.LibraryService, security *SecurityConfig) *LibraryHandler {
	return &LibraryHandler{libSvc: libSvc, security: security}
}

func (h *LibraryHandler) SetBackend(b storageif.StorageBackend) { h.backend = b }

type libraryFlow struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description,omitempty"`
	OwnerID          string    `json:"ownerId"`
	OwnerDisplayName string    `json:"ownerDisplayName,omitempty"`
	IsSharedWithMe   bool      `json:"isSharedWithMe"`
	BlockCount       int       `json:"blockCount"`
	SubflowCount     int       `json:"subflowCount"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// toLibraryFlow builds the list/response DTO. ownerName must be pre-resolved by
// the caller (use ResolveOwnerName for single items, ResolveOwnerNames for lists)
// so that list endpoints don't issue one user query per row (N+1).
func (h *LibraryHandler) toLibraryFlow(doc *storageif.FlowDocument, requestingUserID, ownerName string) libraryFlow {
	return libraryFlow{
		ID:               doc.ID,
		Name:             doc.Name,
		Description:      doc.Description,
		OwnerID:          doc.OwnerID,
		OwnerDisplayName: ownerName,
		IsSharedWithMe:   doc.OwnerID != requestingUserID,
		BlockCount:       doc.Metadata.BlockCount,
		SubflowCount:     doc.Metadata.SubflowCount,
		UpdatedAt:        doc.UpdatedAt,
	}
}

func (h *LibraryHandler) handleLibraryList(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)

	q := r.URL.Query()
	orgID := q.Get("orgId")
	query := q.Get("q")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	docs, err := h.libSvc.ListLibraryFlows(r.Context(), userID, orgID, query, limit, offset)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
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

	items := make([]libraryFlow, len(docs))
	for i, d := range docs {
		items[i] = h.toLibraryFlow(d, userID, ownerNames[d.OwnerID])
	}
	render.JSON(w, render.PagedResponse[libraryFlow]{
		Items:  items,
		Total:  offset + len(items), // approximation; replace when storage adds COUNT
		Offset: offset,
		Limit:  limit,
	})
}

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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	render.JSON(w, h.toLibraryFlow(saved, userID, h.libSvc.ResolveOwnerName(r.Context(), saved.OwnerID)))
}

func (h *LibraryHandler) handleLibraryGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	doc, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, h.toLibraryFlow(doc, userID, h.libSvc.ResolveOwnerName(r.Context(), doc.OwnerID)))
}

func (h *LibraryHandler) handleLibraryGetContent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	doc, err := h.libSvc.GetLibraryFlowForUser(r.Context(), id, userID)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	payload := doc.Content
	if len(payload) == 0 {
		payload = []byte("null")
	}
	_, _ = w.Write(payload)
}

func (h *LibraryHandler) handleLibraryDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	if err := h.libSvc.DeleteLibraryFlow(r.Context(), id, userID); err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *LibraryHandler) handleLibraryUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)

	existing, err := h.libSvc.GetLibraryFlow(r.Context(), id)
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Content     json.RawMessage `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
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

	if err := h.libSvc.UpdateLibraryFlow(r.Context(), existing, userID); err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, h.toLibraryFlow(existing, userID, h.libSvc.ResolveOwnerName(r.Context(), existing.OwnerID)))
}

// handleListFlowVersions returns the version history for a library flow.
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

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	versions, err := h.backend.ListFlowVersions(r.Context(), id, limit)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, versions)
}

// handleSaveFlowVersion snapshots the current flow content as a new version.
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

	var req struct {
		Comment string `json:"comment"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if len(req.Comment) > 500 {
		render.Error(w, fmt.Errorf("comment must be 500 characters or fewer"), http.StatusBadRequest)
		return
	}

	existing, err := h.backend.ListFlowVersions(r.Context(), id, 1)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	nextVersion := 1
	if len(existing) > 0 {
		nextVersion = existing[0].Version + 1
	}

	v := &storageif.FlowVersion{
		ID:        uuid.NewString(),
		FlowID:    id,
		Version:   nextVersion,
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
	logAudit(r.Context(), h.backend, r, AuditActionFlowVersion, "flow", id, map[string]string{"version": strconv.Itoa(nextVersion)})
	w.WriteHeader(http.StatusCreated)
	render.JSON(w, v)
}

// handleGetFlowVersion loads a specific historical version.
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

	vn, _ := strconv.Atoi(chi.URLParam(r, "vn"))
	v, err := h.backend.LoadFlowVersion(r.Context(), id, vn)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, v)
}
