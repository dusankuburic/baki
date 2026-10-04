package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ── Finding Comments ──────────────────────────────────────────────

// @Summary      List comments for a finding
// @Tags         comments
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/comments/list [post]
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
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "viewer"); err != nil {
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

// @Summary      Add a comment to a finding
// @Tags         comments
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/comments/add [post]
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
	// Header, not the resolved document: the write needs the editor check and
	// the flow's name (for the alerts and the email), never its content.
	flow, err := h.flowSvc.GetAuthorizedHeader(r.Context(), req.FlowID, userID, "editor")
	if err != nil {
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

	// The fan-out (bell alerts for the assignee and prior commenters, the assignee's email) is
	// best-effort and runs AFTER the commit, detached: the post-commit hook only ENQUEUES onto
	// h.alerts (capped, drained on shutdown) — running inline needed a second pooled connection
	// while the request's RLS transaction pinned the first, and the middleware runs hooks before
	// the response is complete.
	actorEmail := claimsEmail(r.Context())
	flowName := flow.Name
	storagedb.RegisterPostCommit(r.Context(), func() {
		fanOut := func() {
			h.fanOutComment(req.FlowID, req.FindingKey, flowName, userID, actorEmail, req.Body, comment.ID)
		}
		if !h.alerts.Go(fanOut) {
			// Pool saturated or draining: the bell rows must not be lost (they
			// never could be when this ran inline), so pay the round trips here.
			fanOut()
		}
	})

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionCommentAdd, "flow", req.FlowID, map[string]string{"findingKey": req.FindingKey, "commentId": comment.ID})
	render.JSON(w, comment)
}

// claimsEmail is the caller's email from the request's auth claims ("" when unauthenticated or in
// local mode): the display-name fallback the detached notifications use without a user lookup.
func claimsEmail(ctx context.Context) string {
	if c := auth.ClaimsFromContext(ctx); c != nil {
		return c.Email
	}
	return ""
}

// fanOutComment is the detached work of one comment: the actor's display name resolved ONCE, then
// the targeted alerts recorded and the assignee emailed. Runs on h.alerts (the email hops to
// h.emails so a stalled relay never holds the alert rows back), bounded by bestEffortTimeout.
func (h *AnalysisHandler) fanOutComment(flowID, findingKey, flowName, actorID, actorEmail, body, commentID string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("finding-comment fan-out panicked", "err", r)
		}
	}()
	if h.backend == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), bestEffortTimeout)
	defer cancel()
	actorName := h.callerDisplayName(ctx, actorID, actorEmail)
	h.recordCommentAlerts(ctx, flowID, findingKey, flowName, actorID, actorName, commentID)
	h.emails.Go(func() {
		ectx, ecancel := context.WithTimeout(context.Background(), bestEffortTimeout)
		defer ecancel()
		h.notifyFindingComment(ectx, flowID, findingKey, flowName, actorID, actorName, body)
	})
}

// recordCommentAlerts delivers targeted comment_added alerts to the finding's assignee and everyone
// who previously commented (deduped, actor excluded). Names resolve best-effort.
func (h *AnalysisHandler) recordCommentAlerts(ctx context.Context, flowID, findingKey, flowName, actorID, actorName, commentID string) {
	targets := map[string]bool{}
	// Assignee: one primary-key row when the backend offers it (storageif.FindingStatusGetter, as
	// priorAssignee uses); the fallback lists the whole flow's statuses for the single row.
	if g, ok := h.backend.(storageif.FindingStatusGetter); ok {
		if st, err := g.GetFindingStatus(ctx, flowID, findingKey); err == nil && st != nil && st.AssigneeID != "" {
			targets[st.AssigneeID] = true
		}
	} else if statuses, err := h.backend.ListFindingStatuses(ctx, flowID); err == nil {
		for _, st := range statuses {
			if st.FindingKey == findingKey && st.AssigneeID != "" {
				targets[st.AssigneeID] = true
			}
		}
	}
	// Prior commenters.
	if prior, err := h.backend.ListFindingComments(ctx, flowID, findingKey); err == nil {
		for _, c := range prior {
			if c.AuthorID != "" && c.ID != commentID {
				targets[c.AuthorID] = true
			}
		}
	}
	if len(targets) == 0 {
		return
	}
	title := h.ruleDisplayName(ruleIDFromFindingKey(findingKey))
	message := fmt.Sprintf("New comment from %s", actorName)
	for target := range targets {
		if target == actorID {
			continue // never notify the actor about their own comment
		}
		h.recordFindingAlert(ctx, &storageif.GovernanceAlert{
			ID:         fmt.Sprintf("%s|comment_added|%s|%s|%s", flowID, findingKey, commentID, target),
			FlowID:     flowID,
			FlowName:   flowName,
			Type:       "comment_added",
			Title:      title,
			Message:    message,
			Severity:   "info",
			TargetUser: target,
		})
	}
}

// ruleIDFromFindingKey extracts the rule prefix ("rule:block" keys); the whole
// key when the separator is absent.
func ruleIDFromFindingKey(key string) string {
	if idx := strings.IndexByte(key, ':'); idx > 0 {
		return key[:idx]
	}
	return key
}

