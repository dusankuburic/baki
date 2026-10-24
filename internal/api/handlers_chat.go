package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/drain"
	"pad-analyzer/internal/service"
	"pad-core/models"
)

type ChatHandler struct {
	chatSvc *service.ChatService
	flowSvc *service.FlowService
	common  *SecurityConfig
	audit   *audit.Recorder
	// draining is the server's draining state; nil is the process's (drain.Process), tests set their own.
	draining *drain.State
}

// maxChatMessageBodyBytes bounds bodies carrying user-authored chat content: a turn with inline
// attachments needs no more (large flow content is pulled server-side); tighter than the global 10 MiB cap.
const maxChatMessageBodyBytes = 1 << 20 // 1 MiB

func NewChatHandler(chatSvc *service.ChatService, flowSvc *service.FlowService, common *SecurityConfig, recorder *audit.Recorder) *ChatHandler {
	return &ChatHandler{chatSvc: chatSvc, flowSvc: flowSvc, common: common, audit: recorder}
}

// @Summary      Stream chat message
// @Description  Initiates a streaming chat session with an AI provider. The answer arrives on the event stream as chunk, tool, tool_result, done and error events. The done payload carries tokensIn, tokensOut and chunks, plus — when known — servedModel (the model that answered an alias such as an Azure AI Foundry deployment, router or agent). When the server stored the turn itself (cloud mode, the request carried no history) the answer is in the store before done is sent, and done and error also carry conversationVersion (the stored conversation's version after the server's own writes) and conversationBase (the version the turn began from), only while no other writer changed the thread in between; a client still at conversationBase can adopt conversationVersion without saving or reloading. An error payload carries message and, when classified, code (PROVIDER_AUTH, PROVIDER_PERMISSION, PROVIDER_NOT_CONFIGURED, PROVIDER_UNKNOWN when the turn names a provider no spec describes, PROVIDER_NOT_ALLOWED when the provider policy refused it — disabled in the deployment or off the organisation's allowed-provider list (ADR 0028), MODEL_NOT_AVAILABLE, SERVER_RESTARTING when the server's stop ended the stream, …). A provider id the registry does not know is refused before any stream starts with 400 and code PROVIDER_UNKNOWN. For provider azure-foundry the model is a model registry id ("m/<entryId>"; "" for the server default; a pre-registry deployment, "claude:" or "agent:" id still resolves for one release): one the caller cannot use — removed, another owner's, no longer shared with them — is refused before any stream starts with 400 and code MODEL_NOT_AVAILABLE. While the server is stopping no stream starts: 503 with code SERVER_RESTARTING, a Retry-After header and details.retryAfterSeconds; the request may be sent again once after that wait. A clientStreamId that already names a stream (running, or finished within the resume window, on any replica) is refused with 409 and code STREAM_ID_IN_USE: a resent start whose first send went through gets it, and resumes that stream (POST /api/chat/resume) instead of starting another.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} string "Stream ID"
// @Failure      400 {object} render.ErrorResponse "Bad Request: malformed body, code MODEL_NOT_AVAILABLE (the Azure AI Foundry model is not available to the caller) or PROVIDER_UNKNOWN (no provider spec describes the id)"
// @Failure      409 {object} render.ErrorResponse "The clientStreamId already names a stream (code STREAM_ID_IN_USE): resume it"
// @Failure      429 {object} render.ErrorResponse "Too many concurrent chat streams (code CHAT_CAPACITY_REACHED)"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Failure      503 {object} render.ErrorResponse "The server is restarting (code SERVER_RESTARTING; Retry-After and details.retryAfterSeconds say when to send it again)"
// @Router       /api/chat/stream [post]
func (h *ChatHandler) handleStreamChatMessage(w http.ResponseWriter, r *http.Request) {
	// Refused before the body is read: a stream started now would be cut off by the stop within seconds.
	if h.draining.Draining() {
		drain.Refuse(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatMessageBodyBytes)
	var req models.ChatRequest
	if !decodeBody(w, r, &req) {
		return
	}

	doc, ok := resolveFlow(w, r, h.flowSvc, h.common, req.FlowID, service.ChatUse)
	if !ok {
		return
	}

	id, err := h.chatSvc.StreamChatMessage(r.Context(), h.common.KeyScope(r), doc, nil, req)
	if err != nil {
		// Capacity rejections are the caller's fault, not a server failure: 429 with a machine code
		// the frontend branches on (4xx keeps the message visible; 5xx bodies are masked).
		if errors.Is(err, service.ErrTooManyChatStreams) {
			render.ErrorWithCode(w, err, http.StatusTooManyRequests, "CHAT_CAPACITY_REACHED")
			return
		}
		// The chat service's shutdown began after the check above: the same refusal.
		if errors.Is(err, service.ErrServerRestarting) {
			drain.Refuse(w)
			return
		}
		if renderModelNotAvailable(w, err) {
			return
		}
		// A typed error keeps its own status: a clientStreamId in use is a 409 STREAM_ID_IN_USE.
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// The paid LLM turn is audited on accept so cost-amplification has a
	// non-repudiation record (no-op in local mode).
	h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionChatStream, ResourceType: "flow", ResourceID: req.FlowID, OrgID: doc.OrganizationID, Meta: map[string]string{"streamId": id}})
	render.JSON(w, id)
}

