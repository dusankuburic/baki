package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/models"
)

// decodeOptional decodes JSON from r into v but tolerates an empty body. A
// malformed (non-empty) body still produces an error so callers can return
// HTTP 400 instead of letting zero-value request fields fall through and
// generate a confusing downstream error.
func decodeOptional(r io.Reader, v any) error {
	if err := json.NewDecoder(r).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// @Summary Analyze current flow
// @Description Runs all enabled analysis rules on the current flow document.
// @Tags analysis
// @Produce json
// @Success 200 {object} models.AnalysisReport
// @Failure 500 {object} map[string]string
// @Router /api/analysis/analyze [post]
func (rt *Router) handleAnalyzeFlow(w http.ResponseWriter, r *http.Request) {
	// flowId is required in cloud mode (identifies + authorizes the target flow)
	// and ignored in local mode (operates on the current document).
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	doc, ok := rt.resolveFlow(w, r, req.FlowID, "viewer")
	if !ok {
		return
	}
	res, err := rt.app.AnalyzeFlow(doc)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

// @Summary Get variable lineage
// @Description Returns the history and dependencies of a specific variable within the flow.
// @Tags analysis
// @Accept json
// @Produce json
// @Param request body object{varName=string} true "Variable Lineage Request"
// @Success 200 {object} models.VariableHistory
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/analysis/lineage [post]
func (rt *Router) handleGetVariableLineage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Variable string `json:"varName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if req.Variable == "" {
		rt.sendError(w, fmt.Errorf("varName is required"), http.StatusBadRequest)
		return
	}
	doc, ok := rt.resolveFlow(w, r, req.FlowID, "viewer")
	if !ok {
		return
	}
	history, err := rt.app.GetVariableLineage(doc, req.Variable)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, history)
}

// @Summary Get execution graph
// @Description Returns a graph representation of the flow's execution path.
// @Tags analysis
// @Produce json
// @Success 200 {object} models.GraphData
// @Failure 500 {object} map[string]string
// @Router /api/analysis/graph [get]
func (rt *Router) handleGetExecutionGraph(w http.ResponseWriter, r *http.Request) {
	// flowId identifies + authorizes the target flow in cloud mode and is ignored
	// in local mode. Accept it from the body (frontend POSTs) or the query string.
	flowID := r.URL.Query().Get("flowId")
	if flowID == "" && r.Body != nil {
		var req struct {
			FlowID string `json:"flowId"`
		}
		if err := decodeOptional(r.Body, &req); err != nil {
			rt.sendError(w, err, http.StatusBadRequest)
			return
		}
		flowID = req.FlowID
	}
	doc, ok := rt.resolveFlow(w, r, flowID, "viewer")
	if !ok {
		return
	}
	graph, err := rt.app.GetExecutionGraph(doc)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, graph)
}

// @Summary List analysis rules
// @Description Returns all available analysis rules and their current configuration.
// @Tags analysis
// @Produce json
// @Success 200 {array} models.Rule
// @Router /api/analysis/rules [get]
func (rt *Router) handleGetRules(w http.ResponseWriter, r *http.Request) {
	rules := rt.app.GetRules()
	rt.sendJSON(w, rules)
}

// @Summary Enable/disable analysis rule
// @Description Enables or disables a specific analysis rule by ID.
// @Tags analysis
// @Accept json
// @Produce json
// @Param request body object{id=string,enabled=bool} true "Set Rule Enabled Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/analysis/rule/enabled [post]
func (rt *Router) handleSetRuleEnabled(w http.ResponseWriter, r *http.Request) {
	// Analysis-rule config is global server state; only members may change it.
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}
	var req struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.SetRuleEnabled(req.ID, req.Enabled); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}

// @Summary Update analysis rule config
// @Description Updates the configuration parameters for a specific analysis rule.
// @Tags analysis
// @Accept json
// @Produce json
// @Param request body object{id=string,config=models.RuleConfig} true "Update Rule Config Request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/analysis/rule/config [post]
func (rt *Router) handleUpdateRuleConfig(w http.ResponseWriter, r *http.Request) {
	// Analysis-rule config is global server state; only members may change it.
	if !rt.requireRole(w, r, auth.RoleMember) {
		return
	}
	var req struct {
		ID     string            `json:"id"`
		Config models.RuleConfig `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	if err := rt.app.UpdateRuleConfig(req.ID, req.Config); err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, map[string]string{"status": "ok"})
}
