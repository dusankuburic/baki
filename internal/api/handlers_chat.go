package api

import (
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	"pad-core/models"
)

type ChatHandler struct {
	chatSvc *service.ChatService
	flowSvc *service.FlowService
	common  *SecurityConfig
}

// maxChatMessageBodyBytes bounds request bodies that carry user-authored chat
// content. A single chat turn (plus any inline attachments) has no legitimate
// need for more than this — large flow content is pulled server-side from the
// flow doc, not embedded in the request body. This is tighter than the global
// 10 MiB cap in router.go.
const maxChatMessageBodyBytes = 1 << 20 // 1 MiB

func NewChatHandler(chatSvc *service.ChatService, flowSvc *service.FlowService, common *SecurityConfig) *ChatHandler {
	return &ChatHandler{chatSvc: chatSvc, flowSvc: flowSvc, common: common}
}

// @Summary      Stream chat message
// @Description  Initiates a streaming chat session with an AI provider.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} string "Stream ID"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/stream [post]
func (h *ChatHandler) handleStreamChatMessage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatMessageBodyBytes)
	var req models.ChatRequest
	if !decodeBody(w, r, &req) {
		return
	}

	doc, ok := resolveFlow(w, r, h.flowSvc, h.common, req.FlowID, "viewer")
	if !ok {
		return
	}

	id, err := h.chatSvc.StreamChatMessage(r.Context(), h.common.KeyScope(r), doc, nil, req)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// The paid LLM streaming operation is the one chat action without an audit
	// trail — every other sensitive action (analyze, share, triage, role
	// change) is audited. Record it on accept so cost-amplification / abuse has
	// a non-repudiation record (no-op in local mode where backend is nil).
	logAudit(r.Context(), h.common.Backend, r, h.common.TrustedProxies, AuditActionChatStream, "flow", req.FlowID, map[string]string{"streamId": id})
	render.JSON(w, id)
}

// @Summary      Begin stream
// @Description  Signals the backend to start sending events for the specified stream ID.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Router       /api/chat/begin [post]
func (h *ChatHandler) handleBeginStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !h.ownsStream(w, r, req.ID) {
		return
	}
	// A stream that finished before the client began (fail-fast pre-stream
	// errors) emitted its terminal event before the SSE subscription existed;
	// return that state in the begin response so the client isn't left waiting
	// for events that will never arrive.
	if res := h.chatSvc.BeginStream(r.Context(), req.ID); res != nil {
		render.JSON(w, map[string]any{
			"status":    "finished",
			"text":      res.Text,
			"done":      res.Done,
			"error":     res.Error,
			"tokensIn":  res.TokensIn,
			"tokensOut": res.TokensOut,
		})
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Cancel stream
// @Description  Cancels an active streaming session.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Router       /api/chat/cancel [post]
func (h *ChatHandler) handleCancelStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !h.ownsStream(w, r, req.ID) {
		return
	}
	h.chatSvc.CancelStream(req.ID)
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Resume a chat stream
// @Description  Reconnect to an in-flight AI stream and catch up on missed chunks.
// @Tags         chat
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/chat/resume [post]
func (h *ChatHandler) handleResumeStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		From int    `json:"from,omitempty"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !h.ownsStream(w, r, req.ID) {
		return
	}
	res, err := h.chatSvc.ResumeStream(r.Context(), req.ID, req.From)
	if err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}
	render.JSON(w, res)
}

// ownsStream verifies that the caller owns the given stream ID. In local mode
// (no JWT) there is only one user so the check is always allowed. In JWT mode
// the stream's creator scope must match the caller's identity; an unknown stream
// ID is treated as not-owned to avoid leaking information about other users.
func (h *ChatHandler) ownsStream(w http.ResponseWriter, r *http.Request, streamID string) bool {
	if !h.common.JWTEnabled {
		return true
	}
	owner := h.chatSvc.OwnerOf(r.Context(), streamID)
	caller := h.common.CallerID(r)
	if owner == "" || owner != caller {
		// Return the standard JSON error envelope (every other authz failure in
		// the API does) so frontend handlers parsing {code,message,requestId}
		// don't choke on a plain-text body.
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return false
	}
	return true
}

// @Summary      Get conversation history
// @Description  Retrieves the chat history for a specific flow and provider.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/get [post]
func (h *ChatHandler) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	doc, ok := resolveFlow(w, r, h.flowSvc, h.common, req.FlowID, "viewer")
	if !ok {
		return
	}

	conv, err := h.chatSvc.GetConversation(r.Context(), doc, req.Provider)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, conv)
}

// @Summary      Save conversation history
// @Description  Saves the chat history for a specific flow and provider.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/save [post]
func (h *ChatHandler) handleSaveConversation(w http.ResponseWriter, r *http.Request) {
	if !h.common.RequireRole(w, r, auth.RoleMember) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatMessageBodyBytes)
	var req struct {
		FlowID   string               `json:"flowId"`
		Provider string               `json:"provider"`
		Messages []models.ChatMessage `json:"messages"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	doc, ok := resolveFlow(w, r, h.flowSvc, h.common, req.FlowID, "editor")
	if !ok {
		return
	}

	if err := h.chatSvc.SaveConversation(r.Context(), doc, req.Provider, req.Messages); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Clear conversation history
// @Description  Deletes the chat history for a specific flow and provider.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/clear [post]
func (h *ChatHandler) handleClearConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	doc, ok := resolveFlow(w, r, h.flowSvc, h.common, req.FlowID, "editor")
	if !ok {
		return
	}

	if err := h.chatSvc.ClearConversation(r.Context(), doc, req.Provider); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Export conversation
// @Description  Exports the chat history to a file. Only available in local mode.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/export [post]
func (h *ChatHandler) handleExportConversation(w http.ResponseWriter, r *http.Request) {
	if h.common.JWTEnabled {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return
	}
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
		Path     string `json:"path"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	doc, ok := resolveFlow(w, r, h.flowSvc, h.common, req.FlowID, "viewer")
	if !ok {
		return
	}

	if err := h.chatSvc.ExportConversation(r.Context(), doc, req.Provider, req.Path); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Get remaining demo requests
// @Description  Returns the number of demo AI requests remaining for the current session.
// @Tags         chat
// @Produce      json
// @Success      200 {object} int "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/demo-remaining [get]
func (h *ChatHandler) handleGetDemoRemaining(w http.ResponseWriter, r *http.Request) {
	remaining, err := h.chatSvc.GetDemoRemaining()
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, remaining)
}

// @Summary      Preview AI context
// @Description  Returns a preview of the context that will be sent to the AI provider.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/preview-context [post]
func (h *ChatHandler) handlePreviewContext(w http.ResponseWriter, r *http.Request) {
	var req models.ChatRequest
	if !decodeBody(w, r, &req) {
		return
	}

	doc, ok := resolveFlow(w, r, h.flowSvc, h.common, req.FlowID, "viewer")
	if !ok {
		return
	}

	res, err := h.chatSvc.PreviewContext(r.Context(), h.common.KeyScope(r), doc, nil, req)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

// @Summary      Get suggested prompts
// @Description  Returns suggested prompts based on the current flow state.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} []string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/suggested-prompts [post]
func (h *ChatHandler) handleGetSuggestedPrompts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HasBlock    bool `json:"hasBlock"`
		HasFindings bool `json:"hasFindings"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	prompts, err := h.chatSvc.GetSuggestedPrompts(req.HasBlock, req.HasFindings)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, prompts)
}
