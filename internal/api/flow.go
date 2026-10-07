package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/models"
)

// maxConcurrentUploadsPerUser bounds one user's in-flight uploads (each a
// large parse plus DB writes) so one account cannot fan out many.
const maxConcurrentUploadsPerUser = 3

// uploadLimiter is a per-user counting semaphore; an empty userID (local /
// unauthenticated) bypasses the limit.
type uploadLimiter struct {
	mu   sync.Mutex
	sems map[string]chan struct{}
}

func newUploadLimiter() *uploadLimiter {
	return &uploadLimiter{sems: make(map[string]chan struct{})}
}

// acquire reserves an upload slot, returning a release callback; at the max
// it waits for the context, then returns ok false (caller rejects with 429).
func (l *uploadLimiter) acquire(ctx context.Context, userID string) (release func(), ok bool) {
	if userID == "" {
		return func() {}, true
	}
	l.mu.Lock()
	sem, exists := l.sems[userID]
	if !exists {
		sem = make(chan struct{}, maxConcurrentUploadsPerUser)
		l.sems[userID] = sem
	}
	l.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() {
			<-sem
			// Drop the entry when the last holder releases so the map does not grow unboundedly;
			// cur == sem guards a release race that recreated a different channel, and len==0 means no sender is blocked.
			l.mu.Lock()
			if cur, ok := l.sems[userID]; ok && cur == sem && len(sem) == 0 {
				delete(l.sems, userID)
			}
			l.mu.Unlock()
		}, true
	case <-ctx.Done():
		return func() {}, false
	}
}

type FlowHandler struct {
	flowSvc       *service.FlowService
	docProvider   service.DocumentProvider
	backend       storageif.StorageBackend
	shares        *service.ShareTokenService
	security      *SecurityConfig
	audit         *audit.Recorder
	uploadLimiter *uploadLimiter
}

func NewFlowHandler(flowSvc *service.FlowService, docProvider service.DocumentProvider, backend storageif.StorageBackend, security *SecurityConfig, shares *service.ShareTokenService, recorder *audit.Recorder) *FlowHandler {
	return &FlowHandler{
		flowSvc:       flowSvc,
		docProvider:   docProvider,
		backend:       backend,
		shares:        shares,
		security:      security,
		audit:         recorder,
		uploadLimiter: newUploadLimiter(),
	}
}

