package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestSearchLibrary_LocalModeIs503 pins the desktop answer for cross-flow
// search: with no storage backend there is no library, and the route says so
// (503) instead of returning an empty result that reads as "no matches".
func TestSearchLibrary_LocalModeIs503(t *testing.T) {
	rt := newTestRouter(nil, false) // desktop/in-memory: no storage backend
	rr := doRequest(t, rt, http.MethodPost, "/api/flow/search-library", map[string]any{
		"query": map[string]any{"text": "anything"},
	})
	checkStatus(t, rr, http.StatusServiceUnavailable)
	// 5xx messages are masked by render.writeError, so the client tells this
	// apart from an empty result by the status and machine code, not the text.
	if !strings.Contains(rr.Body.String(), "SERVICE_UNAVAILABLE") {
		t.Errorf("expected the SERVICE_UNAVAILABLE code in the body, got %s", rr.Body.String())
	}
}

// TestApplyFix_BehaviourChangingNeedsUnsafeField is the HTTP half of the F6
// gate: POST /api/flow/apply-fix refuses a behaviour-changing fixer with 400
// unless the body carries unsafe: true (the frontend sends it only from the
// path that has shown the preview diff).
func TestApplyFix_BehaviourChangingNeedsUnsafeField(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	const source = "#Region \"Main\"\nHTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x'\n#EndRegion\n"
	doc := seedCloudFlowWithSource(t, rt, "flow1", "alice", source)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	blockID := doc.Subflows[0].Blocks[0].ID
	body := map[string]any{
		"flowId": "flow1", "blockId": blockID, "fixType": "replace-with-variable", "ruleId": "hardcoded-url", "property": "Url",
	}
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/apply-fix", bearer, body)
	checkStatus(t, rr, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "unsafe") {
		t.Errorf("400 body should tell the caller about the unsafe opt-in, got %s", rr.Body.String())
	}

	body["unsafe"] = true
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/apply-fix", bearer, body)
	checkStatus(t, rr, http.StatusOK)
}
