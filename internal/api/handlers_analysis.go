package api

import (
	"encoding/json"
	"net/http"
	"pad-analyzer/internal/models"
)

func (rt *Router) handleAnalyzeFlow(w http.ResponseWriter, r *http.Request) {
	res, err := rt.app.AnalyzeFlow()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, res)
}

func (rt *Router) handleGetVariableLineage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Variable string `json:"varName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rt.sendError(w, err, http.StatusBadRequest)
		return
	}
	history, err := rt.app.GetVariableLineage(req.Variable)
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, history)
}

func (rt *Router) handleGetExecutionGraph(w http.ResponseWriter, r *http.Request) {
	graph, err := rt.app.GetExecutionGraph()
	if err != nil {
		rt.sendError(w, err, http.StatusInternalServerError)
		return
	}
	rt.sendJSON(w, graph)
}

func (rt *Router) handleGetRules(w http.ResponseWriter, r *http.Request) {
	rules := rt.app.GetRules()
	rt.sendJSON(w, rules)
}

func (rt *Router) handleSetRuleEnabled(w http.ResponseWriter, r *http.Request) {
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

func (rt *Router) handleUpdateRuleConfig(w http.ResponseWriter, r *http.Request) {
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