// renderModelNotAvailable answers a chat request whose Azure AI Foundry model the caller cannot use
// (removed, another owner's, no longer shared) with 400 MODEL_NOT_AVAILABLE — the client restores
// the composer text and offers another model — one naming a provider the server does not know with
// 400 PROVIDER_UNKNOWN (ADR 0025), and one the provider policy refused with 400 PROVIDER_NOT_ALLOWED (ADR 0028;
// the preview asks the policy synchronously, a stream reports it on its error event). false for any other error.
func renderModelNotAvailable(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, service.ErrModelNotAvailable):
		render.ErrorWithCode(w, service.ErrModelNotAvailable, http.StatusBadRequest, service.AzureCodeModelNotAvailable)
	case errors.Is(err, service.ErrProviderUnknown):
		render.ErrorWithCode(w, err, http.StatusBadRequest, service.ProviderCodeUnknown)
	case errors.Is(err, ai.ErrProviderNotAllowed):
		render.ErrorWithCode(w, err, http.StatusBadRequest, service.ProviderCodeNotAllowed)
	default:
		return false
	}
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
	// A stream that finished before the client began emitted its terminal event before the SSE
	// subscription existed; return that state so the client isn't left waiting for it.
	if res := h.chatSvc.BeginStream(r.Context(), req.ID); res != nil {
		render.JSON(w, beginFinishedBody(res))
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// beginFinishedBody is the begin answer for a stream that already finished: the final state the
// client missed with the done or error event (the frontend's BeginStreamResult shape).
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
	if res.ErrorShared {
		body["errorShared"] = true
	}
	return body
}

// @Summary      Cancel stream
// @Description  Cancels an active streaming session. A stream that runs on another server of a multi-server deployment is stopped through that server; when it cannot be reached the answer is 503 with code REPLICA_UNREACHABLE (retry), never an ok that stopped nothing. A stream whose server has stopped running (it no longer renews its liveness record, about 15 seconds after it died) ended with it, and is answered like any ended stream (ok).
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} render.ErrorResponse "Not the stream's owner, or a stream mirrored by a server of the release before multi-server stop (code STREAM_NOT_FOUND; the client takes it as gone)"
// @Failure      503 {object} render.ErrorResponse "The stream runs on another server this one could not reach (code REPLICA_UNREACHABLE; Retry-After and details.retryAfterSeconds say when to retry; nothing is known to have been stopped)"
// @Router       /api/chat/cancel [post]
func (h *ChatHandler) handleCancelStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !h.ownsStreamForCommand(w, r, req.ID) {
		return
	}
	if err := h.chatSvc.CancelStreamAs(r.Context(), h.common.KeyScope(r), req.ID); err != nil {
		h.renderCommandError(w, err)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleFixDecision delivers the user's approval decision for a tool request (apply_fix, apply_fixes)
// pending in their chat stream. Only the stream's owner may decide, and a scoped token must hold the
// scope the tool requires; the flow mutation separately re-checks EDITOR permission at apply time
// (the stream itself only required viewer).
// @Summary      Respond to an approval prompt
// @Description  Approval decision: delivers the user's approve/decline (with per-item opt-outs for a batch) for a tool request — a fix_proposal event — pending in one of the caller's chat streams. The stream must be the caller's, and a scoped API token must hold the scope the tool requires (write for the fix tools). First decision wins; unknown or already-decided proposals return 404, and so does a proposal whose server has stopped running (its stream ended with it).
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} render.ErrorResponse "Not the stream's owner (code STREAM_NOT_FOUND), or a token without the scope the tool requires"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      503 {object} render.ErrorResponse "The stream runs on another server this one could not reach (code REPLICA_UNREACHABLE; Retry-After and details.retryAfterSeconds say when to retry; the decision may not have been delivered, so send it again)"
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
	if !h.ownsStreamForCommand(w, r, req.StreamID) {
		return
	}
	if err := h.chatSvc.DecideApproval(r.Context(), h.common.KeyScope(r), req.StreamID, req.ProposalID, req.Approved, req.ExcludedItemIndices); err != nil {
		switch {
		case errors.Is(err, service.ErrPermissionDenied):
			render.Error(w, err, http.StatusForbidden)
		case errors.Is(err, service.ErrReplicaUnreachable), errors.Is(err, service.ErrStreamNotOwned):
			h.renderCommandError(w, err)
		default:
			render.Error(w, err, http.StatusNotFound)
		}
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Resume a chat stream
// @Description  Reconnect to an in-flight AI stream and catch up on missed chunks. A stream the server does not know — it restarted without the resume backplane, the stream finished more than two minutes ago, or it is another user's — answers code STREAM_NOT_FOUND: 403 with accounts (an unknown stream and another user's are not told apart), 404 without. The client reports such a stream as interrupted.
// @Tags         chat
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} render.ErrorResponse "Unknown stream, or another user's (code STREAM_NOT_FOUND; with accounts)"
// @Failure      404 {object} render.ErrorResponse "Unknown stream (code STREAM_NOT_FOUND; without accounts)"
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
		render.ErrorWithCode(w, err, http.StatusNotFound, codeStreamNotFound)
		return
	}
	render.JSON(w, res)
}

