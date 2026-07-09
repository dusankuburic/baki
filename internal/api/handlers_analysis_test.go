package api

import (
	"net/http"
	"testing"
)

// --- Bad-body 400 tests ---

func TestHandleGetVariableLineage_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/analysis/lineage")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleSetRuleEnabled_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/analysis/rule/enabled")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleUpdateRuleConfig_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/analysis/rule/config")
	checkStatus(t, rr, http.StatusBadRequest)
}

// --- Analysis endpoints with no flow loaded ---
// With explicit flow resolution, an uninitialized app (no current document)
// yields a clean 400 "no flow loaded" from resolveFlow rather than a panic→500.

func TestHandleAnalyzeFlow_NoFlowLoadedReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze", nil)
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleGetExecutionGraph_NoFlowLoadedReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/graph", nil)
	checkStatus(t, rr, http.StatusBadRequest)
}

// GET /api/analysis/rules: App.GetRules() now nil-checks a.analysis, so it
// returns JSON null (not a panic) when the app is uninitialized.
func TestHandleGetRules_UninitializedAppReturnsJSON(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/analysis/rules", nil)
	checkStatus(t, rr, http.StatusOK)
}

// GET /api/analysis/rules/summary: the catalog rollup the dashboard consumes
// for its "auto-fixable rules" and "confidence distribution" KPIs. Must return
// non-zero totals even with an uninitialized app (no settings), since the
// catalog is static.
func TestHandleGetRulesSummary_ReturnsRollup(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/analysis/rules/summary", nil)
	checkStatus(t, rr, http.StatusOK)
	var got struct {
		TotalRules       int            `json:"totalRules"`
		AutoFixableRules int            `json:"autoFixableRules"`
		ByCategory       map[string]int `json:"byCategory"`
		ByConfidence     map[string]int `json:"byConfidence"`
	}
	decodeJSON(t, rr, &got)
	if got.TotalRules == 0 {
		t.Error("expected non-zero TotalRules")
	}
	if got.AutoFixableRules == 0 {
		t.Error("expected non-zero AutoFixableRules")
	}
	if len(got.ByCategory) == 0 {
		t.Error("expected non-empty ByCategory")
	}
	if len(got.ByConfidence) == 0 {
		t.Error("expected non-empty ByConfidence")
	}
}
