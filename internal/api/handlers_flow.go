package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/models"
)

// maxConcurrentUploadsPerUser bounds the number of in-flight uploads a single
// user may have. Each upload can be up to the global body limit (10 MiB) / 50
// MiB blob cap, so without a per-user guard one account could fan out many
// concurrent large parses and DB writes. The request-timeout middleware bounds
// how long an overflowing request waits before it is rejected.
const maxConcurrentUploadsPerUser = 3

// uploadLimiter is a per-user counting semaphore that throttles concurrent
// uploads. Slots are cheap channels keyed by user ID; an empty userID (local /
// unauthenticated) bypasses the limit.
type uploadLimiter struct {
	mu   sync.Mutex
	sems map[string]chan struct{}
}

func newUploadLimiter() *uploadLimiter {
	return &uploadLimiter{sems: make(map[string]chan struct{})}
}

// acquire reserves an upload slot for userID, returning a release callback. If
// the user already holds maxConcurrentUploadsPerUser uploads it waits until the
// request context is cancelled (request timeout / client disconnect), in which
// case ok is false and the caller should reject with 429.
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
			// Drop the entry once the last holder releases so the map doesn't
			// grow unboundedly with lifetime-distinct-user count (a monotonic
			// memory leak in multi-tenant cloud). A fresh channel is recreated
			// on the user's next upload via the !exists branch above.
			//
			// The identity check (cur == sem) guards a release-vs-release race:
			// if another release already deleted this entry and a later upload
			// recreated a *different* channel under the same userID, we must
			// not delete that newer channel. len==0 also guarantees no sender
			// is currently blocked on this sem (a blocked sender only exists
			// when the buffered channel is full, i.e. len==cap>0).
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
	security      *SecurityConfig
	uploadLimiter *uploadLimiter
	notifier      FlowNotifier
}

func NewFlowHandler(flowSvc *service.FlowService, docProvider service.DocumentProvider, backend storageif.StorageBackend, security *SecurityConfig) *FlowHandler {
	return &FlowHandler{
		flowSvc:       flowSvc,
		docProvider:   docProvider,
		backend:       backend,
		security:      security,
		uploadLimiter: newUploadLimiter(),
	}
}

// SetFlowNotifier wires the WebSocket hub so that apply-fix, save-source, and
// suppress-in-source broadcast a flow-changed event to connected collaborators
// (triggering useFlowChangeSync to reload content on other viewers).
func (h *FlowHandler) SetFlowNotifier(n FlowNotifier) {
	h.notifier = n
}

