package middleware

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"

	"pad-analyzer/internal/api/render"
)

// privateRanges lists all IPv4/IPv6 private and loopback address ranges.
// Requests from these CIDRs are allowed to scrape /metrics; all others get 403.
//
// NOTE: When running behind a TLS-terminating reverse proxy (PAD_BEHIND_PROXY=true)
// the proxy's IP becomes RemoteAddr. Internal Azure Monitor / Prometheus scrapers
// that reach the pod directly through the cluster network will have a private-range
// IP, so this check is effective. For full protection on public deployments also
// block /metrics at the Azure NSG / ACA ingress level so it is never reachable via
// the public load balancer.
var privateRanges = func() []*net.IPNet {
	cidrs := []string{
		"127.0.0.0/8",    // IPv4 loopback
		"::1/128",        // IPv6 loopback
		"10.0.0.0/8",     // RFC-1918
		"172.16.0.0/12",  // RFC-1918
		"192.168.0.0/16", // RFC-1918
		"100.64.0.0/10",  // shared address space (RFC-6598, common for pod CIDRs)
		"fc00::/7",       // IPv6 unique local (ULA)
	}
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, n, _ := net.ParseCIDR(c)
		nets = append(nets, n)
	}
	return nets
}()

func isPrivateIP(ipStr string) bool {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return false
	}
	for _, r := range privateRanges {
		if r != nil && r.Contains(ip) {
			return true
		}
	}
	return false
}

// MetricsGuard returns middleware that restricts access to private/loopback IPs.
// Requests from public IPs are rejected with 403 Forbidden.
//
// IMPORTANT: We deliberately use r.RemoteAddr (the actual TCP peer) rather than
// ClientIP, because ClientIP honours X-Forwarded-For whose leftmost entry is
// client-controlled and trivially spoofable. The metrics endpoint must only be
// reachable from inside the cluster network.
//
// When token is non-empty, a matching bearer token is ALSO required (constant
// time compared). This is defense-in-depth against a misconfigured NSG/ingress
// exposing /metrics via the public load balancer: even if a public IP somehow
// reaches the pod, scraping still needs the shared secret.
func MetricsGuard(_ []string, token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if !isPrivateIP(host) {
				render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
				return
			}
			if token != "" {
				// Case-insensitive scheme match (RFC 7235 §2.1), consistent with
				// auth.ExtractToken: a scraper sending "bearer <token>" must work.
				got := extractBearerToken(r.Header.Get("Authorization"))
				if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
					render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// extractBearerToken returns the token portion of a "Bearer <token>"
// Authorization header, matching the scheme case-insensitively (RFC 7235
// §2.1) so "bearer"/"BEARER" work like "Bearer". Returns "" when the header
// is absent or uses a different scheme.
func extractBearerToken(header string) string {
	if idx := strings.IndexByte(header, ' '); idx == len("bearer") &&
		strings.EqualFold(header[:idx], "bearer") {
		return strings.TrimSpace(header[idx+1:])
	}
	return ""
}