// notifyFindingComment emails the finding's assignee when someone else comments. Best-effort: any
// error is logged and swallowed — the comment write already succeeded. The commenter's and flow's
// names arrive resolved (fanOutComment); re-resolving them is what the fan-out exists to avoid.
func (h *AnalysisHandler) notifyFindingComment(ctx context.Context, flowID, findingKey, flowName, commenterID, commenterName, body string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("finding-comment notification panicked", "err", r)
		}
	}()
	if h.email == nil || h.backend == nil {
		return
	}

	// Find the assignee: one primary-key row when the backend offers it, else the flow's statuses.
	assigneeID := ""
	if g, ok := h.backend.(storageif.FindingStatusGetter); ok {
		if st, err := g.GetFindingStatus(ctx, flowID, findingKey); err == nil && st != nil {
			assigneeID = st.AssigneeID
		}
	} else if statuses, err := h.backend.ListFindingStatuses(ctx, flowID); err == nil {
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
	// The flow name arrives resolved through the ACTOR's access; the email only names the flow when
	// the assignee can read it themselves (an editor may assign a non-collaborator).
	if h.flowSvc != nil && h.flowSvc.CheckFlowPermission(ctx, flowID, assigneeID, "viewer") != nil {
		flowName = ""
	}

	// Use the rule name (extracted from the findingKey prefix) as the title.
	findingTitle := h.ruleDisplayName(ruleIDFromFindingKey(findingKey))

	if err := h.email.SendFindingComment(ctx, assignee.Email, assigneeName, commenterName, flowName, findingTitle, body); err != nil {
		logger.Warn("failed to send finding-comment notification", "assignee", assigneeID, "error", err)
	}
}

// @Summary      Delete a comment
// @Tags         comments
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/analysis/comments/delete [post]
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
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}
	// Editors may delete only their own comments; flow admins (owner, org admin, admin-tier
	// collaborator) may moderate any. Author-scoped delete first — the common case; the second
	// flow-authz resolve is paid only when moderating someone else's comment.
	err := h.backend.DeleteFindingComment(r.Context(), req.FlowID, req.CommentID, userID)
	if errors.Is(err, storageif.ErrNotCommentAuthor) {
		if adminErr := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "admin"); adminErr == nil {
			err = h.backend.DeleteFindingComment(r.Context(), req.FlowID, req.CommentID, "")
		}
	}
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionCommentDelete, "flow", req.FlowID, map[string]string{"commentId": req.CommentID})
	render.JSON(w, map[string]string{"status": "ok"})
}

// ── Share Tokens ──────────────────────────────────────────────────

// @Summary      Create a public share link for a flow
// @Tags         sharing
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/flow/share/create [post]
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
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "editor"); err != nil {
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
	// A public, unauthenticated link to the report: the one action a reviewer
	// of the audit trail most needs to see.
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionShareLinkCreate, "flow", req.FlowID, map[string]string{"tokenId": t.ID})
	// Return the raw token (shown once, never again) along with the expiry so
	// the UI can tell the user when the link stops working.
	render.JSON(w, map[string]any{
		"id":        t.ID,
		"token":     rawToken,
		"expiresAt": t.ExpiresAt,
	})
}

// @Summary      List active share links for a flow
// @Tags         sharing
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/flow/share/list [post]
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
	// Require editor (not viewer): share-token metadata (IDs, creator, expiry) is need-to-know for
	// owners/editors — create/revoke already require editor, so list must too, else a viewer could
	// enumerate active share links on a flow.
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "editor"); err != nil {
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

// @Summary      Revoke a share link
// @Tags         sharing
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      400 {object} object "Bad Request"
// @Failure      401 {object} object "Unauthorized"
// @Failure      503 {object} object "Service Unavailable (cloud mode required)"
// @Router       /api/flow/share/revoke [post]
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
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, userID, "editor"); err != nil {
		render.Error(w, err, 0)
		return
	}
	if err := h.backend.RevokeShareToken(r.Context(), req.FlowID, req.TokenID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionShareLinkRevoke, "flow", req.FlowID, map[string]string{"tokenId": req.TokenID})
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleViewShared is an UNAUTHENTICATED endpoint: a holder of the raw token views the flow's
// current analysis report read-only — the token hash is looked up in the backend and, found and
// unexpired, no JWT is required.
// @Summary      View shared report
// @Description  Unauthenticated read-only report for share-link holders.
// @Tags         share
// @Param        token query string true "Share token"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Report"
// @Failure      404 {object} map[string]string "Not found"
// @Router       /api/shared [get]
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

// registerShareRoutes wires the /api/flow share-link sub-routes (read-only
// public report links).
func (h *FlowHandler) registerShareRoutes(r chi.Router) {
	r.Post("/share/create", h.handleCreateShare)
	r.Post("/share/list", h.handleListShares)
	r.Post("/share/revoke", h.handleRevokeShare)
}

// registerCommentRoutes wires the finding-comment sub-routes (team-shared
// review threads).
func (h *AnalysisHandler) registerCommentRoutes(r chi.Router) {
	r.Post("/comments/list", h.handleListComments)
	r.Post("/comments/add", h.handleAddComment)
	r.Post("/comments/delete", h.handleDeleteComment)
}
