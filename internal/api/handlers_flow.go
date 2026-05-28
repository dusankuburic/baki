package api

import (
	"encoding/json"
	"net/http"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
)

func (rt *Router) handleLoadFlowFromPath(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Error("failed to decode load flow from path request", "error", err)
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	logger.Info("loading flow from path", "path", req.Path)
	doc, err := rt.app.LoadFlowFromPath(req.Path)
	if err != nil {
		logger.Error("failed to load flow from path", "path", req.Path, "error", err)
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, doc)
}

func (rt *Router) handleLoadFlowFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Error("failed to decode load flow folder request", "error", err)
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	logger.Info("loading flow folder", "path", req.Path)
	doc, err := rt.app.LoadFlowFolder(req.Path)
	if err != nil {
		logger.Error("failed to load flow folder", "path", req.Path, "error", err)
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, doc)
}

func (rt *Router) handleRecentFiles(w http.ResponseWriter, r *http.Request) {
	files, err := rt.app.RecentFiles()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, files)
}

func (rt *Router) handleRemoveRecentFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.RemoveRecentFile(req.Path); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleClearRecentFiles(w http.ResponseWriter, r *http.Request) {
	if err := rt.app.ClearRecentFiles(); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleRevealInFileManager(w http.ResponseWriter, r *http.Request) {
	if rt.jwtEnabled {
		http.Error(w, "Forbidden in cloud mode", http.StatusForbidden)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.RevealInFileManager(req.Path); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

func (rt *Router) handleSearchFlow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string             `json:"id"`
		Query models.SearchQuery `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	res, err := rt.app.SearchFlow(req.ID, req.Query)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleGetSourceFiles(w http.ResponseWriter, r *http.Request) {
	files, err := rt.app.GetSourceFiles()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, files)
}

func (rt *Router) handleReadSourceFiles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	res, err := rt.app.ReadSourceFiles(req.Files)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleOnFileOpenFromSystem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	rt.app.OnFileOpenFromSystem(req.Path)
	rt.sendJSON(w, map[string]string{"status": "ok"})
}
