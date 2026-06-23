package api

import (
	"net/http"
	"testing"
)

// --- Bad-body 400 tests ---

func TestHandleRevokeCopilotAuth_NoTokenIsIdempotent(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/providers/copilot/revoke", nil)
	// Revoking when nothing is stored is a successful no-op.
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleGetCopilotUser_NoTokenReturnsNotConnected(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/providers/copilot/user", nil)
	// No stored token → "not connected" (200, null body), not a server error.
	checkStatus(t, rr, http.StatusOK)
}
