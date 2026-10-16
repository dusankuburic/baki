package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
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

// resetProxySetCache gives each cache test a cold cache: the memo is package
// state shared with every other test in the package (the ClientIP tests above
// warm it through the same path).
func resetProxySetCache(t *testing.T) {
	t.Helper()
	proxySetCache.Lock()
	proxySetCache.sets = make(map[string]proxySet)
	proxySetCache.Unlock()
}

// TestCachedProxySet_ParsesOncePerConfiguration: ClientIP gets the raw config
// slice per call, so the cache must key on the slice's CONTENTS — two distinct
// slices with the same entries share one parsed set (one parse), and the
// returned sets literally share their backing arrays.
func TestCachedProxySet_ParsesOncePerConfiguration(t *testing.T) {
	resetProxySetCache(t)
	a := cachedProxySet([]string{"10.0.0.0/8", "192.168.0.1"})
	b := cachedProxySet([]string{"10.0.0.0/8", "192.168.0.1"}) // same entries, distinct slice
	if len(a.cidrs) != 1 || len(b.cidrs) != 1 || len(a.ips) != 1 || len(b.ips) != 1 {
		t.Fatalf("parsed %d CIDRs / %d IPs and %d / %d, want 1/1 each", len(a.cidrs), len(a.ips), len(b.cidrs), len(b.ips))
	}
	if &a.ips[0] != &b.ips[0] || &a.cidrs[0] != &b.cidrs[0] {
		t.Error("equal configurations must share one cached set instead of re-parsing per call")
	}
	proxySetCache.Lock()
	entries := len(proxySetCache.sets)
	proxySetCache.Unlock()
	if entries != 1 {
		t.Errorf("cache entries = %d, want 1 (keyed by contents, not by slice)", entries)
	}
}

// TestCachedProxySet_EmptyListSkipsCache: nil/empty has nothing to parse, so
// it must not occupy cache entries — the map round trip would cost more than
// the loop it skips — yet still behave as "trust nobody".
func TestCachedProxySet_EmptyListSkipsCache(t *testing.T) {
	resetProxySetCache(t)
	if s := cachedProxySet(nil); !s.empty() {
		t.Error("nil trusted proxies must parse to an empty set")
	}
	if s := cachedProxySet([]string{}); !s.empty() {
		t.Error("empty trusted proxies must parse to an empty set")
	}
	proxySetCache.Lock()
	entries := len(proxySetCache.sets)
	proxySetCache.Unlock()
	if entries != 0 {
		t.Errorf("cache entries = %d, want 0 for empty inputs", entries)
	}
}

// TestCachedProxySet_Bounded: the cache must never grow with caller-supplied
// inputs. Past the cap a configuration still parses (correctly), it just is
// not stored.
func TestCachedProxySet_Bounded(t *testing.T) {
	resetProxySetCache(t)
	for i := 0; i < maxProxySetCacheEntries+3; i++ {
		s := cachedProxySet([]string{"10.0.0." + strconv.Itoa(i) + "/32"})
		if len(s.cidrs) != 1 {
			t.Fatalf("entry %d: parsed %d CIDRs, want 1 (overflow entries must still parse)", i, len(s.cidrs))
		}
	}
	proxySetCache.Lock()
	entries := len(proxySetCache.sets)
	proxySetCache.Unlock()
	if entries != maxProxySetCacheEntries {
		t.Errorf("cache entries = %d, want the cap %d", entries, maxProxySetCacheEntries)
	}
}

// TestPeerIsTrusted gates every forwarded-header decision (X-Forwarded-For,
// X-Forwarded-Proto): only a configured proxy's word counts.
func TestPeerIsTrusted(t *testing.T) {
	trusted := []string{"10.0.0.0/8"}

	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "10.0.0.1:443"
	if !PeerIsTrusted(direct, trusted) {
		t.Error("peer inside a trusted CIDR should be trusted")
	}

	foreign := httptest.NewRequest(http.MethodGet, "/", nil)
	foreign.RemoteAddr = "203.0.113.5:12345"
	if PeerIsTrusted(foreign, trusted) {
		t.Error("outside peer must not be trusted, whatever headers it sends")
	}

	loopback := httptest.NewRequest(http.MethodGet, "/", nil)
	loopback.RemoteAddr = "127.0.0.1:9999"
	if PeerIsTrusted(loopback, trusted) {
		t.Error("loopback is not implicitly trusted (desktop serves HTTP locally)")
	}

	noList := httptest.NewRequest(http.MethodGet, "/", nil)
	noList.RemoteAddr = "10.0.0.1:443"
	if PeerIsTrusted(noList, nil) {
		t.Error("empty trusted-proxy list must trust nobody")
	}
}
