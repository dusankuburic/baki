package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/config"
	mailer "pad-analyzer/internal/mail"
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

// TestNewAnalysisHandler_EmailWired is a regression test for a bug where the
// email field was accepted as a parameter but never assigned to the struct,
// silently disabling all finding-assignment/comment email notifications.
func TestNewAnalysisHandler_EmailWired(t *testing.T) {
	svc := mailer.NewService(config.EmailConfig{}, config.ModeLocal)
	h := NewAnalysisHandler(nil, nil, nil, nil, nil, nil, svc, "")
	if h.email == nil {
		t.Fatal("email field is nil — notifications are dead code; assign email in the constructor")
	}
}

// POST /api/analysis/custom-rules/validate: authoring feedback without
// installing — per-entry validity + errors, array or single-object payloads,
// clean 400 on neither shape. The backend for the settings rule editor.
func TestHandleValidateCustomRules(t *testing.T) {
	rt := newTestRouter(nil, false)

	t.Run("mixed array flags the invalid entry", func(t *testing.T) {
		body := map[string]any{"rules": []map[string]any{
			{"id": "ok", "name": "fine", "rawTypeMatch": "Labels\\."},
			{"id": "bad", "name": "broken regex", "nameMatch": "*["},
		}}
		rr := doRequest(t, rt, http.MethodPost, "/api/analysis/custom-rules/validate", body)
		checkStatus(t, rr, http.StatusOK)
		var res struct {
			Valid   int `json:"valid"`
			Invalid int `json:"invalid"`
			Entries []struct {
				ID    string `json:"id"`
				Valid bool   `json:"valid"`
				Error string `json:"error"`
			} `json:"entries"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if res.Valid != 1 || res.Invalid != 1 {
			t.Errorf("valid=%d invalid=%d, want 1/1", res.Valid, res.Invalid)
		}
		if len(res.Entries) != 2 || res.Entries[0].ID != "ok" || !res.Entries[0].Valid {
			t.Errorf("entries[0] wrong: %+v", res.Entries)
		}
		if res.Entries[1].Valid || res.Entries[1].Error == "" {
			t.Errorf("invalid entry missing error: %+v", res.Entries[1])
		}
	})

	t.Run("single object accepted", func(t *testing.T) {
		body := map[string]any{"rules": map[string]any{"id": "one", "name": "x", "rawTypeMatch": "Labels\\."}}
		rr := doRequest(t, rt, http.MethodPost, "/api/analysis/custom-rules/validate", body)
		checkStatus(t, rr, http.StatusOK)
	})

	t.Run("garbage returns 400", func(t *testing.T) {
		rr := doRequest(t, rt, http.MethodPost, "/api/analysis/custom-rules/validate", map[string]any{"rules": "not-rules"})
		checkStatus(t, rr, http.StatusBadRequest)
	})
}
