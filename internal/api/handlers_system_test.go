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

// TestMetricsEndpoint_ReturnsPrometheusFormat verifies /metrics returns the
// Prometheus exposition format when accessed from a private/loopback IP.
// In production the endpoint is gated by an IP allowlist (MetricsGuard) and
// additionally by network policy — no JWT is required. The test simulates
// a Prometheus / Azure Monitor scraper running inside the same cluster network.
func TestMetricsEndpoint_ReturnsPrometheusFormat(t *testing.T) {
	rt := newJWTTestRouter(t)

	// httptest.NewRequest defaults to "192.0.2.1" (documentation range, public).
	// Override to loopback to pass the MetricsGuard private-IP check.
	req, _ := http.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:9090"

	rr := newRecorder(t, rt, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/metrics: expected 200, got %d (body: %.200s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "# HELP") {
		t.Errorf("/metrics did not return Prometheus exposition format; got: %.200s", body)
	}
	// Go runtime collectors are always registered and always emit series, so
	// go_goroutines is the most reliable smoke signal that the registry is
	// wired through correctly.
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("expected go_goroutines (runtime collector) in /metrics output")
	}
}

// TestMetricsEndpoint_BlockedFromPublicIP verifies MetricsGuard rejects
// requests from public IP addresses.
func TestMetricsEndpoint_BlockedFromPublicIP(t *testing.T) {
	rt := newJWTTestRouter(t)

	req, _ := http.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "203.0.113.5:1234" // TEST-NET-3, a public documentation range

	rr := newRecorder(t, rt, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("/metrics from public IP: expected 403, got %d", rr.Code)
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
