package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// seedAnalyzableFlow inserts a flow whose Content is valid (empty) flow JSON, so
// FlowService.GetAuthorized (which resolves + unmarshals Content) succeeds. The
// triage endpoints all go through GetAuthorized, so a Content-less seed would
// fail to resolve regardless of permissions.
func seedAnalyzableFlow(t *testing.T, rt *Router, id, ownerID string) {
	t.Helper()
	doc := &storageif.FlowDocument{
		ID:      id,
		Name:    "t",
		OwnerID: ownerID,
		Content: json.RawMessage(`{"id":"` + id + `","name":"t","subflows":[]}`),
	}
	if err := rt.security.Backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
}

func TestTriage_SetListClear_RoundTrip(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	set := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/set", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "hardcoded-credential:b1", "ruleId": "hardcoded-credential",
		"status": "suppressed", "justification": "false positive in test data",
	})
	checkStatus(t, set, http.StatusOK)

	list := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/list", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, list, http.StatusOK)
	var statuses []*storageif.FindingStatus
	decodeJSON(t, list, &statuses)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Status != "suppressed" || statuses[0].FindingKey != "hardcoded-credential:b1" {
		t.Errorf("unexpected status: %+v", statuses[0])
	}
	if statuses[0].UpdatedBy != "alice" {
		t.Errorf("expected UpdatedBy=alice (the caller), got %q", statuses[0].UpdatedBy)
	}

	clear := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/clear", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "hardcoded-credential:b1",
	})
	checkStatus(t, clear, http.StatusOK)

	list2 := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/list", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, list2, http.StatusOK)
	var after []*storageif.FindingStatus
	decodeJSON(t, list2, &after)
	if len(after) != 0 {
		t.Errorf("expected 0 statuses after clear, got %d", len(after))
	}
}

func TestTriage_Set_NonOwnerForbidden(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/set", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "r1:b1", "status": "resolved",
	})
	checkStatus(t, rr, http.StatusForbidden)
}

func TestTriage_Set_InvalidStatus400(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/triage/set", bearer, map[string]any{
		"flowId": "flow1", "findingKey": "r1:b1", "status": "bogus",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestTriage_Unavailable_WhenNoBackend503(t *testing.T) {
	rt := newTestRouter(nil, false) // desktop/in-memory: no storage backend
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/triage/list", map[string]any{"flowId": "flow1"})
	checkStatus(t, rr, http.StatusServiceUnavailable)
}

// seedFlowWithFinding inserts a flow whose single block has a hardcoded URL,
// which reliably trips the hardcoded-url analyzer rule, so analysis yields at
// least one finding for drift/baseline assertions.
func seedFlowWithFinding(t *testing.T, rt *Router, id, ownerID string) {
	t.Helper()
	content := `{"id":"` + id + `","name":"t","subflows":[` +
		`{"id":"Main","name":"Main","blocks":[` +
		`{"id":"b1","name":"HTTP","type":"ACTION","rawType":"Web.Call","subflowId":"Main","lineNumber":1,` +
		`"properties":{"url":"https://api.example.com/v2/users"}}` +
		`]}]}`
	doc := &storageif.FlowDocument{
		ID:      id,
		Name:    "t",
		OwnerID: ownerID,
		Content: json.RawMessage(content),
	}
	if err := rt.security.Backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
}

func TestBaseline_DriftRatchet(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedFlowWithFinding(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// No baseline yet → every finding is new.
	d1 := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/drift", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, d1, http.StatusOK)
	var drift1 struct {
		HasBaseline bool             `json:"hasBaseline"`
		New         []map[string]any `json:"new"`
	}
	decodeJSON(t, d1, &drift1)
	if drift1.HasBaseline {
		t.Error("expected HasBaseline=false before any baseline is set")
	}
	if len(drift1.New) == 0 {
		t.Fatal("expected at least one new finding before baseline")
	}

	// Accept the current findings as the baseline.
	set := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/set", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, set, http.StatusOK)

	// Re-run drift: same flow, so nothing is new since baseline.
	d2 := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/drift", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, d2, http.StatusOK)
	var drift2 struct {
		HasBaseline bool             `json:"hasBaseline"`
		New         []map[string]any `json:"new"`
	}
	decodeJSON(t, d2, &drift2)
	if !drift2.HasBaseline {
		t.Error("expected HasBaseline=true after setting a baseline")
	}
	if len(drift2.New) != 0 {
		t.Errorf("expected 0 new findings after baselining the same flow, got %d", len(drift2.New))
	}
}

func TestBaseline_SetGetClear(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedAnalyzableFlow(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// No baseline yet → JSON null.
	get := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/get", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, get, http.StatusOK)
	if body := get.Body.String(); body != "null\n" && body != "null" {
		t.Errorf("expected null baseline before set, got %q", body)
	}

	// Set: snapshots the (empty) flow's findings as the baseline.
	set := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/set", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, set, http.StatusOK)
	var bl storageif.FlowBaseline
	decodeJSON(t, set, &bl)
	if bl.FlowID != "flow1" {
		t.Errorf("expected baseline flowId=flow1, got %q", bl.FlowID)
	}
	if bl.CreatedBy != "alice" {
		t.Errorf("expected baseline createdBy=alice, got %q", bl.CreatedBy)
	}

	get2 := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/get", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, get2, http.StatusOK)
	var got storageif.FlowBaseline
	decodeJSON(t, get2, &got)
	if got.FlowID != "flow1" {
		t.Errorf("expected persisted baseline, got %+v", got)
	}

	clear := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/clear", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, clear, http.StatusOK)

	get3 := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/get", bearer, map[string]any{"flowId": "flow1"})
	checkStatus(t, get3, http.StatusOK)
	if body := get3.Body.String(); body != "null\n" && body != "null" {
		t.Errorf("expected null baseline after clear, got %q", body)
	}
}
