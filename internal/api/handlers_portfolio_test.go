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

func TestPortfolio_SortAndLimit(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seed("flow-b", "alice")
	seed("flow-a", "alice")
	seed("flow-c", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/portfolio?sort=name&limit=2", bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var p struct {
		TotalFlows int `json:"totalFlows"`
		Entries    []struct {
			FlowName string `json:"flowName"`
		} `json:"entries"`
	}
	decodeJSON(t, rr, &p)
	// The limit trims the ranked entries, not the rollup.
	if p.TotalFlows != 3 {
		t.Errorf("totalFlows = %d, want 3 (limit must not shrink the totals)", p.TotalFlows)
	}
	if len(p.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(p.Entries))
	}
	if p.Entries[0].FlowName > p.Entries[1].FlowName {
		t.Errorf("sort=name returned %q before %q", p.Entries[0].FlowName, p.Entries[1].FlowName)
	}
}

func TestPortfolio_RejectsBadSortAndLimit(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	for _, qs := range []string{"?sort=updated_desc", "?limit=abc", "?limit=-1"} {
		rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/portfolio"+qs, bearer, nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", qs, rr.Code)
		}
	}
}
