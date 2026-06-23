package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip       string
		expected bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.1", true},
		{"172.16.5.10", true},
		{"192.168.1.1", true},
		{"100.64.0.1", true},
		{"fc00::1", true},
		{"fd00::1", true},
		{"8.8.8.8", false},
		{"203.0.113.5", false},
		{"169.254.1.1", false},
		{"invalid", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			if got := isPrivateIP(tt.ip); got != tt.expected {
				t.Errorf("isPrivateIP(%q) = %v, want %v", tt.ip, got, tt.expected)
			}
		})
	}
}

func TestMetricsGuard_PrivateIPAllowed(t *testing.T) {
	handler := MetricsGuard(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 from private IP, got %d", rr.Code)
	}
}

func TestMetricsGuard_PublicIPBlocked(t *testing.T) {
	handler := MetricsGuard(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for public IP")
	}))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 from public IP, got %d", rr.Code)
	}
}

func TestMetricsGuard_LoopbackAllowed(t *testing.T) {
	handler := MetricsGuard(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 from loopback, got %d", rr.Code)
	}
}

func TestMetricsGuard_TrustedProxyXFF(t *testing.T) {
	handler := MetricsGuard([]string{"10.0.0.0/8"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "192.168.1.50")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 (private IP via trusted proxy XFF), got %d", rr.Code)
	}
}

func TestMetricsGuard_PublicViaTrustedProxyXFF(t *testing.T) {
	// After the XFF bypass fix, the guard uses RemoteAddr only — a spoofed
	// X-Forwarded-For header must NOT bypass the private-IP check.
	// Protection against public /metrics access relies on network ACLs
	// (NSG/ingress), not on the client-controlled XFF header.
	handler := MetricsGuard([]string{"10.0.0.0/8"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "10.0.0.1:12345"            // private (trusted proxy)
	req.Header.Set("X-Forwarded-For", "8.8.8.8") // spoofed public XFF
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 (RemoteAddr is private; XFF is ignored), got %d", rr.Code)
	}
}