// codeStreamNotFound answers a stream id the server does not know (or, with accounts, that is not the caller's):
// after a restart without the resume backplane every stream is such a one, and the client reports it interrupted
// at once instead of waiting out its stall probe. The statuses stay the ones clients already treat as gone.
const codeStreamNotFound = "STREAM_NOT_FOUND"

// ownsStream verifies that the caller owns the given stream ID. In local mode (no JWT) there is only
// one user; in JWT mode the stream's creator scope must match, and an unknown stream ID is treated
// as not-owned to avoid leaking information about other users.
func (h *ChatHandler) ownsStream(w http.ResponseWriter, r *http.Request, streamID string) bool {
	if !h.common.JWTEnabled {
		return true
	}
	owner := h.chatSvc.OwnerOf(r.Context(), streamID)
	caller := h.common.CallerID(r)
	if owner == "" || owner != caller {
		// The standard JSON error envelope, so frontend handlers parsing
		// {code,message,requestId} don't choke on a plain-text body.
		render.ErrorWithCode(w, fmt.Errorf("stream not found"), http.StatusForbidden, codeStreamNotFound)
		return false
	}
	return true
}

// ownsStreamForCommand is ownsStream for the commands that act on a running stream (cancel, an approval decision).
// The difference: a stream this replica does not know is looked up in the shared snapshot, and when that cannot be
// read (Redis is down) the answer is 503 REPLICA_UNREACHABLE (retry), not a refusal as an unknown stream.
func (h *ChatHandler) ownsStreamForCommand(w http.ResponseWriter, r *http.Request, streamID string) bool {
	if !h.common.JWTEnabled {
		return true
	}
	owner, err := h.chatSvc.OwnerOfChecked(r.Context(), streamID)
	if err != nil {
		h.renderCommandError(w, err)
		return false
	}
	if owner == "" || owner != h.common.CallerID(r) {
		render.ErrorWithCode(w, fmt.Errorf("stream not found"), http.StatusForbidden, codeStreamNotFound)
		return false
	}
	return true
}

// renderCommandError answers a failed command: the replica that runs the stream could not be reached (503 with
// Retry-After, unlogged here: the service counted and logged it), or the stream is not the caller's.
func (h *ChatHandler) renderCommandError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrReplicaUnreachable):
		w.Header().Set("Retry-After", strconv.Itoa(service.ReplicaRetryAfterSeconds))
		render.ErrorUnlogged(w, err, 0)
	case errors.Is(err, service.ErrStreamNotOwned):
		render.ErrorWithCode(w, err, http.StatusForbidden, codeStreamNotFound)
	default:
		render.Error(w, err, http.StatusInternalServerError)
	}
}

