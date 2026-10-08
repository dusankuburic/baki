package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
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
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, service.FlowRead); err != nil {
		render.Error(w, err, 0)
		return
	}
	comments, err := h.comments.List(r.Context(), req.FlowID, req.FindingKey)
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
	flow, err := h.flowSvc.GetAuthorizedHeader(r.Context(), req.FlowID, service.FlowEdit)
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
	// The fan-out (bell alerts for the assignee and prior commenters, the assignee's email) follows the
	// FindingCommented event the write publishes: OnFindingCommented, after the commit.
	if err := h.comments.Add(r.Context(), service.FlowRefOf(flow), comment); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionCommentAdd, ResourceType: "flow", ResourceID: req.FlowID, OrgID: flow.OrganizationID, Meta: map[string]string{"findingKey": req.FindingKey, "commentId": comment.ID}})
	render.JSON(w, comment)
}

// claimsEmail is the email in ctx's auth claims when they are userID's ("" otherwise, and in local mode): the
// display-name fallback the detached notifications use without a user lookup. An event delivery's ctx carries
// the claims of the request that published it.
func claimsEmail(ctx context.Context, userID string) string {
	if c := auth.ClaimsFromContext(ctx); c != nil && c.UserID == userID {
		return c.Email
	}
	return ""
}

// OnFindingCommented is the async subscriber of events.FindingCommented (subscribed in internal/di): the
// comment's fan-out, after the commit and never for a rolled-back comment.
func (h *AnalysisHandler) OnFindingCommented(ctx context.Context, e events.FindingCommented) {
	actorEmail := claimsEmail(ctx, e.AuthorID)
	h.goFanOut(func() {
		h.fanOutComment(e.FlowID, e.FindingKey, e.FlowName, e.AuthorID, actorEmail, e.Body, e.CommentID)
	})
}

// goFanOut runs a notification fan-out on h.alerts (capped, drained on shutdown), so the event runner, shared by
// every subscriber, is held only for the hand-off. When h.alerts is saturated or draining the fan-out runs here:
// the bell rows must not be lost.
func (h *AnalysisHandler) goFanOut(fanOut func()) {
	if !h.alerts.Go(fanOut) {
		fanOut()
	}
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
	actorName := h.comments.AuthorName(ctx, actorID, actorEmail)
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
	targets := h.comments.AlertTargets(ctx, flowID, findingKey, commentID)
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

	// nil: no assignee, the commenter IS the assignee, or no address to write to.
	assignee := h.comments.AssigneeToNotify(ctx, flowID, findingKey, commenterID)
	if assignee == nil {
		return
	}
	assigneeName := assignee.DisplayName
	if assigneeName == "" {
		assigneeName = assignee.Email
	}
	// The flow name arrives resolved through the ACTOR's access; the email only names the flow when
	// the assignee can read it themselves (an editor may assign a non-collaborator). Decided for the
	// assignee as a signed-in user: their stored role, no token scopes.
	if h.flowSvc != nil && h.flowSvc.CheckFlowPermission(auth.WithActor(ctx, assigneeActor(assignee)), flowID, service.FlowRead) != nil {
		flowName = ""
	}

	// Use the rule name (extracted from the findingKey prefix) as the title.
	findingTitle := h.ruleDisplayName(ruleIDFromFindingKey(findingKey))

	if err := h.email.SendFindingComment(ctx, assignee.Email, assigneeName, commenterName, flowName, findingTitle, body); err != nil {
		logger.Warn("failed to send finding-comment notification", "assignee", assignee.ID, "error", err)
	}
}

// assigneeActor is who a notification's flow check decides for: the assignee as a signed-in user with their stored
// role (a plain member when it is unset), never the caller and never a token.
func assigneeActor(u *storageif.User) *auth.Actor {
	role := u.Role
	if role == "" {
		role = auth.RoleMember
	}
	return &auth.Actor{Kind: auth.ActorUser, UserID: u.ID, Email: u.Email, Role: role}
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
	var req struct {
		FlowID    string `json:"flowId"`
		CommentID string `json:"commentId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	userID := h.security.CallerID(r)
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, service.FlowEdit); err != nil {
		render.Error(w, err, 0)
		return
	}
	// Editors may delete only their own comments; flow admins (owner, org admin, admin-tier
	// collaborator: FlowShare) may moderate any. Author-scoped delete first — the common case; the
	// second flow-authz resolve is paid only when moderating someone else's comment, and it can only
	// grant: its refusal leaves the author-scoped error standing.
	err := h.comments.Delete(r.Context(), req.FlowID, req.CommentID, userID)
	if errors.Is(err, storageif.ErrNotCommentAuthor) {
		if adminErr := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, service.FlowShare); adminErr == nil {
			err = h.comments.DeleteAsModerator(r.Context(), req.FlowID, req.CommentID)
		}
	}
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionCommentDelete, ResourceType: "flow", ResourceID: req.FlowID, Meta: map[string]string{"commentId": req.CommentID}})
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
	var req struct {
		FlowID string `json:"flowId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	userID := h.security.CallerID(r)
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, service.FlowEdit); err != nil {
		render.Error(w, err, 0)
		return
	}

	t, rawToken, err := h.shares.Create(r.Context(), req.FlowID, userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	metrics.RecordFlowOp("share_create")
	// A public, unauthenticated link to the report: the one action a reviewer
	// of the audit trail most needs to see.
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionShareLinkCreate, ResourceType: "flow", ResourceID: req.FlowID, Meta: map[string]string{"tokenId": t.ID}})
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
	// Require editor (not viewer): share-token metadata (IDs, creator, expiry) is need-to-know for
	// owners/editors — create/revoke already require editor, so list must too, else a viewer could
	// enumerate active share links on a flow.
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, service.FlowEdit); err != nil {
		render.Error(w, err, 0)
		return
	}
	tokens, err := h.shares.List(r.Context(), req.FlowID)
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
	var req struct {
		FlowID  string `json:"flowId"`
		TokenID string `json:"tokenId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := h.flowSvc.CheckFlowPermission(r.Context(), req.FlowID, service.FlowEdit); err != nil {
		render.Error(w, err, 0)
		return
	}
	if err := h.shares.Revoke(r.Context(), req.FlowID, req.TokenID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionShareLinkRevoke, ResourceType: "flow", ResourceID: req.FlowID, Meta: map[string]string{"tokenId": req.TokenID}})
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
	token := r.URL.Query().Get("token")
	if token == "" {
		render.Error(w, fmt.Errorf("token is required"), http.StatusBadRequest)
		return
	}
	st, err := h.shares.Resolve(r.Context(), token)
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
// public report links). Links live in the server-side store; without one the
// list is empty and the writes answer 503.
func (h *FlowHandler) registerShareRoutes(rb *route.Builder) {
	rb.Post("/share/create", h.handleCreateShare, route.Needs(capability.ServerStore), route.Idempotent())
	rb.Post("/share/list", h.handleListShares)
	rb.Post("/share/revoke", h.handleRevokeShare, route.Needs(capability.ServerStore))
}

// registerCommentRoutes wires the finding-comment sub-routes (team-shared
// review threads). Comments live in the server-side store; without one the
// list is empty and the writes answer 503.
func (h *AnalysisHandler) registerCommentRoutes(rb *route.Builder) {
	rb.Post("/comments/list", h.handleListComments)
	rb.Post("/comments/add", h.handleAddComment, route.Needs(capability.ServerStore), route.Idempotent())
	rb.Post("/comments/delete", h.handleDeleteComment, route.Needs(capability.ServerStore))
}
