package api

import (
	"encoding/json"
	"net/http"
	"pad-analyzer/internal/models"
)

// @Summary Stream chat message
// @Description Initiates a streaming chat session with an AI provider.
// @Tags chat
// @Accept json
// @Produce json
// @Param request body models.ChatRequest true "Chat Request"
// @Success 200 {string} string "Stream ID"
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/chat/stream [post]
func (rt *Router) handleStreamChatMessage(w http.ResponseWriter, r *http.Request) {
	var req models.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireFlowAccess(w, r, req.FlowID, "viewer") {
		return
	}
	id, err := rt.app.StreamChatMessage(rt.keyScope(r), req)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, id)
}

// @Summary Begin stream
// @Description Signals the backend to start sending events for the specified stream ID.
// @Tags chat
// @Accept json
// @Produce json
// @Param request body object{streamId=string} true "Begin Stream Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Router /api/chat/begin [post]
func (rt *Router) handleBeginStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"streamId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rt.app.BeginStream(req.ID)
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Cancel stream
// @Description Cancels an active streaming session.
// @Tags chat
// @Accept json
// @Produce json
// @Param request body object{streamId=string} true "Cancel Stream Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Router /api/chat/cancel [post]
func (rt *Router) handleCancelStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"streamId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rt.app.CancelStream(req.ID)
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Get conversation history
// @Description Retrieves the chat history for a specific flow and provider.
// @Tags chat
// @Accept json
// @Produce json
// @Param request body object{flowId=string,provider=string} true "Get Conversation Request"
// @Success 200 {array} models.ChatMessage
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/chat/get [post]
func (rt *Router) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireFlowAccess(w, r, req.FlowID, "viewer") {
		return
	}
	conv, err := rt.app.GetConversation(req.FlowID, req.Provider)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, conv)
}

// @Summary Save conversation history
// @Description Saves the chat history for a specific flow and provider.
// @Tags chat
// @Accept json
// @Produce json
// @Param request body object{flowId=string,provider=string,messages=[]models.ChatMessage} true "Save Conversation Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/chat/save [post]
func (rt *Router) handleSaveConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string               `json:"flowId"`
		Provider string               `json:"provider"`
		Messages []models.ChatMessage `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireFlowAccess(w, r, req.FlowID, "editor") {
		return
	}
	if err := rt.app.SaveConversation(req.FlowID, req.Provider, req.Messages); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Clear conversation history
// @Description Deletes the chat history for a specific flow and provider.
// @Tags chat
// @Accept json
// @Produce json
// @Param request body object{flowId=string,provider=string} true "Clear Conversation Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/chat/clear [post]
func (rt *Router) handleClearConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireFlowAccess(w, r, req.FlowID, "editor") {
		return
	}
	if err := rt.app.ClearConversation(req.FlowID, req.Provider); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Export conversation
// @Description Exports the chat history to a file. Only available in local mode.
// @Tags chat
// @Accept json
// @Produce json
// @Param request body object{flowId=string,provider=string,path=string} true "Export Conversation Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/chat/export [post]
func (rt *Router) handleExportConversation(w http.ResponseWriter, r *http.Request) {
	// Exporting writes to a path on the server's local filesystem, which only
	// makes sense for the single-user desktop app. Forbid it in cloud mode.
	if rt.jwtEnabled {
		http.Error(w, "exporting to a local path is not supported in cloud mode", http.StatusForbidden)
		return
	}
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
		Path     string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.ExportConversation(req.FlowID, req.Provider, req.Path); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Get remaining demo requests
// @Description Returns the number of demo AI requests remaining for the current session.
// @Tags chat
// @Produce json
// @Success 200 {integer} integer
// @Failure 500 {object} map[string]string
// @Router /api/chat/demo-remaining [get]
func (rt *Router) handleGetDemoRemaining(w http.ResponseWriter, r *http.Request) {
	remaining, err := rt.app.GetDemoRemaining()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, remaining)
}

// @Summary Preview AI context
// @Description Returns a preview of the context that will be sent to the AI provider.
// @Tags chat
// @Accept json
// @Produce json
// @Param request body models.ChatRequest true "Preview Context Request"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/chat/preview-context [post]
func (rt *Router) handlePreviewContext(w http.ResponseWriter, r *http.Request) {
	var req models.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireFlowAccess(w, r, req.FlowID, "viewer") {
		return
	}
	res, err := rt.app.PreviewContext(rt.keyScope(r), req)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

// @Summary Get suggested prompts
// @Description Returns suggested prompts based on the current flow state.
// @Tags chat
// @Accept json
// @Produce json
// @Param request body object{hasBlock=bool,hasFindings=bool} true "Get Suggested Prompts Request"
// @Success 200 {array} string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/chat/suggested-prompts [post]
func (rt *Router) handleGetSuggestedPrompts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HasBlock    bool `json:"hasBlock"`
		HasFindings bool `json:"hasFindings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	prompts, err := rt.app.GetSuggestedPrompts(req.HasBlock, req.HasFindings)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, prompts)
}
