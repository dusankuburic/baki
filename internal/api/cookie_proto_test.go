package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsHTTPS_ForwardedProtoOnlyFromTrustedProxy: X-Forwarded-Proto decides
// the refresh cookie's Secure flag, so an arbitrary client must not be able to
// force it on (desktop login breakage) or off (plaintext cookie transit on a
// TLS deployment). Only a peer in the trusted-proxy list may speak for the
// proxied scheme.
func TestIsHTTPS_ForwardedProtoOnlyFromTrustedProxy(t *testing.T) {
	trusted := []string{"10.0.0.0/8"}

	mk := func(remote, xfp string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		r.RemoteAddr = remote
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		return r
	}

	if isHTTPS(mk("10.0.0.1:443", "https"), trusted) != true {
		t.Error("trusted proxy saying https must count")
	}
	if isHTTPS(mk("10.0.0.1:443", "http"), trusted) != false {
		t.Error("trusted proxy saying http must not count as TLS")
	}
	if isHTTPS(mk("203.0.113.5:9999", "https"), trusted) {
		t.Error("untrusted client claiming https must be ignored")
	}
	if isHTTPS(mk("203.0.113.5:9999", "http"), trusted) {
		t.Error("plain remote, untrusted header: not TLS")
	}
	if isHTTPS(mk("127.0.0.1:8080", "https"), trusted) {
		t.Error("loopback without being a trusted proxy must not count (desktop)")
	}
}

// TestRejectForeignOrigin: local-mode mutating endpoints accept unauthenticated
// requests by design behind the loopback bind, but a browser must not let a
// random website drive them cross-origin (a text/plain POST is a "simple
// request" — no preflight). Localhost/Tauri webview origins and configured
// allowed origins pass; everything else gets a 403; non-browser callers send
// no Origin and always pass.
func TestRejectForeignOrigin(t *testing.T) {
	sec := &SecurityConfig{AllowedOrigins: []string{"https://ui.example.com"}}

	cases := []struct {
		name     string
		origin   string
		rejected bool
	}{
		{"no origin (curl, tests)", "", false},
		{"localhost dev server", "http://localhost:5173", false},
		{"loopback ip", "http://127.0.0.1:5173", false},
		{"ipv6 loopback dev server", "http://[::1]:5173", false},
		{"tauri webview (macOS/Linux)", "tauri://localhost", false},
		{"tauri webview (Windows)", "http://tauri.localhost", false},
		{"configured origin", "https://ui.example.com", false},
		{"foreign site", "https://evil.example", true},
		{"look-alike of localhost", "http://localhost.evil.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/flow/reveal", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			if got := sec.rejectForeignOrigin(w, r); got != tc.rejected {
				t.Errorf("rejectForeignOrigin = %v, want %v", got, tc.rejected)
			}
			if tc.rejected && w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
		})
	}

	// JWT mode carries its own CSRF posture; the guard must be inert there.
	secJWT := &SecurityConfig{JWTEnabled: true}
	r := httptest.NewRequest(http.MethodPost, "/api/flow/upload", nil)
	r.Header.Set("Origin", "https://evil.example")
	if secJWT.rejectForeignOrigin(httptest.NewRecorder(), r) {
		t.Error("guard must not fire when JWT auth is enabled")
	}
}

// TestRejectForeignOrigin_SameHostPasses: a local deployment reached under a
// custom hostname serves its own SPA; those same-origin POSTs (Origin's
// host:port == the request Host) must pass, while a foreign Origin naming a
// different host against the same Host must not.
func TestRejectForeignOrigin_SameHostPasses(t *testing.T) {
	sec := &SecurityConfig{} // no allowed_origins at all

	same := httptest.NewRequest(http.MethodPost, "/api/flow/upload", nil)
	same.Host = "mybox.local:8080"
	same.Header.Set("Origin", "http://mybox.local:8080")
	if sec.rejectForeignOrigin(httptest.NewRecorder(), same) {
		t.Error("same-origin POST must pass without needing allowed_origins")
	}

	foreign := httptest.NewRequest(http.MethodPost, "/api/flow/upload", nil)
	foreign.Host = "mybox.local:8080"
	foreign.Header.Set("Origin", "http://evil.example")
	if !sec.rejectForeignOrigin(httptest.NewRecorder(), foreign) {
		t.Error("foreign origin against the same Host must be rejected")
	}
}

// TestLocalOriginGuard: the router-wide local-mode guard refuses cross-origin
// mutations, lets reads and same-origin mutations through, and is inert with
// JWT auth on.
func TestLocalOriginGuard(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	localRT := &Router{security: &SecurityConfig{}}

	mk := func(method, origin string) *http.Request {
		r := httptest.NewRequest(method, "/api/flow/save-source", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	guard := localRT.localOriginGuard(next)
	w := httptest.NewRecorder()
	guard.ServeHTTP(w, mk(http.MethodPost, "https://evil.example"))
	if w.Code != http.StatusForbidden {
		t.Errorf("foreign POST: status %d, want 403", w.Code)
	}
	w = httptest.NewRecorder()
	guard.ServeHTTP(w, mk(http.MethodGet, "https://evil.example"))
	if w.Code != http.StatusOK {
		t.Errorf("foreign GET must pass (response is CORS-unreadable): status %d", w.Code)
	}
	w = httptest.NewRecorder()
	guard.ServeHTTP(w, mk(http.MethodPost, "http://localhost:5173"))
	if w.Code != http.StatusOK {
		t.Errorf("localhost-origin POST: status %d, want 200", w.Code)
	}
	w = httptest.NewRecorder()
	guard.ServeHTTP(w, mk(http.MethodPost, ""))
	if w.Code != http.StatusOK {
		t.Errorf("no-origin POST (curl, tests): status %d, want 200", w.Code)
	}

	cloudRT := &Router{security: &SecurityConfig{JWTEnabled: true}}
	w = httptest.NewRecorder()
	cloudRT.localOriginGuard(next).ServeHTTP(w, mk(http.MethodPost, "https://evil.example"))
	if w.Code != http.StatusOK {
		t.Errorf("JWT mode must be inert: status %d, want 200", w.Code)
	}
}