// @Summary      Upload flow file
// @Tags         flow
// @Accept       multipart/form-data
// @Produce      json
// @Success      200 {object} map[string]interface{} "Parsed document"
// @Router       /api/flow/upload [post]
func (h *FlowHandler) handleUploadFlow(w http.ResponseWriter, r *http.Request) {
	if !requireInstancePerm(w, r, h.security, service.InstanceWrite) {
		return
	}
	release, ok := h.uploadLimiter.acquire(r.Context(), h.security.CallerID(r))
	if !ok {
		render.Error(w, fmt.Errorf("too many concurrent uploads; retry shortly"), http.StatusTooManyRequests)
		return
	}
	defer release()
	metrics.RecordFlowOp("upload")
	var req struct {
		Name  string            `json:"name"`
		Files map[string]string `json:"files"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Files) == 0 {
		render.Error(w, fmt.Errorf("no files uploaded"), http.StatusBadRequest)
		return
	}

	doc, err := h.flowSvc.LoadFlowFiles(r.Context(), req.Files, req.Name, h.security.CallerID(r))
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Stash the raw source for single-file flows so cloud-mode apply-fix/preview-fix have the original text.
	if len(req.Files) == 1 {
		for _, text := range req.Files {
			doc.Source = text
		}
	}

	if h.security.JWTEnabled {
		userID := h.security.CallerID(r)
		if err := h.flowSvc.SaveUploadedFlow(r.Context(), doc, userID); err != nil {
			if errors.Is(err, storageif.ErrVersionConflict) {
				render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
				return
			}
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		// flow.upload was once declared but never emitted; emit it so the trail shows who brought a flow in.
		h.audit.Record(r.Context(), audit.Entry{Action: audit.ActionFlowUpload, ResourceType: "flow", ResourceID: doc.ID, Meta: map[string]string{"name": doc.Name}})
	}

	render.JSON(w, doc)
}

// @Summary      Load flow from path
// @Description  Loads a flow document from the specified local file path. Only available in local mode.
// @Tags         flow
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flow/load-path [post]
func (h *FlowHandler) handleLoadFlowFromPath(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("loading from local paths is not supported in cloud mode. use upload instead"), http.StatusForbidden)
		return
	}
	metrics.RecordFlowOp("load_path")
	var req struct {
		Path string `json:"path"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	doc, err := h.flowSvc.LoadFlowFromPath(req.Path)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// No analytics reset here: session analytics key on the file's path; only opening a base FOLDER resets.
	render.JSON(w, doc)
}

// @Summary      Load flow folder
// @Description  Loads a flow document from the specified local folder path. Only available in local mode.
// @Tags         flow
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flow/load-folder [post]
func (h *FlowHandler) handleLoadFlowFolder(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("loading from local folders is not supported in cloud mode. use upload instead"), http.StatusForbidden)
		return
	}
	metrics.RecordFlowOp("load_folder")
	var req struct {
		Path string `json:"path"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	doc, err := h.flowSvc.LoadFlowFolder(req.Path)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Opening a base folder is a deliberate workspace switch — the ONLY action that resets desktop session analytics.
	analyzer.DefaultCache.Clear()
	render.JSON(w, doc)
}

// @Summary      Get recent files
// @Description  Returns a list of recently opened flow documents.
// @Tags         flow
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flow/recent [get]
func (h *FlowHandler) handleRecentFiles(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("recent files are not available in cloud mode"), http.StatusForbidden)
		return
	}
	files, err := h.flowSvc.RecentFiles()
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, files)
}

// @Summary      Remove recent file
// @Description  Removes a file from the list of recently opened flow documents.
// @Tags         flow
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flow/remove-recent [post]
func (h *FlowHandler) handleRemoveRecentFile(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("recent files are not available in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := h.flowSvc.RemoveRecentFile(req.Path); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Clear recent files
// @Description  Clears the entire list of recently opened flow documents.
// @Tags         flow
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flow/clear-recent [post]
func (h *FlowHandler) handleClearRecentFiles(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("recent files are not available in cloud mode"), http.StatusForbidden)
		return
	}
	if err := h.flowSvc.ClearRecentFiles(); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Reveal in file manager
// @Description  Opens the system file manager at the specified path. Only available in local mode.
// @Tags         flow
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flow/reveal [post]
func (h *FlowHandler) handleRevealInFileManager(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("forbidden in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := h.flowSvc.RevealInFileManager(req.Path); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleReimport re-reads the loaded flow's source file via doc.FilePath (no path-injection surface); cloud → 403.
// @Summary      Re-import flow from disk
// @Description  handleReimport re-reads the currently-loaded flow's source file (desktop), re-parses it, and returns the fresh document — so a user who fixed something in Power Automate Desktop can re-import + re-analyze in one click without navigating the file picker. Uses doc.FilePath (the already-loaded path), so there's no path-injection surface. Cloud flows have no on-disk source → 403.
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/flow/reimport [post]
func (h *FlowHandler) handleReimport(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("re-import is not available in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, fmt.Errorf("invalid request body: %w", err), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowRead)
	if !ok {
		return
	}
	if doc.FilePath == "" {
		render.Error(w, fmt.Errorf("no source file path on the current flow"), http.StatusBadRequest)
		return
	}
	fresh, err := h.flowSvc.ReimportFlow(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, fresh)
}

// handleUpdateFlowTags replaces a flow's organizational tags. Editor access.
// Tags are metadata: no version bump, so re-tagging can't trip OCC.
// @Summary      Set flow tags
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Tags"
// @Router       /api/flow/tags [put]
func (h *FlowHandler) handleUpdateFlowTags(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID string   `json:"flowId"`
		Tags   []string `json:"tags"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FlowID == "" {
		render.Error(w, fmt.Errorf("flowId is required"), http.StatusBadRequest)
		return
	}
	// Tags change how the flow is governed/found, so they're an edit; a metadata write needs no content resolution.
	if !requireFlowPerm(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit) {
		return
	}
	if err := h.flowSvc.UpdateFlowTags(r.Context(), req.FlowID, req.Tags); err != nil {
		if errors.Is(err, storageif.ErrNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusBadRequest) // invalid tag names are the common failure
		return
	}
	normalized, _ := storageif.NormalizeFlowTags(req.Tags)
	render.JSON(w, map[string]any{"tags": normalized})
}

