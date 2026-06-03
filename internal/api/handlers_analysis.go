package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/service"
)

type AnalysisHandler struct {
	analysisSvc *service.AnalysisService
	flowSvc     *service.FlowService
	security    *SecurityConfig
}

func NewAnalysisHandler(analysisSvc *service.AnalysisService, flowSvc *service.FlowService, security *SecurityConfig) *AnalysisHandler {
	return &AnalysisHandler{
		analysisSvc: analysisSvc,
		flowSvc:     flowSvc,
		security:    security,
	}
}

func (h *AnalysisHandler) handleAnalyzeFlow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID string `json:"flowId"`
	}
	if err := decodeOptional(r.Body, &req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	
	userID := h.security.CallerID(r)
	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "viewer")
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	res, err := h.analysisSvc.AnalyzeFlow(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
}

func (h *AnalysisHandler) handleGetVariableLineage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID   string `json:"flowId"`
		Variable string `json:"varName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if req.Variable == "" {
		render.Error(w, fmt.Errorf("varName is required"), http.StatusBadRequest)
		return
	}

	userID := h.security.CallerID(r)
	doc, err := h.flowSvc.GetAuthorized(r.Context(), req.FlowID, userID, "viewer")
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	history, err := h.analysisSvc.GetVariableLineage(doc, req.Variable)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, history)
}

func (h *AnalysisHandler) handleGetExecutionGraph(w http.ResponseWriter, r *http.Request) {
	flowID := r.URL.Query().Get("flowId")
	if flowID == "" && r.Body != nil {
		var req struct {
			FlowID string `json:"flowId"`
		}
		if err := decodeOptional(r.Body, &req); err != nil {
			render.Error(w, err, http.StatusBadRequest)
			return
		}
		flowID = req.FlowID
	}
	
	userID := h.security.CallerID(r)
	doc, err := h.flowSvc.GetAuthorized(r.Context(), flowID, userID, "viewer")
	if err != nil {
		render.Error(w, err, 0)
		return
	}

	graph, err := h.analysisSvc.GetExecutionGraph(doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, graph)
}

func (h *AnalysisHandler) handleGetRules(w http.ResponseWriter, r *http.Request) {
	rules := h.analysisSvc.GetRules()
	render.JSON(w, rules)
}

func (h *AnalysisHandler) handleSetRuleEnabled(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleMember) {
		return
	}
	var req struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := h.analysisSvc.SetRuleEnabled(req.ID, req.Enabled); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func (h *AnalysisHandler) handleUpdateRuleConfig(w http.ResponseWriter, r *http.Request) {
	if !h.security.RequireRole(w, r, auth.RoleMember) {
		return
	}
	var req struct {
		ID     string            `json:"id"`
		Config models.RuleConfig `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	if err := h.analysisSvc.UpdateRuleConfig(req.ID, req.Config); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, map[string]string{"status": "ok"})
}

func decodeOptional(r io.Reader, v any) error {
	if r == nil {
		return nil
	}
	if err := json.NewDecoder(r).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
