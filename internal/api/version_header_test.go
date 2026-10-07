package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/service"
)

// TestServeHTTP_BuildVersionHeader: every response names the running build, whatever the route or its outcome, so a
// tab kept open across a deploy can tell its bundle is stale.
func TestServeHTTP_BuildVersionHeader(t *testing.T) {
	if service.Version == "" {
		t.Fatal("service.Version is empty: the header would carry nothing to compare")
	}
	rt := newJWTTestRouter(t)
	for _, path := range []string{"/healthz", "/api/system/info", "/api/nonexistent"} {
		rr := doRequestWithAuth(t, rt, http.MethodGet, path, "", nil)
		if got := rr.Header().Get(BuildVersionHeader); got != service.Version {
			t.Errorf("GET %s (%d): %s = %q, want %q", path, rr.Code, BuildVersionHeader, got, service.Version)
		}
	}
}

// TestCORS_ExposesBuildVersionHeader: a cross-origin client may read a response header only when it is exposed — the
// desktop webview (tauri.localhost against the sidecar) and an allow-listed origin both compare the version.
func TestCORS_ExposesBuildVersionHeader(t *testing.T) {
	cloud := newJWTTestRouter(t)
	cloud.AllowedOrigins = []string{"https://app.example.com"}
	local := newLocalTestRouter()

	cases := []struct {
		name    string
		rt      *Router
		origin  string
		exposed bool
	}{
		{"allow-listed origin", cloud, "https://app.example.com", true},
		{"desktop webview", local, "http://tauri.localhost", true},
		{"foreign origin", cloud, "https://evil.example", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Header.Set("Origin", c.origin)
			rr := httptest.NewRecorder()
			c.rt.ServeHTTP(rr, req)
			exposed := strings.Contains(rr.Header().Get("Access-Control-Expose-Headers"), BuildVersionHeader)
			if exposed != c.exposed {
				t.Errorf("Origin %s: Access-Control-Expose-Headers = %q, exposes %s = %v, want %v",
					c.origin, rr.Header().Get("Access-Control-Expose-Headers"), BuildVersionHeader, exposed, c.exposed)
			}
		})
	}
}
