package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRateLimiter_GetIP_TrustsXFFFromConfiguredProxy verifies the IP-extraction
// logic that the rate limiter uses to key its token buckets. The whole point
// of the trusted-proxies feature: only requests *coming from* an allowlisted
// peer get their X-Forwarded-For honored; otherwise the immediate peer IP is
// used and clients cannot spoof their identity via the header.
func TestRateLimiter_GetIP_TrustsXFFFromConfiguredProxy(t *testing.T) {
	rl := NewRateLimiter(60, 20, []string{"10.0.0.1", "192.168.0.0/16"})

	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{"no proxy, no XFF", "8.8.8.8:1234", "", "8.8.8.8"},
		{"untrusted peer ignores XFF", "8.8.8.8:1234", "1.2.3.4", "8.8.8.8"},
		{"trusted IP honors XFF", "10.0.0.1:1234", "1.2.3.4", "1.2.3.4"},
		{"trusted CIDR honors XFF", "192.168.5.5:1234", "1.2.3.4", "1.2.3.4"},
		{"trusted IP picks first XFF entry", "10.0.0.1:1234", "1.2.3.4, 5.6.7.8", "1.2.3.4"},
		{"trusted but no XFF falls back to peer", "10.0.0.1:1234", "", "10.0.0.1"},
		{"IPv6 peer", "[::1]:1234", "", "::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			got := rl.getIP(req)
			if got != tc.want {
				t.Errorf("getIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRateLimiter_GetIP_NoTrustedProxies_AlwaysUsesPeer(t *testing.T) {
	rl := NewRateLimiter(60, 20, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "8.8.8.8:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := rl.getIP(req); got != "8.8.8.8" {
		t.Errorf("getIP() = %q, want %q (XFF must be ignored with no proxies configured)", got, "8.8.8.8")
	}
}

func TestRateLimiter_GetIP_DropsMalformedProxyEntries(t *testing.T) {
	// Config-layer validation should reject these, but NewRateLimiter must
	// also defend itself — silently drop entries it can't parse rather than
	// panic.
	rl := NewRateLimiter(60, 20, []string{"", "  ", "not-an-ip", "10.0.0.0/99"})
	if len(rl.trustedIPs) != 0 {
		t.Errorf("expected 0 trustedIPs, got %d", len(rl.trustedIPs))
	}
	if len(rl.trustedCIDRs) != 0 {
		t.Errorf("expected 0 trustedCIDRs, got %d", len(rl.trustedCIDRs))
	}
}
