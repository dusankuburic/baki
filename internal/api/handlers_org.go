package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/rag"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type OrgHandler struct {
	orgSvc    *collaboration.OrgService
	backend   storageif.StorageBackend
	knowledge *rag.KnowledgeService
	security  *SecurityConfig
}

func NewOrgHandler(orgSvc *collaboration.OrgService, backend storageif.StorageBackend, knowledge *rag.KnowledgeService, security *SecurityConfig) *OrgHandler {
	return &OrgHandler{orgSvc: orgSvc, backend: backend, knowledge: knowledge, security: security}
}

// maxKnowledgeUploadBytes caps the decoded document content. The UI advertises
// 1MB; enforce it server-side so a client can't ingest arbitrary content (which
// would be chunked and sent to the paid embeddings API).
const maxKnowledgeUploadBytes = 1 << 20 // 1 MiB

// maxKnowledgeUploadBodyBytes bounds the raw request body. It must be larger
// than maxKnowledgeUploadBytes because JSON-encoding the content escapes
// newlines/quotes/control chars, inflating the body well past the content size
// — so a tight cap would reject legitimate ~1MiB markdown before the precise
// content-length check below could return a clean 413.
const maxKnowledgeUploadBodyBytes = 4 << 20 // 4 MiB

func (h *OrgHandler) handleKnowledgeList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.knowledge == nil {
		render.Error(w, fmt.Errorf("knowledge service not configured"), http.StatusServiceUnavailable)
		return
	}
	// Only members of the org may read its knowledge base.
	if !h.orgSvc.IsMember(r.Context(), id, h.security.CallerID(r)) {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	docs, err := h.backend.ListKnowledgeDocuments(r.Context(), id)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, docs)
}

func (h *OrgHandler) handleKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.knowledge == nil {
		render.Error(w, fmt.Errorf("knowledge service not configured"), http.StatusServiceUnavailable)
		return
	}
	// Only org admins may modify the knowledge base.
	if !h.orgSvc.IsAdmin(r.Context(), id, h.security.CallerID(r)) {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}

	// Bound the request body before decoding so an oversized payload is rejected
	// without buffering it all into memory. The precise content limit is enforced
	// after decoding via len(req.Content) below.
	r.Body = http.MaxBytesReader(w, r.Body, maxKnowledgeUploadBodyBytes)
	var req struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if len(req.Content) > maxKnowledgeUploadBytes {
		render.Error(w, fmt.Errorf("document exceeds %d byte limit", maxKnowledgeUploadBytes), http.StatusRequestEntityTooLarge)
		return
	}

	if err := h.knowledge.AddDocument(r.Context(), h.security.KeyScope(r), id, req.Filename, req.Content); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *OrgHandler) handleKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	docID := chi.URLParam(r, "docId")
	// Only org admins may delete, and the delete is scoped to this org so a
	// docId from another org cannot be removed.
	if !h.orgSvc.IsAdmin(r.Context(), id, h.security.CallerID(r)) {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	if err := h.backend.DeleteKnowledgeDocument(r.Context(), id, docID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *OrgHandler) handleOrgList(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	orgs, _ := h.orgSvc.ListForUser(r.Context(), userID)
	render.JSON(w, orgs)
}

func (h *OrgHandler) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	userID := h.security.CallerID(r)
	org, err := h.orgSvc.Create(r.Context(), req.Name, userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, org)
}

func (h *OrgHandler) handleOrgGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Only members of the org may view its details.
	if !h.orgSvc.IsMember(r.Context(), id, h.security.CallerID(r)) {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	org, err := h.orgSvc.Get(r.Context(), id)
	if err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}
	render.JSON(w, org)
}

func (h *OrgHandler) handleOrgUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	if _, err := h.orgSvc.GetAndCheckAdmin(r.Context(), id, userID); err != nil {
		if errors.Is(err, collaboration.ErrNotOrgAdmin) {
			render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
			return
		}
		render.Error(w, err, http.StatusNotFound)
		return
	}
	updated, err := h.orgSvc.Update(r.Context(), id, req.Name)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, updated)
}

func (h *OrgHandler) handleOrgDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	userID := h.security.CallerID(r)
	if _, err := h.orgSvc.GetAndCheckAdmin(r.Context(), id, userID); err != nil {
		if errors.Is(err, collaboration.ErrNotOrgAdmin) {
			render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
			return
		}
		render.Error(w, err, http.StatusNotFound)
		return
	}
	if err := h.orgSvc.Delete(r.Context(), id); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *OrgHandler) handleOrgMemberList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Only members of the org may view its member list.
	if !h.orgSvc.IsMember(r.Context(), id, h.security.CallerID(r)) {
		render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
		return
	}
	if _, err := h.orgSvc.Get(r.Context(), id); err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}
	members, err := h.orgSvc.ListMembers(r.Context(), id)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, members)
}

