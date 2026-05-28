package api

import (
	"net/http"
	"testing"
)

// --- Bad-body 400 tests ---

func TestHandleTestProviderConnection_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/providers/test")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandlePollGitHubAuth_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/providers/github/poll")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandlePollCopilotAuth_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/providers/copilot/poll")
	checkStatus(t, rr, http.StatusBadRequest)
}

// --- No-body endpoints (Guard-protected → 500 with nil app services) ---

func TestHandleListProviders_ReturnsStaticList(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/providers/list", nil)
	// The provider list is statically defined in ProviderService; no Init() required.
	checkStatus(t, rr, http.StatusOK)
	var providers []any
	decodeJSON(t, rr, &providers)
	if len(providers) == 0 {
		t.Error("expected at least one provider in the list")
	}
}

func TestHandleStartGitHubAuth_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/providers/github/start", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}

func TestHandleRevokeGitHubAuth_RouteIsReachable(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/providers/github/revoke", nil)
	// Actual result (200 or 500) depends on keyring state; just verify route + auth work.
	if rr.Code == http.StatusUnauthorized || rr.Code == http.StatusNotFound {
		t.Errorf("expected route to be reachable, got %d", rr.Code)
	}
}

func TestHandleGetGitHubUser_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/providers/github/user", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}

func TestHandleStartCopilotAuth_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/providers/copilot/start", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}

func TestHandleRevokeCopilotAuth_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/providers/copilot/revoke", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}

func TestHandleGetCopilotUser_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/providers/copilot/user", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}
