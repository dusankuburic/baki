package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/metrics"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/logger"
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
	if !decodeBody(w, r, &req) {
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

	// Best-effort comment notification: if the finding has an assignee who is
	// NOT the commenter, email them so they discover the comment asynchronously.
	// Detached so the response isn't delayed by SMTP.
	go h.notifyFindingComment(req.FlowID, req.FindingKey, userID, req.Body)

	render.JSON(w, comment)
}

// notifyFindingComment emails the finding's assignee when someone else comments.
// Best-effort: any error (no assignee, user not found, SMTP down) is logged and
// swallowed — the comment write already succeeded.
func (h *AnalysisHandler) notifyFindingComment(flowID, findingKey, commenterID, body string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("finding-comment notification panicked", "err", r)
		}
	}()
	if h.email == nil || h.backend == nil {
		return
	}
	// Bound the detached work so a slow DB/SMTP can't keep the process (or a DB
	// connection) alive indefinitely on shutdown. Mirrors the context.WithTimeout
	// pattern used by the other detached background handlers (audit, providers).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Find the assignee for this finding.
	assigneeID := ""
	if statuses, err := h.backend.ListFindingStatuses(ctx, flowID); err == nil {
		for _, st := range statuses {
			if st.FindingKey == findingKey {
				assigneeID = st.AssigneeID
				break
			}
		}
	}
	if assigneeID == "" || assigneeID == commenterID {
		return // no assignee, or the commenter IS the assignee
	}

	assignee, err := h.backend.LoadUserByID(ctx, assigneeID)
	if err != nil || assignee == nil || assignee.Email == "" {
		return
	}
	assigneeName := assignee.DisplayName
	if assigneeName == "" {
		assigneeName = assignee.Email
	}

	commenterName := "A reviewer"
	if u, err := h.backend.LoadUserByID(ctx, commenterID); err == nil && u != nil {
		if u.DisplayName != "" {
			commenterName = u.DisplayName
		} else if u.Email != "" {
			commenterName = u.Email
		}
	}

	// Use the rule name (extracted from the findingKey prefix) as the title.
	findingTitle := findingKey
	ruleID := findingKey
	if idx := indexByte(findingKey, ':'); idx > 0 {
		ruleID = findingKey[:idx]
	}
	for _, rule := range analyzer.AllRules() {
		if rule.ID() == ruleID {
			findingTitle = rule.Name()
			break
		}
	}

	flowName := ""
	if doc, err := h.flowSvc.GetAuthorized(ctx, flowID, assigneeID, "viewer"); err == nil && doc != nil {
		flowName = doc.Name
	}

	if err := h.email.SendFindingComment(ctx, assignee.Email, assigneeName, commenterName, flowName, findingTitle, body); err != nil {
		logger.Warn("failed to send finding-comment notification", "assignee", assigneeID, "error", err)
	}
}

// indexByte returns the index of the first occurrence of b in s, or -1.
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
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
	if !decodeBody(w, r, &req) {
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
	if !decodeBody(w, r, &req) {
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
	// Require editor (not viewer): share-token metadata (IDs, creator, expiry)
	// is need-to-know for owners/editors managing links, not for read-only
	// collaborators. create/revoke already require editor — list must too, so a
	// viewer can't enumerate active share links on a flow.
	if _, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "editor"); err != nil {
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
	if !decodeBody(w, r, &req) {
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
