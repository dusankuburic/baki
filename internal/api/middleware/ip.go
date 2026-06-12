package middleware

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP extracts the real client IP from r, trusting X-Forwarded-For only
// when the request's immediate peer matches one of trustedProxies (plain IPs or
// CIDR blocks). trustedProxies is parsed on each call, so this is intended for
// low-frequency callers (e.g. SSE connect); request hot paths should use a
// RateLimiter, which pre-parses the list once.
func ClientIP(r *http.Request, trustedProxies []string) string {
	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remoteIP); err == nil {
		remoteIP = host
	}
	if len(trustedProxies) == 0 {
		return remoteIP
	}
	peer := net.ParseIP(remoteIP)
	if peer == nil {
		return remoteIP
	}
	trusted := false
	for _, p := range trustedProxies {
		entry := strings.TrimSpace(p)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(peer) {
				trusted = true
				break
			}
			continue
		}
		if ip := net.ParseIP(entry); ip != nil && ip.Equal(peer) {
			trusted = true
			break
		}
	}
	if !trusted {
		return remoteIP
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remoteIP
	}
	if idx := strings.IndexByte(xff, ','); idx >= 0 {
		return strings.TrimSpace(xff[:idx])
	}
	return strings.TrimSpace(xff)
}
