package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
)

// pprofRequest issues a request to a pprof path with a PRIVATE RemoteAddr.
//
// The private address is the whole point. MetricsGuard's allowlist keys off
// r.RemoteAddr, and behind a reverse proxy — PAD_BEHIND_PROXY=true, which the
// Dockerfile sets and infra/main.bicep's external ACA ingress implies —
// RemoteAddr is the proxy's own private address for requests arriving from the
// public internet. So "private RemoteAddr" IS the remote-attacker case here, and
// a test that used a public address would prove nothing: that path was already
// blocked before this fix.
func pprofRequest(t *testing.T, rt *Router, path, authz string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "10.1.2.3:41234"
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)
	return rec
}

// TestPprof_NotRegisteredByDefault is the regression test for the exposure:
// /debug/pprof/* used to be registered unconditionally behind a guard that a
// reverse proxy defeats, handing out a dump of live process memory (auth
// secret, decrypted provider keys, flow content, chat history) to anyone.
//
// It is now opt-in, and when the opt-in is off the route does not exist at all —
// a stronger property than "the guard says no", since an unregistered route
// cannot be reached by any guard bug.
func TestPprof_NotRegisteredByDefault(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), true)

	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/goroutine",
		"/debug/pprof/cmdline",
		"/debug/pprof/profile",
		"/debug/pprof/trace",
		"/debug/pprof/symbol",
	} {
		t.Run(path, func(t *testing.T) {
			rec := pprofRequest(t, rt, path, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s from a private RemoteAddr: got %d, want 404 — the profiler must not be registered unless PAD_PPROF_ENABLED is set", path, rec.Code)
			}
		})
	}
}

// TestPprof_EnabledRequiresToken proves the opt-in path still authenticates.
// Config validation guarantees a token is present whenever pprof is on (see
// TestValidate_PprofRequiresMetricsToken), so this covers what the guard does
// with it.
func TestPprof_EnabledRequiresToken(t *testing.T) {
	const token = "s3cret-metrics-token"
	rt := newTestRouterSSO(testutil.NewFakeBackend(), true, nil, nil, func(c *config.Config) {
		c.Server.PprofEnabled = true
		c.Server.MetricsToken = token
	})

	tests := []struct {
		name  string
		authz string
		want  int
	}{
		{"no header", "", http.StatusForbidden},
		{"wrong token", "Bearer nope", http.StatusForbidden},
		{"right token", "Bearer " + token, http.StatusOK},
		{"lowercase scheme", "bearer " + token, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := pprofRequest(t, rt, "/debug/pprof/heap", tc.authz)
			if rec.Code != tc.want {
				t.Fatalf("GET /debug/pprof/heap with %q: got %d, want %d", tc.authz, rec.Code, tc.want)
			}
		})
	}
}

// TestPprof_EnabledStillBlocksPublicIP confirms enabling the profiler does not
// drop the IP allowlist — the token is an ADDITIONAL factor, not a replacement.
func TestPprof_EnabledStillBlocksPublicIP(t *testing.T) {
	const token = "s3cret-metrics-token"
	rt := newTestRouterSSO(testutil.NewFakeBackend(), true, nil, nil, func(c *config.Config) {
		c.Server.PprofEnabled = true
		c.Server.MetricsToken = token
	})

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	req.RemoteAddr = "203.0.113.7:41234"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("public IP with a valid token: got %d, want 403 — the IP allowlist must still apply", rec.Code)
	}
}