func (h *OrgHandler) handleOrgMemberAdd(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Email  string `json:"email"`
		UserID string `json:"userId"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	// Check if org exists and caller is admin
	userID := h.security.CallerID(r)
	if _, err := h.orgSvc.GetAndCheckAdmin(r.Context(), id, userID); err != nil {
		if errors.Is(err, collaboration.ErrNotOrgAdmin) {
			render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
			return
		}
		render.Error(w, err, http.StatusNotFound)
		return
	}

	if h.backend == nil {
		render.Error(w, fmt.Errorf("user store not available"), http.StatusServiceUnavailable)
		return
	}

	var targetID string
	if req.UserID != "" {
		targetID = req.UserID
	} else if req.Email != "" {
		u, err := h.backend.LoadUserByEmail(r.Context(), req.Email)
		if err != nil {
			render.Error(w, fmt.Errorf("user not found"), http.StatusNotFound)
			return
		}
		targetID = u.ID
	} else {
		render.Error(w, fmt.Errorf("userId or email required"), http.StatusBadRequest)
		return
	}

	if err := h.orgSvc.AddMember(r.Context(), id, targetID, auth.Role(req.Role)); err != nil {
		if strings.Contains(err.Error(), "already a member") {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *OrgHandler) handleOrgMemberRemove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")

	callerID := h.security.CallerID(r)
	_, adminErr := h.orgSvc.GetAndCheckAdmin(r.Context(), id, callerID)
	if adminErr != nil {
		if !errors.Is(adminErr, collaboration.ErrNotOrgAdmin) {
			render.Error(w, adminErr, http.StatusNotFound)
			return
		}
		if callerID != userID {
			render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
			return
		}
	}

	if err := h.orgSvc.RemoveMember(r.Context(), id, userID); err != nil {
		if strings.Contains(err.Error(), "last admin") {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleOrgInviteList returns all pending and resolved invites for an
// organisation. Only org admins may view the invite list.
func (h *OrgHandler) handleOrgInviteList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)
	if _, err := h.orgSvc.GetAndCheckAdmin(r.Context(), id, userID); err != nil {
		if errors.Is(err, collaboration.ErrNotOrgAdmin) {
			render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
			return
		}
		render.Error(w, err, http.StatusNotFound)
		return
	}
	invites, err := h.orgSvc.ListInvites(r.Context(), id)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, invites)
}

// handleOrgInviteCreate creates a pending invite for an email to join the
// organisation. Only org admins may create invites. The raw invite token is
// returned exactly once in this response — only its hash is persisted, so it
// cannot be recovered later. The caller is responsible for delivering it to
// the invitee (e.g. via an emailed accept link).
func (h *OrgHandler) handleOrgInviteCreate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)
	if _, err := h.orgSvc.GetAndCheckAdmin(r.Context(), id, userID); err != nil {
		if errors.Is(err, collaboration.ErrNotOrgAdmin) {
			render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
			return
		}
		render.Error(w, err, http.StatusNotFound)
		return
	}

	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := validateEmail(req.Email); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	role := auth.Role(req.Role)
	if role == "" {
		role = auth.RoleMember
	}
	if !role.IsValid() {
		render.Error(w, fmt.Errorf("invalid role %q", req.Role), http.StatusBadRequest)
		return
	}

	invite, token, err := h.orgSvc.CreateInvite(r.Context(), id, req.Email, role, userID, collaboration.DefaultInviteTTL)
	if err != nil {
		if errors.Is(err, storageif.ErrOrgInviteExists) {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgInviteCreate, "org", id, map[string]string{"email": req.Email, "role": string(role)})

	render.JSON(w, map[string]any{
		"invite": invite,
		"token":  token,
	})
}

// handleOrgInviteRevoke deletes a pending invite so it can no longer be
// accepted. Only org admins may revoke invites.
func (h *OrgHandler) handleOrgInviteRevoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	inviteID := chi.URLParam(r, "inviteId")
	userID := h.security.CallerID(r)
	if _, err := h.orgSvc.GetAndCheckAdmin(r.Context(), id, userID); err != nil {
		if errors.Is(err, collaboration.ErrNotOrgAdmin) {
			render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
			return
		}
		render.Error(w, err, http.StatusNotFound)
		return
	}

	if err := h.orgSvc.RevokeInvite(r.Context(), id, inviteID); err != nil {
		if errors.Is(err, collaboration.ErrInviteNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgInviteRevoke, "org", id, map[string]string{"inviteId": inviteID})
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleInviteAccept redeems an invite token, adding the calling user to the
// invite's organisation with the role specified in the invite.
func (h *OrgHandler) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	userID := h.security.CallerID(r)

	org, err := h.orgSvc.AcceptInvite(r.Context(), token, userID)
	if err != nil {
		switch {
		case errors.Is(err, collaboration.ErrInviteNotFound):
			render.Error(w, err, http.StatusNotFound)
		case errors.Is(err, collaboration.ErrInviteExpired), errors.Is(err, collaboration.ErrInviteAlreadyAccepted):
			render.Error(w, err, http.StatusGone)
		default:
			render.Error(w, err, http.StatusInternalServerError)
		}
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgInviteAccept, "org", org.ID, nil)
	render.JSON(w, org)
}

func (h *OrgHandler) handleOrgMemberRoleUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}

	callerID := h.security.CallerID(r)
	if _, err := h.orgSvc.GetAndCheckAdmin(r.Context(), id, callerID); err != nil {
		if errors.Is(err, collaboration.ErrNotOrgAdmin) {
			render.Error(w, fmt.Errorf("Forbidden"), http.StatusForbidden)
			return
		}
		render.Error(w, err, http.StatusNotFound)
		return
	}
	if err := h.orgSvc.SetRole(r.Context(), id, userID, auth.Role(req.Role)); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}
