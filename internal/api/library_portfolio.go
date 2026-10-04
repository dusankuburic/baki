package api

import (
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/service"
)

// @Summary      Flow portfolio view
// @Description  handlePortfolio returns the org-wide governance portfolio: every flow the caller can access (optionally filtered by ?orgId=), ordered by ?sort= and trimmed to ?limit= entries. Rollup totals always cover every ranked flow.
// @Tags         library
// @Param        orgId query string false "Only flows in this organisation"
// @Param        sort query string false "Entry order: risk (default, worst health first), health, errors, name, analyzed (most recent first)" Enums(risk, health, errors, name, analyzed)
// @Param        limit query integer false "Maximum entries returned (default and cap 1000)"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Portfolio"
// @Failure      400 {object} map[string]string "Bad Request"
// @Router       /api/library/portfolio [get]
func (h *LibraryHandler) handlePortfolio(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	q := r.URL.Query()
	orgID := q.Get("orgId")
	sortBy, err := service.ParsePortfolioSort(q.Get("sort"))
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	// 0 (absent) means every ranked flow; the service caps the ranking itself.
	limit, ok := parseIntParam(w, q.Get("limit"), "limit", 0)
	if !ok {
		return
	}
	if limit < 0 {
		render.Error(w, fmt.Errorf("invalid \"limit\" parameter: must not be negative"), http.StatusBadRequest)
		return
	}
	p, err := h.libSvc.BuildPortfolio(r.Context(), userID, orgID, sortBy, limit)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	render.JSON(w, p)
}
