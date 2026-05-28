package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// libraryFlow is the API projection of a stored flow document.
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

func toLibraryFlow(doc *storageif.FlowDocument, requestingUserID, ownerDisplayName string) libraryFlow {
	return libraryFlow{
		ID:               doc.ID,
		Name:             doc.Name,
		Description:      doc.Description,
		OwnerID:          doc.OwnerID,
		OwnerDisplayName: ownerDisplayName,
		IsSharedWithMe:   doc.OwnerID != requestingUserID,
		BlockCount:       doc.Metadata.BlockCount,
		SubflowCount:     doc.Metadata.SubflowCount,
		UpdatedAt:        doc.UpdatedAt,
	}
}

// ownerDisplayName resolves a user ID to a human-readable name (email) via the
// user store. Returns "" when there is no backend or the user is unknown.
func (rt *Router) ownerDisplayName(r *http.Request, ownerID string) string {
	if ownerID == "" {
		return ""
	}
	backend := rt.app.StorageBackend()
	if backend == nil {
		return ""
	}
	if u, err := backend.LoadUserByID(r.Context(), ownerID); err == nil {
		return u.Email
	}
	return ""
}

// @Summary List library flows
// @Description Returns a list of flow documents stored in the library, with optional filtering by organization and search query.
// @Tags library
// @Produce json
// @Param orgId query string false "Organization ID"
// @Param q query string false "Search query"
// @Param limit query integer false "Limit"
// @Param offset query integer false "Offset"
// @Success 200 {array} libraryFlow
// @Failure 500 {object} map[string]string
// @Router /api/library [get]
func (rt *Router) handleLibraryList(w http.ResponseWriter, r *http.Request) {
	userID := rt.localUserID
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		userID = claims.UserID
	}

	q := r.URL.Query()
	orgID := q.Get("orgId")
	query := q.Get("q")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	docs, err := rt.app.ListLibraryFlows(userID, orgID, query, limit, offset)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	nameCache := make(map[string]string)
	result := make([]libraryFlow, len(docs))
	for i, d := range docs {
		dn, ok := nameCache[d.OwnerID]
		if !ok {
			dn = rt.ownerDisplayName(r, d.OwnerID)
			nameCache[d.OwnerID] = dn
		}
		result[i] = toLibraryFlow(d, userID, dn)
	}
	rt.sendJSON(w, result)
}

// @Summary Create library flow
// @Description Saves a new flow document to the library.
// @Tags library
// @Accept json
// @Produce json
// @Param request body object{name=string,description=string,orgId=string,content=json.RawMessage} true "Create Library Flow Request"
// @Success 201 {object} libraryFlow
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/library [post]
func (rt *Router) handleLibraryCreate(w http.ResponseWriter, r *http.Request) {
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}

	userID := rt.localUserID
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		userID = claims.UserID
	}

	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		OrgID       string          `json:"orgId"`
		Content     json.RawMessage `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		rt.sendError(w, errors.New("name is required"), http.StatusBadRequest)
		return
	}

	doc := storageif.FlowDocument{
		Name:        req.Name,
		Description: req.Description,
		Content:     req.Content,
	}
	saved, err := rt.app.CreateLibraryFlow(userID, req.OrgID, doc)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	rt.sendJSON(w, toLibraryFlow(saved, userID, rt.ownerDisplayName(r, saved.OwnerID)))
}

// @Summary Get library flow metadata
// @Description Returns the metadata for a specific library flow.
// @Tags library
// @Produce json
// @Param id path string true "Flow ID"
// @Success 200 {object} libraryFlow
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/library/{id} [get]
func (rt *Router) handleLibraryGet(w http.ResponseWriter, r *http.Request, id string) {
	userID := rt.localUserID
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		userID = claims.UserID
	}

	doc, err := rt.app.GetLibraryFlow(id)
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	// Access check: must be owner or org member (org check is approximate here).
	if rt.jwtEnabled && doc.OwnerID != "" && doc.OwnerID != userID && doc.OrganizationID == "" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	rt.sendJSON(w, toLibraryFlow(doc, userID, rt.ownerDisplayName(r, doc.OwnerID)))
}

// @Summary Get library flow content
// @Description Returns the raw JSON content of a specific library flow.
// @Tags library
// @Produce json
// @Param id path string true "Flow ID"
// @Success 200 {object} map[string]any
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/library/{id}/content [get]
func (rt *Router) handleLibraryGetContent(w http.ResponseWriter, r *http.Request, id string) {
	userID := rt.localUserID
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		userID = claims.UserID
	}

	doc, err := rt.app.GetLibraryFlow(id)
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	// Access check
	if rt.jwtEnabled && doc.OwnerID != "" && doc.OwnerID != userID && doc.OrganizationID == "" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Return the raw stored flow-document JSON (what the frontend's getContent expects),
	// not the storage wrapper.
	w.Header().Set("Content-Type", "application/json")
	if len(doc.Content) == 0 {
		_, _ = w.Write([]byte("null"))
		return
	}
	_, _ = w.Write(doc.Content)
}

// @Summary Delete library flow
// @Description Deletes a specific flow document from the library. Only the owner can delete.
// @Tags library
// @Produce json
// @Param id path string true "Flow ID"
// @Success 200 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/library/{id} [delete]
func (rt *Router) handleLibraryDelete(w http.ResponseWriter, r *http.Request, id string) {
	userID := rt.localUserID
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		userID = claims.UserID
	}

	doc, err := rt.app.GetLibraryFlow(id)
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}

	// Only the owner may delete.
	if rt.jwtEnabled && doc.OwnerID != "" && doc.OwnerID != userID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if err := rt.app.DeleteLibraryFlow(id); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Update library flow
// @Description Updates the name, description, or content of a specific library flow. Only the owner can update.
// @Tags library
// @Accept json
// @Produce json
// @Param id path string true "Flow ID"
// @Param request body object{name=string,description=string,content=json.RawMessage} true "Update Library Flow Request"
// @Success 200 {object} libraryFlow
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/library/{id} [put]
func (rt *Router) handleLibraryUpdate(w http.ResponseWriter, r *http.Request, id string) {
	userID := rt.localUserID
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		userID = claims.UserID
	}

	// Verify the flow exists and the caller owns it.
	existing, err := rt.app.GetLibraryFlow(id)
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	// In cloud mode the caller must own the flow. Empty-owner (legacy) flows are
	// not world-writable.
	if rt.jwtEnabled && existing.OwnerID != userID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Content     json.RawMessage `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}

	// Apply updates over the existing document.
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if len(req.Content) > 0 {
		existing.Content = req.Content
	}

	if err := rt.app.UpdateLibraryFlow(existing); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, toLibraryFlow(existing, userID, rt.ownerDisplayName(r, existing.OwnerID)))
}

// handleLibraryItem routes requests for /api/library/:id paths.
func (rt *Router) handleLibraryItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/library/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	if id, ok := strings.CutSuffix(path, "/content"); ok {
		rt.handleLibraryGetContent(w, r, id)
		return
	}

	id := path
	switch r.Method {
	case http.MethodGet:
		rt.handleLibraryGet(w, r, id)
	case http.MethodPut:
		rt.handleLibraryUpdate(w, r, id)
	case http.MethodDelete:
		rt.handleLibraryDelete(w, r, id)
	default:
		http.NotFound(w, r)
	}
}
