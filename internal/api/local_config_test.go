package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// decodeLocalConfig reads the /api/local-config response body.
func decodeLocalConfig(t *testing.T, rt *Router) map[string]string {
	t.Helper()
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/local-config", "", nil)
	checkStatus(t, rr, http.StatusOK)
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// TestLocalConfig_DoesNotServeTheTokenByDefault is the regression gate for a
// desktop-mode credential leak.
//
// /api/local-config is a PUBLIC route — no token required to call it — and in
// local mode isOriginAllowed reflects ANY loopback Origin. So while it returned
// the session token unconditionally, any page served from any localhost port (a
// Vite dev server, a Jupyter notebook, a locally-served HTML report) could scan
// ports and read the desktop app's token cross-origin. With that token it
// reaches every other /api route, including load-path (arbitrary file read) and
// the export endpoints (arbitrary file write outside cloud mode).
//
// The Tauri webview never calls this endpoint — it gets the token from
// invoke("get_backend_config"), which reads the 0600 session file. So the
// default must be to withhold it.
func TestLocalConfig_DoesNotServeTheTokenByDefault(t *testing.T) {
	rt := newTestRouter(nil, false) // local mode: JWT disabled
	body := decodeLocalConfig(t, rt)

	if body["mode"] != "local" {
		t.Errorf("mode = %q, want \"local\"", body["mode"])
	}
	if tok, ok := body["token"]; ok && tok != "" {
		t.Errorf("the session token was served to an unauthenticated caller on a route any "+
			"localhost-origin page can read: %q", tok)
	}
}

// TestLocalConfig_ServesTheTokenWhenExplicitlyEnabled keeps browser-based local
// development working — the one caller that legitimately needs the handoff.
func TestLocalConfig_ServesTheTokenWhenExplicitlyEnabled(t *testing.T) {
	rt := newTestRouter(nil, false)
	rt.security.Features.LocalConfigEndpoint = true

	if got := decodeLocalConfig(t, rt)["token"]; got == "" {
		t.Error("opting in returned no token — the dev handoff is broken, not gated")
	}
}

// TestLocalConfig_CloudModeNeverServesAToken guards the pre-existing behaviour
// the opt-in must not weaken.
func TestLocalConfig_CloudModeNeverServesAToken(t *testing.T) {
	rt := newTestRouter(nil, true) // cloud mode
	rt.security.Features.LocalConfigEndpoint = true

	body := decodeLocalConfig(t, rt)
	if body["mode"] != "cloud" {
		t.Errorf("mode = %q, want \"cloud\"", body["mode"])
	}
	if _, ok := body["token"]; ok {
		t.Error("cloud mode served a token even with the local-dev opt-in set")
	}
}
