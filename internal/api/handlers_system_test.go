package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
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

// stubRedisPinger is a RedisPinger stand-in: err is what a live Ping answers (the admin health page), degraded what
// the state view says (the readiness probe).
type stubRedisPinger struct {
	err      error
	degraded bool
}

func (f stubRedisPinger) Ping(_ context.Context) error { return f.err }
func (f stubRedisPinger) Degraded() bool               { return f.degraded }

func probeReady(t *testing.T, h *SystemHandler) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleReadiness(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz body %q: %v", w.Body.String(), err)
	}
	return w.Code, body
}

// TestReadinessProbe_RedisDownIsDegradedNotUnready: every mechanism that uses Redis has a declared on-down
// behaviour, so an outage must not take a replica out of rotation (it used to: 503 after three probes, on every
// replica at once). The probe stays 200 and says so: {"status":"ok","degraded":["redis"]}.
func TestReadinessProbe_RedisDownIsDegradedNotUnready(t *testing.T) {
	h := &SystemHandler{
		health:      service.NewHealthService(testutil.NewFakeBackend(), nil),
		redisPinger: stubRedisPinger{err: errors.New("redis: connection refused"), degraded: true},
	}
	for i := 1; i <= 2*readinessFailureThreshold; i++ {
		code, body := probeReady(t, h)
		if code != http.StatusOK {
			t.Fatalf("probe %d: status %d, want 200 however long Redis is down", i, code)
		}
		if body["status"] != "ok" {
			t.Errorf("probe %d: status key = %v, want ok (the existing key is unchanged)", i, body["status"])
		}
		if d, _ := body["degraded"].([]any); len(d) != 1 || d[0] != "redis" {
			t.Errorf("probe %d: degraded = %v, want [redis]", i, body["degraded"])
		}
	}
}

// A healthy Redis adds nothing to the body.
func TestReadinessProbe_HealthyRedisAddsNoKey(t *testing.T) {
	h := &SystemHandler{
		health:      service.NewHealthService(testutil.NewFakeBackend(), nil),
		redisPinger: stubRedisPinger{},
	}
	code, body := probeReady(t, h)
	if code != http.StatusOK || len(body) != 1 || body["status"] != "ok" {
		t.Errorf("status %d, body %v; want 200 and exactly {status: ok}", code, body)
	}
}

// The database still gates readiness (503 after the consecutive-failure threshold) whatever Redis says, and a
// 503 keeps its error body.
func TestReadinessProbe_DatabaseDownStillFailsWithRedisDegraded(t *testing.T) {
	backend := testutil.NewFakeBackend()
	backend.PingErr = errors.New("db down")
	h := &SystemHandler{
		health:      service.NewHealthService(backend, nil),
		redisPinger: stubRedisPinger{degraded: true},
	}
	for i := 1; i <= readinessFailureThreshold; i++ {
		w := httptest.NewRecorder()
		h.handleReadiness(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if want := map[bool]int{true: http.StatusServiceUnavailable, false: http.StatusOK}[i >= readinessFailureThreshold]; w.Code != want {
			t.Errorf("probe %d: status %d, want %d", i, w.Code, want)
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
// returns a structured per-subsystem payload. The JWT test router uses the
// fake backend, so DB=ok, blob=skipped (the fake has no blob container),
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
		t.Errorf("blob status = %q, want skipped (the fake has no blob)", resp.Blob.Status)
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

// The admin page keeps reporting Redis as degraded (a live ping, not the smoothed state), and a Redis that
// answers as ok.
func TestAdminSystemHealth_RedisDownIsDegraded(t *testing.T) {
	for name, tc := range map[string]struct {
		pinger  stubRedisPinger
		status  string
		overall string
	}{
		"down":    {stubRedisPinger{err: errors.New("dial tcp: connection refused"), degraded: true}, "error", "degraded"},
		"healthy": {stubRedisPinger{}, "ok", "ok"},
	} {
		t.Run(name, func(t *testing.T) {
			h := &SystemHandler{health: service.NewHealthService(testutil.NewFakeBackend(), nil), redisPinger: tc.pinger}
			w := httptest.NewRecorder()
			h.handleAdminSystemHealth(w, httptest.NewRequest(http.MethodGet, "/api/admin/system/health", nil))
			var resp adminHealthResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || resp.Redis.Status != tc.status || resp.Overall != tc.overall {
				t.Errorf("status %d, redis %q, overall %q; want 200, %q, %q", w.Code, resp.Redis.Status, resp.Overall, tc.status, tc.overall)
			}
		})
	}
}
