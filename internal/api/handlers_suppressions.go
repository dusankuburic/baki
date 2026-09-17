package api

import (
	"net/http"

	"pad-analyzer/internal/api/render"
)

// handleSuppressionInventory lists the inline `# pad-ignore` directives in a
// flow and flags the stale ones (directives that mask nothing today but would
// silently hide a future finding). The analyzer computed this for the CLI
// only: the app could write a directive into a flow ("Suppress in file") and
// had no way to list or audit them afterwards.
// @Summary      List inline suppressions
// @Description  handleSuppressionInventory lists every inline `# pad-ignore` directive in the flow and marks stale ones (the suppressed rule no longer fires on the target block). Audited against the flow's org rule profile. Viewer access.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Param        request body object true "{flowId}"
// @Success      200 {array} map[string]interface{} "Suppression entries: line, rule, blockLabel, blockType, subflow, stale, reason"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Router       /api/analysis/suppressions [post]
func (h *AnalysisHandler) handleSuppressionInventory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID string `json:"flowId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	doc, ok := resolveFlow(w, r, h.flowSvc, h.security, req.FlowID, "viewer")
	if !ok {
		return
	}
	entries, err := h.analysisSvc.SuppressionInventory(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, entries)
}