// conversationResponse is the GET-conversation body: {messages} (always an array, never null) plus
// activeStreamId when the caller has a stream whose answer is not stored yet, so a client that
// reloaded mid-turn re-attaches via /api/chat/resume.
type conversationResponse struct {
	Messages []models.ChatMessage `json:"messages"`
	// ActiveStreamID is the caller's stream whose answer is not yet stored (live, or finished and
	// still being stored); omitted once the answer is stored.
	ActiveStreamID string `json:"activeStreamId,omitempty"`
	// Version is the stored conversation's version, to send back as baseVersion on save and clear; 0 for a
	// conversation that does not exist yet, omitted where the store has no versions (local files).
	Version *int64 `json:"version,omitempty"`
}

// saveConversationResponse is the save body: status, and the new version when the save was versioned.
type saveConversationResponse struct {
	Status  string `json:"status"`
	Version *int64 `json:"version,omitempty"`
}

// @Summary      Get conversation history
// @Description  Retrieves the chat history for a specific flow and conversation key (the wire field is historically named `provider`). The body is an object — {messages: [...]} (always an array), version (the conversation's version, 0 when none is stored yet; send it back as baseVersion on save and clear; absent where the store has no versions) plus activeStreamId when the caller has a stream on this conversation whose question is stored but whose answer is not yet (live, or finished and still being stored) — so a client that reloads mid-turn can re-attach via /api/chat/resume instead of losing the answer until another reload. The frontend still accepts the bare array older servers answered with.
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

	flowID, ok := resolveFlowID(w, r, h.flowSvc, h.common, req.FlowID, service.ChatUse)
	if !ok {
		return
	}

	read, err := h.chatSvc.ReadConversation(r.Context(), h.common.KeyScope(r), flowID, req.Provider)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	conv := read.Messages
	if conv == nil {
		// An old local file can store "messages": null; the client reads a
		// null list as a failed load.
		conv = []models.ChatMessage{}
	}
	resp := conversationResponse{Messages: conv, ActiveStreamID: read.ActiveStreamID}
	if read.Versioned {
		resp.Version = &read.Version
	}
	render.JSON(w, resp)
}