// @Summary      Upload flow file
// @Tags         flow
// @Accept       multipart/form-data
// @Produce      json
// @Success      200 {object} map[string]interface{} "Parsed document"
// @Router       /api/flow/upload [post]
func (h *FlowHandler) handleUploadFlow(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleMember) {
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

	doc, err := h.flowSvc.LoadFlowFiles(r.Context(), req.Files, req.Name)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Stash the raw source for single-file flows so cloud-mode apply-fix/
	// preview-fix (which patch line-based source) have the original text.
	// Multi-file flows are NOT supported for cloud fix — per-file source
	// storage is a future enhancement. Leave Source empty there; the fix
	// path returns a clear error ("source not available").
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
	// No analytics reset here: session analytics key on the file's path
	// (analyzer.StableFlowID), so re-analyzing this file replaces its own entry
	// and other flows' analytics survive. Only opening a base FOLDER resets the
	// session (see handleLoadFlowFolder).
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
	// Opening a base folder is a deliberate workspace switch — the ONLY action
	// that resets the desktop session analytics. Single-file loads and batch
	// analyses accumulate into the session instead (stable per-path identity
	// prevents double-counting).
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

// handleReimport re-reads the currently-loaded flow's source file (desktop),
// re-parses it, and returns the fresh document — so a user who fixed something
// in Power Automate Desktop can re-import + re-analyze in one click without
// navigating the file picker. Uses doc.FilePath (the already-loaded path), so
// there's no path-injection surface. Cloud flows have no on-disk source → 403.
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
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "viewer")
	if !ok {
		return
	}
	if doc.FilePath == "" {
		render.Error(w, fmt.Errorf("no source file path on the current flow"), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(doc.FilePath)
	if err != nil {
		render.Error(w, fmt.Errorf("source file not accessible: %w", err), http.StatusInternalServerError)
		return
	}
	metrics.RecordFlowOp("reimport")
	var fresh *models.FlowDocument
	if info.IsDir() {
		fresh, err = h.flowSvc.LoadFlowFolder(doc.FilePath)
	} else {
		fresh, err = h.flowSvc.LoadFlowFromPath(doc.FilePath)
	}
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Drop only the re-imported flow's stale analytics entry (the source may
	// have changed on disk); the rest of the session's analytics survive.
	analyzer.DefaultCache.Invalidate(analyzer.StableFlowID(fresh))
	// The flow's content changed on disk (edited in PAD): derived caches —
	// search index, chat context — must not serve the pre-import state.
	h.flowSvc.InvalidateSearchIndex(fresh.ID)
	render.JSON(w, fresh)
}

// handleSuppressInSource writes a `# pad-ignore[ruleId]` directive into the
// flow's source file before the given block, re-parses, and returns the updated
// document. Desktop/local only — cloud flows have no on-disk source. This is
// the first end-to-end apply-fix: the suppression travels with the file (honored
// by the analyzer, CLI gate, baselines, CI), unlike a UI-only suppression.
// @Summary      Insert pad-ignore directive
// @Description  handleSuppressInSource writes a `# pad-ignore[ruleId]` directive into the flow's source file before the given block, re-parses, and returns the updated document. Desktop/local only — cloud flows have no on-disk source. This is the first end-to-end apply-fix: the suppression travels with the file (honored by the analyzer, CLI gate, baselines, CI), unlike a UI-only suppression.
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/flow/suppress-in-source [post]
func (h *FlowHandler) handleSuppressInSource(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("source-file patching is not available in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		FlowID  string `json:"flowId"`
		BlockID string `json:"blockId"`
		RuleID  string `json:"ruleId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" {
		render.Error(w, fmt.Errorf("blockId is required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("patch_suppress")
	updated, err := h.flowSvc.SuppressFindingInSource(doc, req.BlockID, req.RuleID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(doc.ID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, updated)
}

// handleApplyFix applies a deterministic auto-fix (e.g. wrap-in-error-handler)
// to a block in the flow's source file, re-parses, and returns the updated
// document. Desktop/local only. The finding carries the available fixType in
// its AutoFix field; the frontend shows "Apply fix" only when that is set.
// @Summary      Apply an auto-fix
// @Description  handleApplyFix applies a deterministic auto-fix (e.g. wrap-in-error-handler) to a block in the flow's source file, re-parses, and returns the updated document. Desktop/local only. The finding carries the available fixType in its AutoFix field; the frontend shows "Apply fix" only when that is set.
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/flow/apply-fix [post]
func (h *FlowHandler) handleApplyFix(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		BlockID  string `json:"blockId"`
		FixType  string `json:"fixType"`
		RuleID   string `json:"ruleId"`
		Variable string `json:"variable"`
		Property string `json:"property"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" || req.FixType == "" {
		render.Error(w, fmt.Errorf("blockId and fixType are required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("patch_fix")
	updated, err := h.flowSvc.ApplyFix(r.Context(), doc, req.BlockID, req.FixType, req.RuleID, req.Variable, req.Property)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, updated)
}

// handlePreviewFix returns the before/after source text for a fix WITHOUT
// writing. The frontend renders a diff so the user can review the change
// before committing. Works in both desktop (local source file) and cloud
// (stored raw source) modes for single-file flows.
// @Summary      Preview an auto-fix diff
// @Description  handlePreviewFix returns the before/after source text for a fix WITHOUT writing. The frontend renders a diff so the user can review the change before committing. Works in both desktop (local source file) and cloud (stored raw source) modes for single-file flows.
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/flow/preview-fix [post]
func (h *FlowHandler) handlePreviewFix(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		BlockID  string `json:"blockId"`
		FixType  string `json:"fixType"`
		RuleID   string `json:"ruleId"`
		Variable string `json:"variable"`
		Property string `json:"property"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" || req.FixType == "" {
		render.Error(w, fmt.Errorf("blockId and fixType are required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	result, err := h.flowSvc.PreviewFix(doc, req.BlockID, req.FixType, req.RuleID, req.Variable, req.Property)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, result)
}

// handleApplyFixBatch applies multiple auto-fixes in one pass (iterative loop,
// persisted once). The frontend's bulk-action bar derives the rule set from the
// selected findings; the server fixes every auto-fixable finding whose rule is
// in that set (re-parsing between fixes so line shifts don't corrupt targets).
// @Summary      Apply fixes in bulk
// @Description  handleApplyFixBatch applies multiple auto-fixes in one pass (iterative loop, persisted once). The frontend's bulk-action bar derives the rule set from the selected findings; the server fixes every auto-fixable finding whose rule is in that set (re-parsing between fixes so line shifts don't corrupt targets).
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/flow/apply-fix-batch [post]
func (h *FlowHandler) handleApplyFixBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID string   `json:"flowId"`
		Rules  []string `json:"rules"`
		Limit  int      `json:"limit"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FlowID == "" {
		render.Error(w, fmt.Errorf("flowId is required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("patch_fix_batch")
	var ruleFilter map[string]bool
	if len(req.Rules) > 0 {
		ruleFilter = make(map[string]bool, len(req.Rules))
		for _, id := range req.Rules {
			if id = strings.TrimSpace(id); id != "" {
				ruleFilter[id] = true
			}
		}
	}
	updated, applied, err := h.flowSvc.ApplyFixBatch(r.Context(), doc, ruleFilter, req.Limit)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, map[string]any{"document": updated, "applied": applied})
}

// handleListSourceSnapshots returns the flow's undo ring (newest last):
// the pre-mutation states captured by every fix/batch/source-save path this
// session. Editor access (restoring is an edit; listing previews it).
// @Summary      List undo snapshots
// @Tags         flow
// @Produce      json
// @Success      200 {object} map[string]interface{} "Snapshots"
// @Router       /api/flow/snapshots [get]
func (h *FlowHandler) handleListSourceSnapshots(w http.ResponseWriter, r *http.Request) {
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, r.URL.Query().Get("flowId"), "editor")
	if !ok {
		return
	}
	render.JSON(w, map[string]any{"snapshots": h.flowSvc.ListSourceSnapshots(doc)})
}

// handleRestoreSourceSnapshot writes a snapshot's bytes back (undo for the
// last fix/batch/save). Desktop re-writes the recorded file; cloud persists
// through the standard OCC path — and snapshots the current state first, so
// a restore is itself undoable.
// @Summary      Restore undo snapshot
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/snapshots/restore [post]
func (h *FlowHandler) handleRestoreSourceSnapshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID     string `json:"flowId"`
		SnapshotID string `json:"snapshotId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.SnapshotID == "" {
		render.Error(w, fmt.Errorf("snapshotId is required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("snapshot_restore")
	updated, err := h.flowSvc.RestoreSourceSnapshot(r.Context(), doc, req.SnapshotID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, map[string]any{"document": updated})
}

// handleUpdateFlowTags replaces a flow's organizational tags (business unit,
// criticality, environment). Editor access. Tags are metadata, not content:
// no version bump, so re-tagging can't trip OCC for a concurrent editor.
// @Summary      Set flow tags
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Tags"
// @Router       /api/flow/tags [put]
func (h *FlowHandler) handleUpdateFlowTags(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("flow tags require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
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
	// Editor: tags change how the flow is governed/found, so they're an edit.
	// Permission-only check — a metadata write needs no content resolution.
	if !requireFlowPerm(w, r, h.flowSvc, h.security, req.FlowID, "editor") {
		return
	}
	if err := h.backend.UpdateFlowTags(r.Context(), req.FlowID, req.Tags); err != nil {
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

// handleRemoveBlock deletes one block (with descendants) from the flow
// source — the visual editor's delete. Parse-gated + snapshotted server-side
// (undo via the snapshot endpoints). Editor access.
// @Summary      Remove a block
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/remove [post]
func (h *FlowHandler) handleRemoveBlock(w http.ResponseWriter, r *http.Request) {
	h.handleBlockEdit(w, r, "remove")
}

// handleDuplicateBlock inserts a verbatim copy of one block directly after
// it. Editor access.
// @Summary      Duplicate a block
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/duplicate [post]
func (h *FlowHandler) handleDuplicateBlock(w http.ResponseWriter, r *http.Request) {
	h.handleBlockEdit(w, r, "duplicate")
}

func (h *FlowHandler) handleBlockEdit(w http.ResponseWriter, r *http.Request, kind string) {
	var req struct {
		FlowID  string `json:"flowId"`
		BlockID string `json:"blockId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" {
		render.Error(w, fmt.Errorf("blockId is required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_" + kind)
	var updated *models.FlowDocument
	var err error
	if kind == "remove" {
		updated, err = h.flowSvc.RemoveBlock(r.Context(), doc, req.BlockID)
	} else {
		updated, err = h.flowSvc.DuplicateBlock(r.Context(), doc, req.BlockID)
	}
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict) // stale-file/guard errors carry their own guidance
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, map[string]any{"document": updated})
}

// handleUpdateBlockProperties applies a batch of property edits to one block
// (targeted in-line replaces; other properties' text untouched). Editor.
// @Summary      Update block properties
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/properties [post]
func (h *FlowHandler) handleUpdateBlockProperties(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID  string            `json:"flowId"`
		BlockID string            `json:"blockId"`
		Changes map[string]string `json:"changes"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" || len(req.Changes) == 0 {
		render.Error(w, fmt.Errorf("blockId and at least one change are required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_properties")
	updated, err := h.flowSvc.UpdateBlockProperties(r.Context(), doc, req.BlockID, req.Changes)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict) // guards carry their own guidance
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, map[string]any{"document": updated})
}

// handleMoveBlock reorders a block among its siblings ({"up"|"down"}).
// @Summary      Move a block
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/move [post]
func (h *FlowHandler) handleMoveBlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID    string `json:"flowId"`
		BlockID   string `json:"blockId"`
		Direction string `json:"direction"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" {
		render.Error(w, fmt.Errorf("blockId is required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_move")
	updated, err := h.flowSvc.MoveBlock(r.Context(), doc, req.BlockID, req.Direction)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict)
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, map[string]any{"document": updated})
}

// handleRemoveBlocks bulk-deletes blocks in one patch (U3b multi-select).
// @Summary      Bulk delete blocks
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/remove-batch [post]
func (h *FlowHandler) handleRemoveBlocks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string   `json:"flowId"`
		BlockIDs []string `json:"blockIds"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.BlockIDs) == 0 {
		render.Error(w, fmt.Errorf("blockIds must not be empty"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_remove_batch")
	updated, err := h.flowSvc.RemoveBlocks(r.Context(), doc, req.BlockIDs)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict) // guards carry their own guidance
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, map[string]any{"document": updated})
}

// handleRenameBlock renames LABEL/COMMENT blocks (rewriting same-file GOTO
// references for labels). @Summary Rename a block
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document + gotoRefsUpdated"
// @Router       /api/flow/block/rename [post]
func (h *FlowHandler) handleRenameBlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID  string `json:"flowId"`
		BlockID string `json:"blockId"`
		Name    string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" || req.Name == "" {
		render.Error(w, fmt.Errorf("blockId and name are required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_rename")
	updated, gotoRefs, err := h.flowSvc.RenameBlock(r.Context(), doc, req.BlockID, req.Name)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict) // guards carry their own guidance
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, map[string]any{"document": updated, "gotoRefsUpdated": gotoRefs})
}

// handleMoveBlockTo reorders a block before/after a reference sibling —
// the primitive drag-and-drop maps to. Same-scope only (re-parenting
// refused). Editor access.
// @Summary      Move a block relative to another
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Document"
// @Router       /api/flow/block/move-to [post]
func (h *FlowHandler) handleMoveBlockTo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID     string `json:"flowId"`
		BlockID    string `json:"blockId"`
		RefBlockID string `json:"refBlockId"`
		Position   string `json:"position"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" || req.RefBlockID == "" {
		render.Error(w, fmt.Errorf("blockId and refBlockId are required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("block_move_to")
	updated, err := h.flowSvc.MoveBlockTo(r.Context(), doc, req.BlockID, req.RefBlockID, req.Position)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusConflict) // guards carry their own guidance
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, map[string]any{"document": updated})
}

// handleGetSourceMeta returns the cheap file stat signal the desktop
// watcher polls (size + mtime; folder flows aggregate members). Viewer.
// @Summary      Get flow file change signal
// @Tags         flow
// @Produce      json
// @Success      200 {object} map[string]interface{} "Meta"
// @Router       /api/flow/source-meta [get]
func (h *FlowHandler) handleGetSourceMeta(w http.ResponseWriter, r *http.Request) {
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, r.URL.Query().Get("flowId"), "viewer")
	if !ok {
		return
	}
	meta, err := h.flowSvc.GetSourceMeta(doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, meta)
}

