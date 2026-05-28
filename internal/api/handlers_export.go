package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
)

func (rt *Router) handleCompareCurrentWith(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	diff, err := rt.app.CompareCurrentWith(req.Path)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, diff)
}

func (rt *Router) handleExportMarkdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	content, err := rt.app.ExportMarkdown(req.Path)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]any{
		"status": "ok",
		"data":   base64.StdEncoding.EncodeToString(content),
	})
}

func (rt *Router) handleExportPDF(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	content, err := rt.app.ExportPDF(req.Path)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]any{
		"status": "ok",
		"data":   base64.StdEncoding.EncodeToString(content),
	})
}
