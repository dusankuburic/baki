package api

import (
	"encoding/json"
	"net/http"
	"pad-analyzer/internal/models"
)

func (rt *Router) handleStreamChatMessage(w http.ResponseWriter, r *http.Request) {
	var req models.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	id, err := rt.app.StreamChatMessage(req)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, id)
}

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

func (rt *Router) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	conv, err := rt.app.GetConversation(req.FlowID, req.Provider)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, conv)
}

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
	if err := rt.app.SaveConversation(req.FlowID, req.Provider, req.Messages); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleClearConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.ClearConversation(req.FlowID, req.Provider); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleExportConversation(w http.ResponseWriter, r *http.Request) {
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

func (rt *Router) handleGetDemoRemaining(w http.ResponseWriter, r *http.Request) {
	remaining, err := rt.app.GetDemoRemaining()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, remaining)
}

func (rt *Router) handlePreviewContext(w http.ResponseWriter, r *http.Request) {
	var req models.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	res, err := rt.app.PreviewContext(req)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

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