// handleGetSource returns the raw PAD source text for the flow. Desktop: reads
// the file. Cloud: returns the stored source. Viewer access.
// @Summary      Get raw flow source
// @Description  handleGetSource returns the raw PAD source text for the flow. Desktop: reads the file. Cloud: returns the stored source. Viewer access.
// @Tags         flow
// @Produce      json
// @Success      200 {object} map[string]interface{} "Source"
// @Router       /api/flow/source [get]
func (h *FlowHandler) handleGetSource(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flowId")
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, flowID, "viewer")
	if !ok {
		return
	}
	source, err := h.flowSvc.GetSource(doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"source": source})
}

// handleSaveSource replaces the flow's raw source text (from the in-app source
// editor), re-parses, and returns the updated document. Editor access.
// @Summary      Save edited flow source
// @Description  handleSaveSource replaces the flow's raw source text (from the in-app source editor), re-parses, and returns the updated document. Editor access.
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/flow/save-source [post]
func (h *FlowHandler) handleSaveSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID string `json:"flowId"`
		Source string `json:"source"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FlowID == "" {
		render.Error(w, fmt.Errorf("flowId is required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "editor")
	if !ok {
		return
	}
	metrics.RecordFlowOp("save_source")
	updated, err := h.flowSvc.SaveSource(r.Context(), doc, req.Source)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if h.notifier != nil {
		h.notifier.NotifyFlowChanged(req.FlowID, int(time.Now().UnixMilli()))
	}
	render.JSON(w, updated)
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

	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.ID, "viewer")
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

// handleSearchLibrary runs a query across every flow the caller can access
// (cross-flow / org-wide search), merging per-flow hits into one result set.
// Each hit is stamped with its source flowId/flowName so the UI can group them.
// @Summary      Search cloud library
// @Description  handleSearchLibrary runs a query across every flow the caller can access (cross-flow / org-wide search), merging per-flow hits into one result set. Each hit is stamped with its source flowId/flowName so the UI can group them.
// @Tags         flow
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
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

// @Summary      Read source files
// @Description  Reads the content of specified source files.
// @Tags         flow
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/flow/read-sources [post]
func (h *FlowHandler) handleReadSourceFiles(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("source file reading is not available in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		Files []string `json:"files"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	doc := h.docProvider.CurrentDoc()
	res, err := h.flowSvc.ReadSourceFiles(doc, req.Files)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

// @Summary      Handle file open from system
// @Description  Notifies the application that a file was opened via the operating system.
// @Tags         flow
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Router       /api/flow/open-from-system [post]
func (h *FlowHandler) handleOnFileOpenFromSystem(w http.ResponseWriter, r *http.Request) {
	if h.security.JWTEnabled {
		render.Error(w, fmt.Errorf("opening from local paths is not supported in cloud mode"), http.StatusForbidden)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	h.flowSvc.OnFileOpenFromSystem(req.Path)
	render.JSON(w, map[string]string{"status": "ok"})
}
