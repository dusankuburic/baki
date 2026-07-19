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
	render.JSON(w, fresh)
}

// handleSuppressInSource writes a `# pad-ignore[ruleId]` directive into the
// flow's source file before the given block, re-parses, and returns the updated
// document. Desktop/local only — cloud flows have no on-disk source. This is
// the first end-to-end apply-fix: the suppression travels with the file (honored
// by the analyzer, CLI gate, baselines, CI), unlike a UI-only suppression.
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
	render.JSON(w, updated)
}

// handleApplyFix applies a deterministic auto-fix (e.g. wrap-in-error-handler)
// to a block in the flow's source file, re-parses, and returns the updated
// document. Desktop/local only. The finding carries the available fixType in
// its AutoFix field; the frontend shows "Apply fix" only when that is set.
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
	render.JSON(w, updated)
}

// handlePreviewFix returns the before/after source text for a fix WITHOUT
// writing. The frontend renders a diff so the user can review the change
// before committing. Works in both desktop (local source file) and cloud
// (stored raw source) modes for single-file flows.
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
	render.JSON(w, map[string]any{"document": updated, "applied": applied})
}

// handleGetSource returns the raw PAD source text for the flow. Desktop: reads
// the file. Cloud: returns the stored source. Viewer access.
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
	render.JSON(w, updated)
}

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
