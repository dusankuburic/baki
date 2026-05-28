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
	IsSharedWithMe   bool      `json:"isSharedWithMe"`
	BlockCount       int       `json:"blockCount"`
	SubflowCount     int       `json:"subflowCount"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

func toLibraryFlow(doc *storageif.FlowDocument, requestingUserID string) libraryFlow {
	return libraryFlow{
		ID:             doc.ID,
		Name:           doc.Name,
		Description:    doc.Description,
		OwnerID:        doc.OwnerID,
		IsSharedWithMe: doc.OwnerID != requestingUserID,
		BlockCount:     doc.Metadata.BlockCount,
		SubflowCount:   doc.Metadata.SubflowCount,
		UpdatedAt:      doc.UpdatedAt,
	}
}

// GET /api/library
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

	result := make([]libraryFlow, len(docs))
	for i, d := range docs {
		result[i] = toLibraryFlow(d, userID)
	}
	rt.sendJSON(w, result)
}

// POST /api/library
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
	rt.sendJSON(w, toLibraryFlow(saved, userID))
}

// GET /api/library/:id
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

	rt.sendJSON(w, toLibraryFlow(doc, userID))
}

// GET /api/library/:id/content
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

	rt.sendJSON(w, doc)
}

// DELETE /api/library/:id
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

// PUT /api/library/:id
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
	if rt.jwtEnabled && existing.OwnerID != "" && existing.OwnerID != userID {
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
	rt.sendJSON(w, toLibraryFlow(existing, userID))
}

// handleLibraryItem routes requests for /api/library/:id paths.
func (rt *Router) handleLibraryItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/library/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	if strings.HasSuffix(path, "/content") {
		id := strings.TrimSuffix(path, "/content")
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
