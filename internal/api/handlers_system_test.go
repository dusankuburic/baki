package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestHandleAppInfo_ReturnsVersionField(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/system/info", nil)
	checkStatus(t, rr, http.StatusOK)

	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if _, ok := resp["version"]; !ok {
		t.Error("response missing 'version' field")
	}
}

func TestHandleGetSettings_ReturnsOK(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/system/settings", nil)
	checkStatus(t, rr, http.StatusOK)

	if ct := rr.Header().Get("Content-Type"); ct == "" {
		t.Error("expected non-empty Content-Type")
	}
}

func TestHandleUpdateSettings_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/system/settings")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleUpdateSettings_ValidBodyReturnsResponse(t *testing.T) {
	rt := newTestRouter()
	// App not fully initialised, so settings store is nil → expect 500.
	// The important thing is that the body is parsed (no 400) and sent downstream.
	rr := doRequest(t, rt, http.MethodPost, "/api/system/settings", map[string]any{})
	if rr.Code == http.StatusBadRequest {
		t.Errorf("valid body should not return 400, got %d", rr.Code)
	}
}

func TestHandleLogError_OK(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/system/log-error", map[string]string{
		"message": "boom",
		"stack":   "at line 1",
	})
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleLogError_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/system/log-error")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleHasApiKey_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/keys/has")
	checkStatus(t, rr, http.StatusBadRequest)
}

// TestLivenessProbe_AlwaysReturns200 locks in that /healthz never touches
// the DB. K8s liveness probes restart the pod on failure, so this endpoint
// must only fail when the process is genuinely dead — not when a downstream
// dependency hiccups. /readyz is the right place for that.
func TestLivenessProbe_AlwaysReturns200(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/healthz", "", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("/healthz: expected 200, got %d", rr.Code)
	}
}

// TestReadinessProbe_OkWhenBackendUp verifies /readyz returns 200 when
// the storage backend Ping succeeds.
func TestReadinessProbe_OkWhenBackendUp(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/readyz", "", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("/readyz with healthy backend: expected 200, got %d", rr.Code)
	}
}

// TestMetricsEndpoint_ReturnsPrometheusFormat verifies /metrics is
// reachable without auth (gated by network policy, not by JWT) and returns
// the Prometheus exposition format. Defensive against misrouting (HTML
// fallback) and against an accidental auth requirement that would break
// Prometheus scraping.
func TestMetricsEndpoint_ReturnsPrometheusFormat(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/metrics", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("/metrics: expected 200, got %d (body: %.200s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "# HELP") {
		t.Errorf("/metrics did not return Prometheus exposition format; got: %.200s", body)
	}
	// Go runtime collectors are registered at startup and always emit
	// series, so they're the most reliable smoke signal that the registry
	// is wired through. (http_requests_total only appears after the
	// Metrics middleware records at least one request, which this test
	// — hitting the router directly — does not exercise; that's fine.)
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("expected go_goroutines (runtime collector) in /metrics output")
	}
}

func TestHandleSaveApiKey_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/keys/save")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleDeleteApiKey_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/keys/delete")
	checkStatus(t, rr, http.StatusBadRequest)
}
