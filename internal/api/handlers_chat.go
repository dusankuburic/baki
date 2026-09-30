package api

import (
	"errors"
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
// @Description  Initiates a streaming chat session with an AI provider. The answer arrives on the event stream as chunk, tool, tool_result, done and error events. The done payload carries tokensIn, tokensOut and chunks, plus — when known — servedModel (the model that answered an alias such as an Azure AI Foundry deployment, router or agent) and inputCostPerM / outputCostPerM (the per-million-token rates the turn was billed at). An error payload carries message and, when classified, code (AI_BUDGET_EXCEEDED, PROVIDER_AUTH, PROVIDER_PERMISSION, PROVIDER_NOT_CONFIGURED, MODEL_NOT_AVAILABLE, …). For provider azure-foundry the model is a model registry id ("m/<entryId>"; "" for the server default; a pre-registry deployment, "claude:" or "agent:" id still resolves for one release): one the caller cannot use — removed, another owner's, no longer shared with them — is refused before any stream starts with 400 and code MODEL_NOT_AVAILABLE.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} string "Stream ID"
// @Failure      400 {object} render.ErrorResponse "Bad Request (code MODEL_NOT_AVAILABLE: the Azure AI Foundry model is not available to the caller)"
// @Failure      429 {object} render.ErrorResponse "Too many concurrent chat streams (code CHAT_CAPACITY_REACHED)"
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
		// Capacity rejections are the caller's fault (too many concurrent
		// streams), not a server failure: render as 429 with a distinct
		// machine code so the frontend can branch on it instead of regexing
		// the message. 4xx also keeps the message visible (5xx bodies are
		// masked to "internal server error").
		if errors.Is(err, service.ErrTooManyChatStreams) {
			render.ErrorWithCode(w, err, http.StatusTooManyRequests, "CHAT_CAPACITY_REACHED")
			return
		}
		if renderModelNotAvailable(w, err) {
			return
		}
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

// renderModelNotAvailable answers a chat request whose Azure AI Foundry model
// the caller cannot use (service.ErrModelNotAvailable — removed, another
// owner's, no longer shared with them) with 400 MODEL_NOT_AVAILABLE, which
// the client answers by putting the text back in the composer, refreshing
// its model list and offering another model. false for any other error.
func renderModelNotAvailable(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, service.ErrModelNotAvailable) {
		return false
	}
	render.ErrorWithCode(w, service.ErrModelNotAvailable, http.StatusBadRequest, service.AzureCodeModelNotAvailable)
	return true
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
		render.JSON(w, beginFinishedBody(res))
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// beginFinishedBody is the begin answer for a stream that already finished:
// its final state, including — when reported — the served model, the
// rates the turn was billed at and the shared-credential failure flag
// (the frontend's BeginStreamResult), which the client missed with the done
// or error event.
func beginFinishedBody(res *service.ResumeResult) map[string]any {
	body := map[string]any{
		"status":    "finished",
		"text":      res.Text,
		"done":      res.Done,
		"error":     res.Error,
		"errorCode": res.ErrorCode,
		"tokensIn":  res.TokensIn,
		"tokensOut": res.TokensOut,
	}
	if res.ServedModel != "" {
		body["servedModel"] = res.ServedModel
	}
	if res.InputCostPerM != 0 {
		body["inputCostPerM"] = res.InputCostPerM
	}
	if res.OutputCostPerM != 0 {
		body["outputCostPerM"] = res.OutputCostPerM
	}
	if res.ErrorShared {
		body["errorShared"] = true
	}
	return body
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

// handleFixDecision delivers the user's approve/decline for an apply_fix
// proposal shown in their chat stream (the AI requested a fix; the user
// clicked Approve/Dismiss in the proposal card). Only the stream's owner may
// decide; the actual flow mutation separately re-checks EDITOR permission at
// apply time (the stream itself only required viewer).
// @Summary      Respond to a fix approval prompt
// @Description  Delivers the user's approve/decline for an apply_fix proposal pending in one of the caller's chat streams. First decision wins; unknown or already-decided proposals return 404.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      404 {object} map[string]string "Not Found"
// @Router       /api/chat/fix-decision [post]
func (h *ChatHandler) handleFixDecision(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StreamID   string `json:"streamId"`
		ProposalID string `json:"proposalId"`
		Approved   bool   `json:"approved"`
		// Per-item opt-outs for batch approvals (U4.1): indices into the
		// card's item list. Ignored for single-fix decisions.
		ExcludedItemIndices []int `json:"excludedItemIndices"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.StreamID == "" || req.ProposalID == "" {
		render.Error(w, fmt.Errorf("streamId and proposalId are required"), http.StatusBadRequest)
		return
	}
	if !h.ownsStream(w, r, req.StreamID) {
		return
	}
	if err := h.chatSvc.ResolveFixDecision(req.StreamID, req.ProposalID, req.Approved, req.ExcludedItemIndices); err != nil {
		render.Error(w, err, http.StatusNotFound)
		return
	}
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

// conversationResponse is the GET-conversation body (F-21): an object, no
// longer the bare message array this route answered before — {messages} plus,
// when the caller has a stream on this conversation whose answer the stored
// history does not carry yet, activeStreamId, so a client that reloaded
// mid-turn re-attaches through /api/chat/resume instead of waiting for
// another reload. Messages is always an array ([] for an empty or missing
// conversation), never null.
type conversationResponse struct {
	Messages []models.ChatMessage `json:"messages"`
	// ActiveStreamID is the caller's stream on this (flow, conversation key)
	// whose question is stored and whose answer is not yet: live, or finished
	// with its answer still being stored (service.ActiveStreamFor). Omitted
	// otherwise — a finished stream whose answer is stored is not reported.
	ActiveStreamID string `json:"activeStreamId,omitempty"`
}

// @Summary      Get conversation history
// @Description  Retrieves the chat history for a specific flow and conversation key (the wire field is historically named `provider`). The body is an object — {messages: [...]} (always an array) plus activeStreamId when the caller has a stream on this conversation whose question is stored but whose answer is not yet (live, or finished and still being stored) — so a client that reloads mid-turn can re-attach via /api/chat/resume instead of losing the answer until another reload. Before F-21 this route answered a bare message array.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} conversationResponse "OK"
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

	conv, sid, err := h.chatSvc.ConversationWithActiveStream(r.Context(), h.common.KeyScope(r), doc, req.Provider)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if conv == nil {
		// An old local file can store "messages": null; the client reads a
		// null list as a failed load.
		conv = []models.ChatMessage{}
	}
	render.JSON(w, conversationResponse{Messages: conv, ActiveStreamID: sid})
}

// @Summary      Save conversation history
// @Description  Saves the chat history for a specific flow and provider. A message's modelLabel (the model's display name when it was sent) is stored clamped: NFC-normalised, control, bidi and zero-width characters dropped, whitespace collapsed, at most 64 characters.
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

	// modelLabel is client text rendered next to every stored answer (and
	// in exports): it gets the rules of a registry display name, clamped
	// instead of refused so one odd label cannot fail the whole save.
	for i := range req.Messages {
		req.Messages[i].ModelLabel = service.ClampModelLabel(req.Messages[i].ModelLabel)
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

// @Summary      Preview AI context
// @Description  Returns a preview of the context that will be sent to the AI provider: the system prompt, the context text, the user message, an estimate of their tokens and the model's context window. The model resolves as the turn's would: for provider azure-foundry a model registry id the caller cannot use is refused with 400 and code MODEL_NOT_AVAILABLE.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} models.ContextPreview "OK"
// @Failure      400 {object} render.ErrorResponse "Bad Request (code MODEL_NOT_AVAILABLE: the Azure AI Foundry model is not available to the caller)"
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
		if renderModelNotAvailable(w, err) {
			return
		}
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
