package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// handleApplyFix applies a deterministic auto-fix to a block in the source
// file. Desktop/local only; the finding carries the fixType in its AutoFix field.
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
		// Unsafe is the explicit opt-in for behaviour-changing fixers; without
		// it such a fix is a 400 (service gate).
		Unsafe bool `json:"unsafe"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.BlockID == "" || req.FixType == "" {
		render.Error(w, fmt.Errorf("blockId and fixType are required"), http.StatusBadRequest)
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
	if !ok {
		return
	}
	metrics.RecordFlowOp("patch_fix")
	updated, err := h.flowSvc.ApplyFix(r.Context(), doc, req.BlockID, req.FixType, req.RuleID, req.Variable, req.Property, req.Unsafe)
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

// handlePreviewFix returns the before/after source for a fix WITHOUT writing.
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
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
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

// handleApplyFixBatch applies every auto-fixable finding whose rule is in the
// request's set, re-parsing between fixes so line shifts don't corrupt targets.
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
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, service.FlowEdit)
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
	out, err := h.flowSvc.ApplyFixBatchResult(r.Context(), doc, ruleFilter, req.Limit)
	if err != nil {
		if errors.Is(err, storageif.ErrVersionConflict) {
			render.Error(w, fmt.Errorf("flow was modified concurrently; reload and retry"), http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// `partial`: the deadline ended the loop early; the applied fixes ARE persisted and the client can
	// re-run. The timeout middleware only cancels r.Context(), so this response still reaches the client.
	resp := map[string]any{"document": out.Doc, "applied": out.Applied, "partial": out.Partial}
	if out.Partial && out.Reason != nil {
		resp["partialReason"] = out.Reason.Error()
	}
	render.JSON(w, resp)
}

// registerFixRoutes wires the /api/flow fix-application sub-routes. Each resolves the flow, then loops
// parse+analyze+patch over sources of up to 10 MB: outside the request transaction.
func (h *FlowHandler) registerFixRoutes(rb *route.Builder) {
	rb.Post("/apply-fix", h.handleApplyFix, route.NoTx())
	rb.Post("/apply-fix-batch", h.handleApplyFixBatch, route.NoTx())
	rb.Post("/preview-fix", h.handlePreviewFix, route.NoTx())
}
