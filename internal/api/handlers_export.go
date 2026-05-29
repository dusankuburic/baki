package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
)

// @Summary Compare current flow with another
// @Description Returns a diff between the currently loaded flow and a flow at the specified path. Only available in local mode.
// @Tags export
// @Accept json
// @Produce json
// @Param request body object{path=string} true "Compare Flow Request"
// @Success 200 {object} models.FlowDiff
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/export/compare [post]
func (rt *Router) handleCompareCurrentWith(w http.ResponseWriter, r *http.Request) {
	if rt.jwtEnabled {
		http.Error(w, "comparison from local paths is not supported in cloud mode", http.StatusForbidden)
		return
	}
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

// @Summary Export flow as Markdown
// @Description Exports the specified flow to a Markdown file. Returns base64 encoded data. Only available in local mode.
// @Tags export
// @Accept json
// @Produce json
// @Param request body object{path=string} true "Export Markdown Request"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/export/markdown [post]
func (rt *Router) handleExportMarkdown(w http.ResponseWriter, r *http.Request) {
	if rt.jwtEnabled {
		http.Error(w, "export from local paths is not supported in cloud mode", http.StatusForbidden)
		return
	}
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

// @Summary Export flow as PDF
// @Description Exports the specified flow to a PDF file. Returns base64 encoded data. Only available in local mode.
// @Tags export
// @Accept json
// @Produce json
// @Param request body object{path=string} true "Export PDF Request"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/export/pdf [post]
func (rt *Router) handleExportPDF(w http.ResponseWriter, r *http.Request) {
	if rt.jwtEnabled {
		http.Error(w, "export from local paths is not supported in cloud mode", http.StatusForbidden)
		return
	}
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
