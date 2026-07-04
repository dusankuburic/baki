package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP_NoTrustedProxies(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	if ip := ClientIP(req, nil); ip != "203.0.113.5" {
		t.Errorf("expected 203.0.113.5, got %s", ip)
	}
}

func TestClientIP_UntrustedProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if ip := ClientIP(req, []string{"10.0.0.0/8"}); ip != "203.0.113.5" {
		t.Errorf("expected remote IP (untrusted), got %s", ip)
	}
}

func TestClientIP_TrustedProxyUsesXFF(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.42")
	if ip := ClientIP(req, []string{"10.0.0.0/8"}); ip != "198.51.100.42" {
		t.Errorf("expected XFF IP, got %s", ip)
	}
}

func TestClientIP_TrustedProxyMultipleXFF(t *testing.T) {
	// The trusted proxy appends the real observed client IP as the RIGHTMOST
	// entry; anything to its left is client-supplied and must not be trusted.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.42, 10.0.0.2, 203.0.113.7")
	if ip := ClientIP(req, []string{"10.0.0.0/8"}); ip != "203.0.113.7" {
		t.Errorf("expected rightmost (proxy-appended) XFF IP, got %s", ip)
	}
}

func TestClientIP_SpoofedLeftmostXFFIgnored(t *testing.T) {
	// A client pre-populates X-Forwarded-For to try to control the reported IP.
	// The trusted proxy appends the real client IP after it, so the rightmost
	// (real) value wins and the spoofed leftmost is ignored.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.42")
	if ip := ClientIP(req, []string{"10.0.0.0/8"}); ip != "198.51.100.42" {
		t.Errorf("spoofed leftmost XFF should be ignored, got %s", ip)
	}
}

func TestClientIP_TrustedProxyNoXFF(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	if ip := ClientIP(req, []string{"10.0.0.0/8"}); ip != "10.0.0.1" {
		t.Errorf("expected remote IP when no XFF, got %s", ip)
	}
}

func TestClientIP_TrustedByPlainIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	if ip := ClientIP(req, []string{"10.0.0.1"}); ip != "198.51.100.99" {
		t.Errorf("expected XFF IP with plain IP trust, got %s", ip)
	}
}

func TestClientIP_TrimsWhitespaceInXFF(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "  198.51.100.42  ")
	if ip := ClientIP(req, []string{"10.0.0.0/8"}); ip != "198.51.100.42" {
		t.Errorf("expected trimmed XFF IP, got %q", ip)
	}
}

func TestClientIP_EmptyTrustedProxiesSkipped(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "198.51.100.42")
	if ip := ClientIP(req, []string{"", "  ", "10.0.0.0/8"}); ip != "198.51.100.42" {
		t.Errorf("expected XFF IP (empty entries skipped), got %s", ip)
	}
}

func TestClientIP_NoHostPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5"
	if ip := ClientIP(req, nil); ip != "203.0.113.5" {
		t.Errorf("expected 203.0.113.5, got %s", ip)
	}
}