// @Summary      Save conversation history
// @Description  Saves the chat history for a specific flow and provider. A message's content is stored as its scrubbed copy (secrets such as passwords, tokens and keys masked `[REDACTED]`, as the model saw them), so a reload returns the masked text. A message's modelLabel (the model's display name when it was sent) is stored clamped: NFC-normalised, control, bidi and zero-width characters dropped, whitespace collapsed, at most 64 characters. With baseVersion (the version GET returned, or a previous save answered) the list replaces the stored one only if the conversation still has that version, and the response carries the new version; a conversation that changed since answers 409 CONVERSATION_CONFLICT with details.version (the current version): reload, merge by message id and save again. Without baseVersion (a client from before versions) the save replaces the stored list whatever it holds, which can erase a turn another tab or the server stored; it is counted in pad_chat_blind_saves_total.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} saveConversationResponse "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      409 {object} render.ErrorResponse "CONVERSATION_CONFLICT: the conversation is no longer at baseVersion (details.version is the current one)"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/save [post]
func (h *ChatHandler) handleSaveConversation(w http.ResponseWriter, r *http.Request) {
	// Kept on the role check, not Can's InstanceWrite: that would demand the write scope of a chat token.
	if !h.common.RequireRole(w, r, auth.RoleMember) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxChatMessageBodyBytes)
	var req struct {
		FlowID   string               `json:"flowId"`
		Provider string               `json:"provider"`
		Messages []models.ChatMessage `json:"messages"`
		// BaseVersion is the conversation version the client's list was built on; absent from clients that predate versions.
		BaseVersion *int64 `json:"baseVersion"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BaseVersion != nil && *req.BaseVersion < 0 {
		render.ErrorWithCode(w, fmt.Errorf("baseVersion must not be negative"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}

	// Kept on the editor role check, not Can's FlowEdit: that would demand the write scope of a chat token.
	flowID, ok := resolveFlowIDByRole(w, r, h.flowSvc, h.common, req.FlowID, "editor")
	if !ok {
		return
	}

	// modelLabel is client text rendered next to stored answers and in exports: clamped like a
	// registry display name, not refused, so one odd label cannot fail the whole save.
	for i := range req.Messages {
		req.Messages[i].ModelLabel = service.ClampModelLabel(req.Messages[i].ModelLabel)
	}
	saved, err := h.chatSvc.SaveClientConversation(r.Context(), flowID, req.Provider, req.BaseVersion, req.Messages)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	resp := saveConversationResponse{Status: "ok"}
	if saved.Versioned {
		resp.Version = &saved.Version
	}
	render.JSON(w, resp)
}

// @Summary      Clear conversation history
// @Description  Deletes the chat history for a specific flow and provider. With baseVersion the history is deleted only if the conversation still has that version, else 409 CONVERSATION_CONFLICT with details.version; without it the delete is unconditional, as before.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      409 {object} render.ErrorResponse "CONVERSATION_CONFLICT: the conversation is no longer at baseVersion (details.version is the current one)"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/clear [post]
func (h *ChatHandler) handleClearConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
		// BaseVersion is the conversation version the client cleared from; absent from clients that predate versions.
		BaseVersion *int64 `json:"baseVersion"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BaseVersion != nil && *req.BaseVersion < 0 {
		render.ErrorWithCode(w, fmt.Errorf("baseVersion must not be negative"), http.StatusBadRequest, "BAD_REQUEST")
		return
	}

	// Kept on the editor role check, not Can's FlowEdit: that would demand the write scope of a chat token.
	flowID, ok := resolveFlowIDByRole(w, r, h.flowSvc, h.common, req.FlowID, "editor")
	if !ok {
		return
	}

	var err error
	if req.BaseVersion != nil {
		err = h.chatSvc.ClearConversationIf(r.Context(), flowID, req.Provider, *req.BaseVersion)
	} else {
		err = h.chatSvc.ClearConversation(r.Context(), flowID, req.Provider)
	}
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Preview AI context
// @Description  Returns a preview of the request the turn would send, built by the turn's own assembly: the system prompt (the marker-mode tool instructions included), the flow context (the cached analysis report included, as the turn includes it), the user turn (the organisation's guidelines included), the prompt's token estimate as the window check counts it (system prompt, tool schemas, every message of the stored or forwarded history, the turn) with its breakdown by part, how many history messages the window would drop, the tools offered, the effective context budget and the model's context window. The model and the provider resolve as the turn's would: for provider azure-foundry a model registry id the caller cannot use is refused with 400 and code MODEL_NOT_AVAILABLE; a provider no spec describes with 400 PROVIDER_UNKNOWN; one the provider policy refuses (disabled in the deployment, or off the flow's organisation's allowed-provider list) with 400 PROVIDER_NOT_ALLOWED.
// @Tags         chat
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} models.ContextPreview "OK"
// @Failure      400 {object} render.ErrorResponse "Bad Request: malformed body, code MODEL_NOT_AVAILABLE (the Azure AI Foundry model is not available to the caller), PROVIDER_UNKNOWN (no provider spec describes the id) or PROVIDER_NOT_ALLOWED (the provider policy refused it)"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/chat/preview-context [post]
func (h *ChatHandler) handlePreviewContext(w http.ResponseWriter, r *http.Request) {
	var req models.ChatRequest
	if !decodeBody(w, r, &req) {
		return
	}

	doc, ok := resolveFlow(w, r, h.flowSvc, h.common, req.FlowID, service.ChatUse)
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
	prompts, err := h.chatSvc.GetSuggestedPrompts(r.Context(), req.HasBlock, req.HasFindings)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, prompts)
}

// Mount registers the /api/chat group: the chat scope for scoped tokens, and
// no request transaction (a stream would pin a pooled connection for its
// lifetime).
func (h *ChatHandler) Mount(rb *route.Builder) {
	rb.Route("/api/chat", func(rb *route.Builder) {
		rb.Post("/stream", h.handleStreamChatMessage, route.RateLimit(route.RateChat), route.Streaming())
		rb.Post("/begin", h.handleBeginStream)
		rb.Post("/cancel", h.handleCancelStream)
		// Approving a fix proposal edits the flow: write on top of chat.
		rb.Post("/fix-decision", h.handleFixDecision, route.Scopes(auth.ScopeChat, auth.ScopeWrite))
		rb.Post("/resume", h.handleResumeStream)
		rb.Post("/get", h.handleGetConversation)
		rb.Post("/save", h.handleSaveConversation)
		rb.Post("/clear", h.handleClearConversation)
		rb.Post("/preview-context", h.handlePreviewContext)
		rb.Post("/suggested-prompts", h.handleGetSuggestedPrompts)
	}, route.Scopes(auth.ScopeChat), route.NoTx())
}
