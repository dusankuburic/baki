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

// metricsClientIP extracts the real client IP, honouring X-Forwarded-For only
// when the request's immediate peer is one of the trusted-proxy IPs/CIDRs.
func metricsClientIP(r *http.Request, trustedProxies []string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if len(trustedProxies) == 0 || !isTrustedProxy(host, trustedProxies) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx >= 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	return host
}

func isTrustedProxy(remoteIP string, proxies []string) bool {
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if strings.Contains(p, "/") {
			if _, cidr, err := net.ParseCIDR(p); err == nil && cidr.Contains(ip) {
				return true
			}
		} else if net.ParseIP(p) != nil && net.ParseIP(p).Equal(ip) {
			return true
		}
	}
	return false
}

// MetricsGuard returns middleware that restricts access to private/loopback IPs.
// Requests from public IPs are rejected with 403 Forbidden.
// Pass cfg.Server.TrustedProxies so X-Forwarded-For is honoured when the
// service is behind an ingress controller that injects the real client IP.
func MetricsGuard(trustedProxies []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := metricsClientIP(r, trustedProxies)
			if !isPrivateIP(ip) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
