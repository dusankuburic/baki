package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "test-secret-token"

// newTestRouter creates a Router with a fresh App and a fixed token.
// The App is not fully initialised (Init is not called), so only routes that
// don't reach the app layer can be tested here. Handler-level tests live in
// handlers_test.go and require a fully initialised App.
func newLocalTestRouter() *Router {
	return newTestRouter(nil, false)
}

// --- Auth middleware ---

func TestRouter_MissingAuth_Returns401(t *testing.T) {
	rt := newLocalTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/api/system/info", nil)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestRouter_WrongToken_Returns401(t *testing.T) {
	rt := newLocalTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/api/system/info", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestRouter_TokenInQuery_RejectedOnNonSSEPath(t *testing.T) {
	rt := newLocalTestRouter()

	// Non-SSE path: token in query must be rejected (401) so access JWTs don't
	// leak into proxy/browser logs. The Authorization header is the only
	// accepted channel outside /api/events (whose acceptance is covered by the
	// auth package's ExtractToken/Middleware unit tests).
	req := httptest.NewRequest(http.MethodGet, "/api/system/info?token="+testToken, nil)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("non-SSE path: token in query must be rejected, got status %d", rr.Code)
	}

	// Sanity: the same token via the header still works on this path.
	req = httptest.NewRequest(http.MethodGet, "/api/system/info", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr = httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	if rr.Code == http.StatusUnauthorized {
		t.Error("non-SSE path: same token via header should be accepted")
	}
}

// --- CORS ---

func TestRouter_OPTIONS_Returns200WithCORSHeaders(t *testing.T) {
	// Router with an explicit allowlist — the listed origin must be echoed back.
	rt := newTestRouter(nil, false)
	rt.AllowedOrigins = []string{"https://app.example.com"}

	req := httptest.NewRequest(http.MethodOptions, "/api/system/info", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for OPTIONS, got %d", rr.Code)
	}
	if origin := rr.Header().Get("Access-Control-Allow-Origin"); origin != "https://app.example.com" {
		t.Errorf("expected CORS origin to be echoed back, got %q", origin)
	}
	if methods := rr.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(methods, "POST") {
		t.Errorf("expected CORS methods to include POST, got %q", methods)
	}
}

func TestRouter_OPTIONS_UnknownOrigin_NoACO(t *testing.T) {
	rt := newTestRouter(nil, false)

	req := httptest.NewRequest(http.MethodOptions, "/api/system/info", nil)
	req.Header.Set("Origin", "https://evil.com")
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for OPTIONS, got %d", rr.Code)
	}
	if origin := rr.Header().Get("Access-Control-Allow-Origin"); origin != "" {
		t.Errorf("expected no ACAO header for unknown origin, got %q", origin)
	}
}

// --- 404 routing ---

func TestRouter_UnknownPath_Returns404(t *testing.T) {
	rt := newLocalTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/api/nonexistent", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

// --- SSE endpoint ---

func TestRouter_EventsEndpoint_SetsSSEHeaders(t *testing.T) {
	rt := newLocalTestRouter()

	// Use a short-lived context so the blocking event loop exits promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()

	// Run in a goroutine because the handler blocks until context is done.
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.ServeHTTP(rr, req)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not return after context cancellation")
	}

	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("expected SSE content-type, got %q", ct)
	}
}

// --- Emit ---

func TestRouter_Emit_DoesNotPanicWithNoClients(t *testing.T) {
	rt := newLocalTestRouter()
	// Should be a no-op when no SSE clients are connected
	rt.eventManager.Emit("test-event", map[string]string{"key": "value"})
}

// TestIsLocalhostOrigin_TauriWebviewOrigins guards the desktop CORS allowlist.
// The Windows Tauri v2 webview origin is http://tauri.localhost; missing it
// meant the production desktop build's requests to the sidecar all failed CORS
// (preflight 200 but no Access-Control-Allow-Origin) and nothing loaded.
func TestIsLocalhostOrigin_TauriWebviewOrigins(t *testing.T) {
	allowed := []string{
		"http://tauri.localhost",  // Tauri v2 webview on Windows (WebView2)
		"https://tauri.localhost", // https variant
		"tauri://localhost",       // Tauri webview on macOS/Linux
		"http://localhost:5173",   // Vite dev server
		"http://127.0.0.1:9210",   // ephemeral sidecar port
	}
	for _, o := range allowed {
		if !isLocalhostOrigin(o) {
			t.Errorf("expected origin %q to be allowed in local mode", o)
		}
	}

	rejected := []string{
		"http://localhost.evil.com",       // look-alike host must NOT match
		"http://tauri.localhost.evil.com", // look-alike host must NOT match
		"http://example.com",
		"https://evil.com",
		"ftp://localhost",
	}
	for _, o := range rejected {
		if isLocalhostOrigin(o) {
			t.Errorf("expected origin %q to be rejected", o)
		}
	}
}
