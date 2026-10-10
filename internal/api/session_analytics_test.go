package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

const sessionTestFlow = "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''http://intranet/a''' Method: HTTPClient.Method.GET\n#EndRegion\n"

// The desktop session analytics (GET /api/analysis/dashboard and the local home dashboard) count the flows
// analyzed since the last base folder was opened: load-folder resets them through the analyzer the analysis
// service serves from.
func TestDesktopSessionAnalytics_LoadFolderResets(t *testing.T) {
	rt := newTestRouter(nil, false)
	dir := t.TempDir()
	file := filepath.Join(dir, "flow.txt")
	if err := os.WriteFile(file, []byte(sessionTestFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	counts := func() (session, home int) {
		t.Helper()
		var stats struct {
			TotalFlowsAnalyzed int `json:"totalFlowsAnalyzed"`
		}
		rr := doRequest(t, rt, http.MethodGet, "/api/analysis/dashboard", nil)
		checkStatus(t, rr, http.StatusOK)
		decodeJSON(t, rr, &stats)
		var h struct {
			Overview struct {
				TotalFlows int `json:"totalFlows"`
			} `json:"overview"`
		}
		rr = doRequest(t, rt, http.MethodGet, "/api/dashboard/home", nil)
		checkStatus(t, rr, http.StatusOK)
		decodeJSON(t, rr, &h)
		return stats.TotalFlowsAnalyzed, h.Overview.TotalFlows
	}

	checkStatus(t, doRequest(t, rt, http.MethodPost, "/api/flow/load-path", map[string]string{"path": file}), http.StatusOK)
	checkStatus(t, doRequest(t, rt, http.MethodPost, "/api/analysis/analyze", nil), http.StatusOK)
	if s, h := counts(); s != 1 || h != 1 {
		t.Fatalf("after one analysis: session %d, home %d flows; want 1 and 1", s, h)
	}
	// Re-analyzing the same file keeps one entry.
	checkStatus(t, doRequest(t, rt, http.MethodPost, "/api/analysis/analyze", nil), http.StatusOK)
	if s, _ := counts(); s != 1 {
		t.Fatalf("after a re-analysis: session %d flows, want 1", s)
	}

	checkStatus(t, doRequest(t, rt, http.MethodPost, "/api/flow/load-folder", map[string]string{"path": dir}), http.StatusOK)
	if s, h := counts(); s != 0 || h != 0 {
		t.Errorf("after opening a folder: session %d, home %d flows; want 0 and 0", s, h)
	}
}

// B2 end to end: analyze-raw's documented CI use posts the same unchanged flow on every run. The second post used to
// hit the first parse's cached report, whose block ids the second document does not have, so its SARIF lost every
// line number.
func TestAnalyzeRaw_RepeatKeepsSARIFLines(t *testing.T) {
	rt := newTestRouter(nil, false)
	body := map[string]any{"files": map[string]string{"Main.txt": sessionTestFlow}, "name": "Main", "format": "sarif"}
	for run := 1; run <= 2; run++ {
		rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", body)
		checkStatus(t, rr, http.StatusOK)
		var sarif struct {
			Runs []struct {
				Results []struct {
					RuleID    string `json:"ruleId"`
					Locations []struct {
						PhysicalLocation struct {
							Region *struct {
								StartLine int `json:"startLine"`
							} `json:"region"`
						} `json:"physicalLocation"`
					} `json:"locations"`
				} `json:"results"`
			} `json:"runs"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &sarif); err != nil {
			t.Fatalf("run %d: decode SARIF: %v", run, err)
		}
		if len(sarif.Runs) != 1 || len(sarif.Runs[0].Results) == 0 {
			t.Fatalf("run %d: no results", run)
		}
		for _, r := range sarif.Runs[0].Results {
			if len(r.Locations) == 0 || r.Locations[0].PhysicalLocation.Region == nil || r.Locations[0].PhysicalLocation.Region.StartLine == 0 {
				t.Errorf("run %d: result %s has no start line", run, r.RuleID)
			}
		}
	}
}
