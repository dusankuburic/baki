package middleware

import (
	"net"
	"net/http"
	"strings"
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
func MetricsGuard(_ []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if !isPrivateIP(host) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
