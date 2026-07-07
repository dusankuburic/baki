package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// ── Finding Comments ──────────────────────────────────────────────

func (h *AnalysisHandler) handleListComments(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.JSON(w, []interface{}{})
		return
	}
	var req struct {
		FlowID     string `json:"flowId"`
		FindingKey string `json:"findingKey"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if req.FlowID == "" || req.FindingKey == "" {
		render.Error(w, fmt.Errorf("flowId and findingKey are required"), http.StatusBadRequest)
		return
	}
	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "viewer"); err != nil {
		render.Error(w, err, 0)
		return
	}
	comments, err := h.backend.ListFindingComments(r.Context(), req.FlowID, req.FindingKey)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if comments == nil {
		comments = []*storageif.FindingComment{}
	}
	render.JSON(w, comments)
}

func (h *AnalysisHandler) handleAddComment(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("finding comments require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		FlowID     string `json:"flowId"`
		FindingKey string `json:"findingKey"`
		Body       string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if req.Body == "" || req.FlowID == "" || req.FindingKey == "" {
		render.Error(w, fmt.Errorf("flowId, findingKey, and body are required"), http.StatusBadRequest)
		return
	}
	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}
	comment := &storageif.FindingComment{
		FlowID:     req.FlowID,
		FindingKey: req.FindingKey,
		AuthorID:   userID,
		Body:       req.Body,
	}
	if err := h.backend.AddFindingComment(r.Context(), comment); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, comment)
}

func (h *AnalysisHandler) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("finding comments require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		FlowID    string `json:"flowId"`
		CommentID string `json:"commentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}
	// Editors may delete only their own comments; flow admins (owner,
	// org admin, admin-tier collaborator) may moderate any comment. Try the
	// author-scoped delete first — the common case — and only pay for the
	// second flow-authz resolve when moderating someone else's comment.
	err := h.backend.DeleteFindingComment(r.Context(), req.FlowID, req.CommentID, userID)
	if errors.Is(err, storageif.ErrNotCommentAuthor) {
		if _, adminErr := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "admin"); adminErr == nil {
			err = h.backend.DeleteFindingComment(r.Context(), req.FlowID, req.CommentID, "")
		}
	}
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// ── Share Tokens ──────────────────────────────────────────────────

func (h *FlowHandler) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("share links require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}

	// Generate a random token (32 bytes → 64 hex chars)
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	rawToken := hex.EncodeToString(rawBytes)
	hash := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(hash[:])

	t := &storageif.ShareToken{
		FlowID:    req.FlowID,
		TokenHash: tokenHash,
		CreatedBy: userID,
	}
	// Default 30-day expiry so stale links don't grant permanent access.
	expiry := time.Now().AddDate(0, 0, 30)
	t.ExpiresAt = &expiry
	if err := h.backend.CreateShareToken(r.Context(), t); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	metrics.RecordFlowOp("share_create")
	// Return the raw token (shown once, never again) along with the expiry so
	// the UI can tell the user when the link stops working.
	render.JSON(w, map[string]any{
		"id":        t.ID,
		"token":     rawToken,
		"expiresAt": t.ExpiresAt,
	})
}

func (h *FlowHandler) handleListShares(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.JSON(w, []interface{}{})
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "viewer"); err != nil {
		render.Error(w, err, 0)
		return
	}
	tokens, err := h.backend.ListShareTokens(r.Context(), req.FlowID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if tokens == nil {
		tokens = []*storageif.ShareToken{}
	}
	render.JSON(w, tokens)
}

func (h *FlowHandler) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("share links require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	var req struct {
		FlowID  string `json:"flowId"`
		TokenID string `json:"tokenId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	userID := h.security.CallerID(r)
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}
	if err := h.backend.RevokeShareToken(r.Context(), req.FlowID, req.TokenID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleViewShared is an UNAUTHENTICATED endpoint: a holder of the raw token
// can view the flow's current analysis report (read-only). The token hash is
// looked up in the backend; if found and not expired, the flow's analysis is
// returned. No JWT required.
func (h *AnalysisHandler) handleViewShared(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("share links are not available"), http.StatusServiceUnavailable)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		render.Error(w, fmt.Errorf("token is required"), http.StatusBadRequest)
		return
	}
	hash := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(hash[:])

	st, err := h.backend.GetShareTokenByHash(r.Context(), tokenHash)
	if err != nil {
		render.Error(w, fmt.Errorf("invalid or expired link"), http.StatusNotFound)
		return
	}
	// Bypass auth: the token IS the authorization
	doc, err := h.flowSvc.DocProvider().ResolveDoc(r.Context(), st.FlowID)
	if err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}
	report, err := h.analysisSvc.AnalyzeFlow(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]interface{}{
		"flowId":   st.FlowID,
		"report":   report,
		"flowName": doc.Name,
	})
}

// decodeOptional decodes a possibly-empty body into dst. An empty body is a
// no-op (dst keeps its zero values), matching the pattern used by handlers that
// accept optional filter params.
var _ = time.Now // keep import if temporarily unused
