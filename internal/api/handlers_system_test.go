package api

import (
	"net/http"
	"strings"
	"testing"
)

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
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/keys/save")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleDeleteApiKey_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/keys/delete")
	checkStatus(t, rr, http.StatusBadRequest)
}
