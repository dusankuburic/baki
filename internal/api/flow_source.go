package api

import (
	"errors"
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// handleSuppressInSource writes a `# pad-ignore[ruleId]` directive into the source before the
// block and re-parses. Desktop/local only; the suppression travels with the file (analyzer, CLI gate, baselines, CI).
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
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("patch_suppress")
	updated, err := h.flowSvc.SuppressFindingInSource(r.Context(), doc, req.BlockID, req.RuleID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, updated)
}

// handleListSourceSnapshots returns the flow's undo ring (newest last).
// Editor access (restoring is an edit; listing previews it).
// @Summary      List undo snapshots
// @Tags         flow
// @Produce      json
// @Success      200 {object} map[string]interface{} "Snapshots"
// @Router       /api/flow/snapshots [get]
func (h *FlowHandler) handleListSourceSnapshots(w http.ResponseWriter, r *http.Request) {
	// Kept on the editor role check, not Can's FlowEdit: that would demand the write scope of a read token on this GET.
	doc, ok := resolveFlowByRole(w, r, h.flowSvc, h.security, r.URL.Query().Get("flowId"), "editor")
	if !ok {
		return
	}
	render.JSON(w, map[string]any{"snapshots": h.flowSvc.ListSourceSnapshots(doc)})
}

// handleRestoreSourceSnapshot writes a snapshot's bytes back (undo for the last fix/batch/save),
// snapshotting the current state first so a restore is itself undoable.
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
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("snapshot_restore")
	updated, err := h.flowSvc.RestoreSourceSnapshot(r.Context(), doc, req.SnapshotID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]any{"document": updated})
}

// @Summary      Get flow file change signal
// @Tags         flow
// @Produce      json
// @Success      200 {object} map[string]interface{} "Meta"
// @Router       /api/flow/source-meta [get]
func (h *FlowHandler) handleGetSourceMeta(w http.ResponseWriter, r *http.Request) {
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, r.URL.Query().Get("flowId"), service.FlowRead)
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

// @Summary      Get raw flow source
// @Description  handleGetSource returns the raw PAD source text for the flow. Desktop: reads the file (folder flows: the member named by ?file=, Main.txt when omitted). Cloud: returns the stored source. Viewer access.
// @Tags         flow
// @Param        file query string false "Member file of a folder flow (bare filename)"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Source"
// @Failure      400 {object} map[string]string "Bad Request (file is not a member of the flow folder)"
// @Router       /api/flow/source [get]
func (h *FlowHandler) handleGetSource(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flowId")
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, flowID, service.FlowRead)
	if !ok {
		return
	}
	source, err := h.flowSvc.GetSource(doc, r.URL.Query().Get("file"))
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"source": source})
}

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
		// File names the member of a desktop folder flow being saved (bare
		// filename; Main.txt when empty).
		File string `json:"file"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FlowID == "" {
		render.Error(w, fmt.Errorf("flowId is required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("save_source")
	updated, err := h.flowSvc.SaveSource(r.Context(), doc, req.Source, req.File)
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

// registerSourceRoutes wires the /api/flow source, suppression and snapshot
// sub-routes.
func (h *FlowHandler) registerSourceRoutes(rb *route.Builder) {
	// Both rewrite and re-parse the whole source (up to 10 MB): outside the request transaction.
	rb.Post("/suppress-in-source", h.handleSuppressInSource, route.NoTx())
	rb.Get("/snapshots", h.handleListSourceSnapshots)
	rb.Post("/snapshots/restore", h.handleRestoreSourceSnapshot)
	rb.Get("/source", h.handleGetSource)
	rb.Get("/source-meta", h.handleGetSourceMeta)
	rb.Post("/save-source", h.handleSaveSource, route.NoTx())
}
