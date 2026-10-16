package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
)

// ClientIP extracts the real client IP from r, trusting X-Forwarded-For only when the immediate
// peer matches one of trustedProxies. The list is parsed once per distinct configuration and
// shared (cachedProxySet), so per-request callers — audit logging, auth handlers, SSE keys —
// don't re-run net.ParseIP/ParseCIDR per call. Chain-owning middleware parses at construction.
func ClientIP(r *http.Request, trustedProxies []string) string {
	return cachedProxySet(trustedProxies).clientIP(r)
}

// PeerIsTrusted reports whether the immediate TCP peer of r matches one of
// trustedProxies. Callers use it to decide whether a forwarded header
// (X-Forwarded-For, X-Forwarded-Proto) may be believed: a client that is not
// a configured proxy must never influence decisions made from those headers.
func PeerIsTrusted(r *http.Request, trustedProxies []string) bool {
	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remoteIP); err == nil {
		remoteIP = host
	}
	return cachedProxySet(trustedProxies).trusts(remoteIP)
}

// proxySet is a trusted-proxy list parsed once (exact addresses and CIDR blocks). Malformed
// entries are silently dropped — config validation already rejects them; this is defense-in-depth.
type proxySet struct {
	ips   []net.IP
	cidrs []*net.IPNet
}

func parseProxySet(trustedProxies []string) proxySet {
	var s proxySet
	for _, p := range trustedProxies {
		entry := strings.TrimSpace(p)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil {
				s.cidrs = append(s.cidrs, cidr)
			}
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			s.ips = append(s.ips, ip)
		}
	}
	return s
}

// proxySetCache memoizes parseProxySet by the raw configuration's contents: ClientIP receives the
// list as a []string on every call (its callers are plain functions, not middleware constructors),
// and in practice a handful of distinct values exist for the process's life. The key joins entries
// with a NUL separator (never inside an IP or CIDR), so equal configurations share one entry.
// Bounded at maxProxySetCacheEntries — a table at capacity simply parses without storing.
// A shared proxySet is safe: trusts and clientIP only read it.
var proxySetCache = struct {
	sync.Mutex
	sets map[string]proxySet
}{sets: make(map[string]proxySet)}

// maxProxySetCacheEntries caps proxySetCache — distinct trusted-proxy configurations are a
// deployment property, so single digits is already generous.
const maxProxySetCacheEntries = 8

// cachedProxySet returns the parsed set for trustedProxies, parsing it at most once per distinct
// configuration. An empty or nil list skips the cache — the map round trip would cost more than
// the loop it replaces.
func cachedProxySet(trustedProxies []string) proxySet {
	if len(trustedProxies) == 0 {
		return proxySet{}
	}
	key := strings.Join(trustedProxies, "\x00")
	proxySetCache.Lock()
	defer proxySetCache.Unlock()
	if s, ok := proxySetCache.sets[key]; ok {
		return s
	}
	s := parseProxySet(trustedProxies)
	if len(proxySetCache.sets) < maxProxySetCacheEntries {
		proxySetCache.sets[key] = s
	}
	return s
}
func (s proxySet) empty() bool { return len(s.ips) == 0 && len(s.cidrs) == 0 }

// trusts reports whether remoteIP (a bare address, no port) is in the set,
// matching either an exact IP or any CIDR.
func (s proxySet) trusts(remoteIP string) bool {
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	for _, t := range s.ips {
		if t.Equal(ip) {
			return true
		}
	}
	for _, c := range s.cidrs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is ClientIP against the pre-parsed set.
func (s proxySet) clientIP(r *http.Request) string {
	remoteIP := r.RemoteAddr
	// SplitHostPort handles both "1.2.3.4:5678" and "[::1]:5678"; fall back to
	// the raw value for malformed inputs.
	if host, _, err := net.SplitHostPort(remoteIP); err == nil {
		remoteIP = host
	}
	if s.empty() || !s.trusts(remoteIP) {
		return remoteIP
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remoteIP
	}
	// Rightmost = the address the trusted proxy appended; the leftmost is
	// client-forgeable and would let a client rotate it to dodge the limiter.
	return rightmostForwardedFor(xff)
}

// rightmostForwardedFor returns the last entry of X-Forwarded-For — the address the trusted proxy
// directly observed and appended. The LEFTMOST is client-supplied and trivially forgeable (trusting
// it lets a client rotate the value to dodge per-IP limits and spoof audit-log source IPs); the
// rightmost cannot be forged. Callers MUST only use this after confirming the peer is a trusted
// proxy. Assumes a single trusted hop (the deployment model).
func rightmostForwardedFor(xff string) string {
	if idx := strings.LastIndexByte(xff, ','); idx >= 0 {
		return strings.TrimSpace(xff[idx+1:])
	}
	return strings.TrimSpace(xff)
}
