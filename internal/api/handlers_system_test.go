package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"
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

// failingRedisPinger is a RedisPinger stand-in that always returns an error,
// used to prove /readyz reports unhealthy when Redis (the optional backplane)
// is configured but unreachable.
type failingRedisPinger struct{ err error }

func (f failingRedisPinger) Ping(_ context.Context) error { return f.err }

// TestReadinessProbe_503AfterNConsecutiveRedisFailures guards H21: when Redis
// is configured (non-nil pinger) and unreachable, the probe must accumulate
// failures and return 503 after readinessFailureThreshold consecutive hits.
// Previously /readyz did not check Redis at all — an outage silently degraded
// multi-replica correctness (rate limiter, hub presence, chat-resume all fail
// open) without taking the pod out of rotation.
func TestReadinessProbe_503AfterNConsecutiveRedisFailures(t *testing.T) {
	// Use FakeBackend (always-healthy Ping) so the backend check passes and we
	// isolate the Redis check failure path.
	h := &SystemHandler{
		backend:     testutil.NewFakeBackend(),
		redisPinger: failingRedisPinger{err: errors.New("redis: connection refused")},
	}
	for i := 1; i <= readinessFailureThreshold; i++ {
		w := httptest.NewRecorder()
		h.handleReadiness(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if i < readinessFailureThreshold {
			if w.Code != http.StatusOK {
				t.Errorf("probe %d: expected 200 (below threshold), got %d", i, w.Code)
			}
		} else {
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("probe %d: expected 503 (at threshold), got %d", i, w.Code)
			}
		}
	}
}

// TestReadinessProbe_NilRedisPingerSkipsCheck proves single-replica mode
// (Redis unconfigured) continues to pass readiness without trying to ping a
// nil client.
func TestReadinessProbe_NilRedisPingerSkipsCheck(t *testing.T) {
	h := &SystemHandler{redisPinger: nil}
	w := httptest.NewRecorder()
	h.handleReadiness(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when Redis pinger is nil, got %d", w.Code)
	}
}

// TestAdminSystemHealth_AdminGetsBreakdown verifies the admin health endpoint
// returns a structured per-subsystem payload. The JWT test router uses a
// filesystem backend, so DB=ok, blob=skipped (filesystem has no blob backend),
// Redis=skipped (nil pinger), overall=ok.
func TestAdminSystemHealth_AdminGetsBreakdown(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/system/health", bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp adminHealthResponse
	decodeJSON(t, rr, &resp)
	if resp.Database.Status != "ok" {
		t.Errorf("database status = %q, want ok", resp.Database.Status)
	}
	if resp.Blob.Status != "skipped" {
		t.Errorf("blob status = %q, want skipped (filesystem has no blob)", resp.Blob.Status)
	}
	if resp.Redis.Status != "skipped" {
		t.Errorf("redis status = %q, want skipped (no pinger wired)", resp.Redis.Status)
	}
	if resp.Overall != "ok" {
		t.Errorf("overall = %q, want ok", resp.Overall)
	}
}

// TestAdminSystemHealth_NonAdmin_Forbidden proves the admin gate applies.
func TestAdminSystemHealth_NonAdmin_Forbidden(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "member-1", "member@example.com", auth.RoleMember)
	pair, err := rt.security.AuthMgr.Issue("member-1", "member@example.com", auth.RoleMember)
	if err != nil {
		t.Fatalf("issue member jwt: %v", err)
	}
	bearer := "Bearer " + pair.AccessToken

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/system/health", bearer, nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}
