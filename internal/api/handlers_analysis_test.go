package api

import (
	"net/http"
	"testing"
)

// --- Bad-body 400 tests ---

func TestHandleGetVariableLineage_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/analysis/lineage")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleSetRuleEnabled_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/analysis/rule/enabled")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleUpdateRuleConfig_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/analysis/rule/config")
	checkStatus(t, rr, http.StatusBadRequest)
}

// --- No-body endpoints (Guard-protected → 500 with nil app services) ---

func TestHandleAnalyzeFlow_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}

func TestHandleGetExecutionGraph_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/analysis/graph", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}

// GET /api/analysis/rules: App.GetRules() now nil-checks a.analysis, so it
// returns JSON null (not a panic) when the app is uninitialized.
func TestHandleGetRules_UninitializedAppReturnsJSON(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/analysis/rules", nil)
	checkStatus(t, rr, http.StatusOK)
}