// @Summary      Search within flow
// @Description  Performs a search for text or patterns within a specific flow document.
// @Tags         flow
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flow/search [post]
func (h *FlowHandler) handleSearchFlow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string             `json:"id"`
		Query models.SearchQuery `json:"query"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.ID, service.FlowRead)
	if !ok {
		return
	}

	res, err := h.flowSvc.SearchFlow(doc, req.Query)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

// @Summary      Search cloud library
// @Description  handleSearchLibrary runs a query across every flow the caller can access (cross-flow / org-wide search), merging per-flow hits into one result set. Each hit is stamped with its source flowId/flowName so the UI can group them.
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Failure      503 {object} map[string]string "Service Unavailable (cloud mode required)"
// @Router       /api/flow/search-library [post]
func (h *FlowHandler) handleSearchLibrary(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query models.SearchQuery `json:"query"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	userID := h.security.CallerID(r)
	res, err := h.flowSvc.SearchLibrary(r.Context(), userID, req.Query)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

// @Summary      Get source files
// @Description  Returns a list of source files related to the current flow.
// @Tags         flow
// @Produce      json
// @Success      200 {object} []string "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flow/source-files [get]
func (h *FlowHandler) handleGetSourceFiles(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("source files are not available in cloud mode"), http.StatusForbidden)
		return
	}
	doc := h.docProvider.CurrentDoc()
	files, err := h.flowSvc.GetSourceFiles(doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, files)
}

// Mount registers the /api/flow group: upload/load/recent/search here;
// source, block, fix and share sub-groups live with their handlers.
func (h *FlowHandler) Mount(rb *route.Builder) {
	rb.Route("/api/flow", func(rb *route.Builder) {
		// A 10 MB multipart parse and save: its own bucket, outside the request transaction.
		rb.Post("/upload", h.handleUploadFlow, route.RateLimit(route.RateUpload), route.NoTx())
		rb.Post("/load-path", h.handleLoadFlowFromPath)
		rb.Post("/load-folder", h.handleLoadFlowFolder)
		rb.Get("/recent", h.handleRecentFiles)
		rb.Post("/remove-recent", h.handleRemoveRecentFile)
		rb.Post("/clear-recent", h.handleClearRecentFiles)
		rb.Post("/reveal", h.handleRevealInFileManager)
		// Searches index whole documents (blob loads per flow for the library) and write nothing. Without a
		// server-side store there is no library: 503, which the UI tells from an empty "no matches".
		rb.Post("/search", h.handleSearchFlow, route.NoTx())
		rb.Post("/search-library", h.handleSearchLibrary, route.NoTx(), route.Needs(capability.ServerStore))
		rb.Get("/source-files", h.handleGetSourceFiles)
		rb.Post("/reimport", h.handleReimport)
		rb.Put("/tags", h.handleUpdateFlowTags, route.Needs(capability.ServerStore))
		h.registerSourceRoutes(rb)
		h.registerBlockRoutes(rb)
		h.registerFixRoutes(rb)
		h.registerShareRoutes(rb)
	})
}
