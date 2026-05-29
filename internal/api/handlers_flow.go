package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// resolveFlow returns the parsed FlowDocument a request should operate on.
// Local/Tauri mode: the in-memory current document (single user). Cloud mode:
// the flow identified by flowID, loaded from storage AFTER an authorization
// check at minPerm ("viewer"|"editor"|"admin"). On failure it writes the
// appropriate response (400 no-flow / 403 / 404 / 500) and returns ok=false.
func (rt *Router) resolveFlow(w http.ResponseWriter, r *http.Request, flowID, minPerm string) (*models.FlowDocument, bool) {
	if !rt.jwtEnabled {
		doc := rt.app.CurrentParsedDoc()
		if doc == nil {
			rt.sendError(w, fmt.Errorf("no flow loaded"), http.StatusBadRequest)
			return nil, false
		}
		return doc, true
	}
	if !rt.requireFlowAccess(w, r, flowID, minPerm) {
		return nil, false
	}
	doc, err := rt.app.LoadParsedFlow(flowID)
	if err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			http.NotFound(w, r)
		} else {
			rt.sendError(w, err, http.StatusInternalServerError)
		}
		return nil, false
	}
	return doc, true
}

func (rt *Router) handleUploadFlow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string            `json:"name"`
		Files map[string]string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		rt.sendError(w, fmt.Errorf("no files uploaded"), http.StatusBadRequest)
		return
	}

	doc, err := rt.app.LoadFlowFiles(req.Files, req.Name)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, doc)
}

// @Summary Load flow from path
// @Description Loads a flow document from the specified local file path. Only available in local mode.
// @Tags flow
// @Accept json
// @Produce json
// @Param request body object{path=string} true "Load Flow Request"
// @Success 200 {object} models.FlowDocument
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flow/load-path [post]
func (rt *Router) handleLoadFlowFromPath(w http.ResponseWriter, r *http.Request) {
	if rt.jwtEnabled {
		rt.sendError(w, fmt.Errorf("loading from local paths is not supported in cloud mode. use upload instead"), http.StatusForbidden)
		return
	}
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

// @Summary Load flow folder
// @Description Loads a flow document from the specified local folder path. Only available in local mode.
// @Tags flow
// @Accept json
// @Produce json
// @Param request body object{path=string} true "Load Flow Folder Request"
// @Success 200 {object} models.FlowDocument
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flow/load-folder [post]
func (rt *Router) handleLoadFlowFolder(w http.ResponseWriter, r *http.Request) {
	if rt.jwtEnabled {
		rt.sendError(w, fmt.Errorf("loading from local folders is not supported in cloud mode. use upload instead"), http.StatusForbidden)
		return
	}
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

// @Summary Get recent files
// @Description Returns a list of recently opened flow documents.
// @Tags flow
// @Produce json
// @Success 200 {array} models.RecentFile
// @Failure 500 {object} map[string]string
// @Router /api/flow/recent [get]
func (rt *Router) handleRecentFiles(w http.ResponseWriter, r *http.Request) {
	files, err := rt.app.RecentFiles()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, files)
}

// @Summary Remove recent file
// @Description Removes a file from the list of recently opened flow documents.
// @Tags flow
// @Accept json
// @Produce json
// @Param request body object{path=string} true "Remove Recent File Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flow/remove-recent [post]
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

// @Summary Clear recent files
// @Description Clears the entire list of recently opened flow documents.
// @Tags flow
// @Produce json
// @Success 200 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flow/clear-recent [post]
func (rt *Router) handleClearRecentFiles(w http.ResponseWriter, r *http.Request) {
	if err := rt.app.ClearRecentFiles(); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Reveal in file manager
// @Description Opens the system file manager at the specified path. Only available in local mode.
// @Tags flow
// @Accept json
// @Produce json
// @Param request body object{path=string} true "Reveal Path Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flow/reveal [post]
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

// @Summary Search within flow
// @Description Performs a search for text or patterns within a specific flow document.
// @Tags flow
// @Accept json
// @Produce json
// @Param request body object{id=string,query=models.SearchQuery} true "Search Flow Request"
// @Success 200 {object} models.SearchResults
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flow/search [post]
func (rt *Router) handleSearchFlow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string             `json:"id"`
		Query models.SearchQuery `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if !rt.requireFlowAccess(w, r, req.ID, "viewer") {
		return
	}
	res, err := rt.app.SearchFlow(req.ID, req.Query)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

// @Summary Get source files
// @Description Returns a list of source files related to the current flow.
// @Tags flow
// @Produce json
// @Success 200 {array} string
// @Failure 500 {object} map[string]string
// @Router /api/flow/source-files [get]
func (rt *Router) handleGetSourceFiles(w http.ResponseWriter, r *http.Request) {
	// Source files are local filesystem paths alongside the PAD file. This feature
	// is only meaningful in desktop/local mode. Guard explicitly so a future refactor
	// that sets currentDoc in cloud mode cannot inadvertently expose server file reads.
	if rt.jwtEnabled {
		rt.sendError(w, fmt.Errorf("source file reading is not available in cloud mode"), http.StatusForbidden)
		return
	}
	files, err := rt.app.GetSourceFiles()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, files)
}

// @Summary Read source files
// @Description Reads the content of specified source files.
// @Tags flow
// @Accept json
// @Produce json
// @Param request body object{files=[]string} true "Read Source Files Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/flow/read-sources [post]
func (rt *Router) handleReadSourceFiles(w http.ResponseWriter, r *http.Request) {
	if rt.jwtEnabled {
		rt.sendError(w, fmt.Errorf("source file reading is not available in cloud mode"), http.StatusForbidden)
		return
	}
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

// @Summary Handle file open from system
// @Description Notifies the application that a file was opened via the operating system.
// @Tags flow
// @Accept json
// @Produce json
// @Param request body object{path=string} true "File Open Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Router /api/flow/open-from-system [post]
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
