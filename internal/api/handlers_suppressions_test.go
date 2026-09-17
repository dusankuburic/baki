package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/storage/filesystem"
)

// TestHandleSuppressionInventory_ListsAndFlagsStale pins C10's route: the
// inventory the CLI computes is reachable over HTTP, a directive on a block
// where its rule fires is live, and one where it does not is stale.
func TestHandleSuppressionInventory_ListsAndFlagsStale(t *testing.T) {
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	rt := newTestRouter(fs, false) // local mode: resolveFlow uses the loaded doc

	flow := "#Region \"Main\"\n" +
		"# pad-ignore[missing-timeout]\n" +
		"HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x'\n" +
		"# pad-ignore[missing-timeout]\n" +
		"WAIT 1\n" +
		"#EndRegion\n"
	file := filepath.Join(t.TempDir(), "flow.txt")
	if err := os.WriteFile(file, []byte(flow), 0o600); err != nil {
		t.Fatalf("write flow: %v", err)
	}
	checkStatus(t, doRequest(t, rt, http.MethodPost, "/api/flow/load-path", map[string]string{"path": file}), http.StatusOK)

	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/suppressions", map[string]string{"flowId": ""})
	checkStatus(t, rr, http.StatusOK)
	var entries []struct {
		Line   int    `json:"line"`
		Rule   string `json:"rule"`
		Stale  bool   `json:"stale"`
		Reason string `json:"reason"`
	}
	decodeJSON(t, rr, &entries)
	if len(entries) != 2 {
		t.Fatalf("expected 2 suppression entries, got %+v", entries)
	}
	if entries[0].Rule != "missing-timeout" || entries[0].Stale {
		t.Errorf("directive before the HTTP call should be live: %+v", entries[0])
	}
	if !entries[1].Stale || entries[1].Reason == "" {
		t.Errorf("directive before WAIT should be stale with a reason: %+v", entries[1])
	}
}

func TestHandleSuppressionInventory_NoFlowLoadedReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/suppressions", map[string]string{"flowId": ""})
	checkStatus(t, rr, http.StatusBadRequest)
}

// Cloud mode resolves the flow through GetAuthorized("viewer"): a caller with
// no access to the flow must not read its suppressions.
func TestHandleSuppressionInventory_RequiresFlowAccess(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-1", "alice")
	bob := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/suppressions", bob, map[string]string{"flowId": "flow-1"})
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 403/404 for a caller without access; body=%s", rr.Code, rr.Body.String())
	}
}
