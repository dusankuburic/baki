package api

import (
	"net/http"
	"testing"
)

func TestPortfolio_ListsAccessibleFlows(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow1", "alice")
	seed("flow2", "alice")
	seed("flow3", "bob") // not alice's — must not appear in her portfolio
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/portfolio", bearer, nil)
	checkStatus(t, rr, http.StatusOK)

	var p struct {
		TotalFlows    int `json:"totalFlows"`
		AnalyzedFlows int `json:"analyzedFlows"`
		Entries       []struct {
			FlowID   string `json:"flowId"`
			Analyzed bool   `json:"analyzed"`
		} `json:"entries"`
	}
	decodeJSON(t, rr, &p)

	if p.TotalFlows != 2 || len(p.Entries) != 2 {
		t.Fatalf("expected alice's 2 flows, got total=%d entries=%d", p.TotalFlows, len(p.Entries))
	}
	for _, e := range p.Entries {
		if e.FlowID == "flow3" {
			t.Error("portfolio leaked another user's flow")
		}
		// Filesystem backend persists no health, so flows are unanalyzed here.
		if e.Analyzed {
			t.Errorf("expected unanalyzed flow in local mode, got analyzed %s", e.FlowID)
		}
	}
}

func TestPortfolio_RequiresAuth(t *testing.T) {
	rt, _ := newLibraryTestRouter(t) // JWT enabled
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/portfolio", "", nil)
	checkStatus(t, rr, http.StatusUnauthorized)
}
