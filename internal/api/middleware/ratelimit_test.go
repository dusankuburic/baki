package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
		{"trusted IP picks rightmost (proxy-appended) XFF entry", "10.0.0.1:1234", "1.2.3.4, 5.6.7.8", "5.6.7.8"},
		{"spoofed leftmost XFF is ignored", "10.0.0.1:1234", "6.6.6.6, 5.6.7.8", "5.6.7.8"},
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

// TestRateLimiter_GetIP_SpoofedXFFCannotRotateBucket is the F1 regression: a
// client behind a trusted proxy that rotates the leftmost X-Forwarded-For value
// must still map to a single bucket key (the proxy-appended rightmost IP), so it
// can't dodge the per-IP limiter by forging the header.
func TestRateLimiter_GetIP_SpoofedXFFCannotRotateBucket(t *testing.T) {
	rl := NewRateLimiter(60, 20, []string{"10.0.0.1"})
	newReq := func(xff string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:1234" // trusted proxy peer
		req.Header.Set("X-Forwarded-For", xff)
		return req
	}
	// Real client 203.0.113.7 rotates the forged leftmost entry each request.
	first := rl.key(rl.getIP(newReq("1.1.1.1, 203.0.113.7")))
	second := rl.key(rl.getIP(newReq("2.2.2.2, 203.0.113.7")))
	if first != second {
		t.Fatalf("spoofed leftmost XFF produced different bucket keys (%q vs %q) — limiter bypassable", first, second)
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
	if len(rl.proxies.ips) != 0 {
		t.Errorf("expected 0 trusted IPs, got %d", len(rl.proxies.ips))
	}
	if len(rl.proxies.cidrs) != 0 {
		t.Errorf("expected 0 trusted CIDRs, got %d", len(rl.proxies.cidrs))
	}
}

// TestRateLimiter_IPv6ClientsShareTheirSlash64: an IPv6 client is limited per
// /64, not per address. One host routinely holds a whole /64, so per-address
// buckets let it rotate its source address for a fresh burst each time — the
// limit did not apply at all, and every address was a new bucket.
func TestRateLimiter_IPv6ClientsShareTheirSlash64(t *testing.T) {
	rl := NewRateLimiter(0, 2, nil) // burst 2, no refill during the test
	defer rl.Stop()
	h := rl.Limit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	status := func(remoteAddr string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, addr := range []string{"[2001:db8:1:2::a]:1234", "[2001:db8:1:2::b]:1234"} {
		if got := status(addr); got != http.StatusOK {
			t.Fatalf("%s: status %d, want 200 (within the burst)", addr, got)
		}
	}
	// A third address in the same /64 draws on the same, now empty, bucket.
	if got := status("[2001:db8:1:2:ffff:ffff:ffff:ffff]:1234"); got != http.StatusTooManyRequests {
		t.Errorf("rotated address in the same /64: status %d, want 429", got)
	}
	// The neighbouring /64 is another client.
	if got := status("[2001:db8:1:3::a]:1234"); got != http.StatusOK {
		t.Errorf("address in another /64: status %d, want 200", got)
	}
}

// TestRateLimitAddr pins the bucket-key address for each kind of client
// value: IPv6 by /64; IPv4 unchanged; IPv4-mapped IPv6 as the IPv4 address
// it is (its /64 would be every IPv4 client at once); anything else verbatim.
func TestRateLimitAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"2001:db8:1:2::", "2001:db8:1:2::/64"},
		{"fe80::1%eth0", "fe80::/64"},
		{"::1", "::/64"},
		{"203.0.113.7", "203.0.113.7"},
		{"::ffff:203.0.113.7", "203.0.113.7"},
		{"::ffff:198.51.100.1", "198.51.100.1"},
		// A forwarded ip:port (a proxy that appends the peer's port) is keyed
		// by its address: verbatim, every new source port was a new bucket.
		{"203.0.113.7:4444", "203.0.113.7"},
		{"[2001:db8:1:2::9]:443", "2001:db8:1:2::/64"},
		// An IPv4 client as a translator presents it is keyed as that IPv4
		// client: by its /64, every client behind the translator shared one
		// bucket. RFC 6052 well-known NAT64 prefix, RFC 6145 translated form,
		// and RFC 8215's local-use range, whose embedding is operator-chosen,
		// by the whole address.
		{"64:ff9b::203.0.113.7", "203.0.113.7"},
		{"64:ff9b::198.51.100.1", "198.51.100.1"},
		{"::ffff:0:203.0.113.7", "203.0.113.7"},
		{"64:ff9b:1:2::cb00:7107", "64:ff9b:1:2::cb00:7107"},
		{"64:ff9b:0:0:1::", "64:ff9b::/64"}, // outside the /96: an ordinary address
		{"not-an-ip", "not-an-ip"},
		{"", ""},
	}
	for _, c := range cases {
		if got := RateLimitAddr(c.in); got != c.want {
			t.Errorf("RateLimitAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	rl := NewRateLimiter(60, 20, nil)
	defer rl.Stop()
	if got, want := rl.key("::ffff:203.0.113.7"), rl.key("203.0.113.7"); got != want {
		t.Errorf("mapped IPv4 key %q != IPv4 key %q; one client, two buckets", got, want)
	}
	if got := rl.key("203.0.113.7"); got != "ratelimit:general:203.0.113.7" {
		t.Errorf("IPv4 key = %q; IPv4 keys are unchanged", got)
	}
	if rl.key("::ffff:203.0.113.7") == rl.key("::ffff:198.51.100.1") {
		t.Error("two IPv4-mapped clients share a bucket; mapped addresses must not be grouped by /64")
	}
	if rl.key("64:ff9b::203.0.113.7") == rl.key("64:ff9b::198.51.100.1") {
		t.Error("two IPv4 clients behind a NAT64/SIIT translator share a bucket")
	}
	if rl.key("64:ff9b:1::a") == rl.key("64:ff9b:1::b") {
		t.Error("two clients in the local-use NAT64 range share a bucket")
	}
}

// TestInMemoryStore_CapsTheBucketMap: the in-memory map never grows past its
// cap. Buckets otherwise leave only after 5 minutes idle, so a client
// minting keys grows it for as long as it keeps minting.
func TestInMemoryStore_CapsTheBucketMap(t *testing.T) {
	s := newInMemoryStore()
	defer s.Stop()
	s.maxBuckets = 4
	ctx := context.Background()
	// rate 0: a used bucket never refills, so no sweep can make room.
	for i := range 100 {
		s.Allow(ctx, fmt.Sprintf("ratelimit:general:k%d", i), 0, 5)
	}
	if n := len(s.buckets); n > s.maxBuckets {
		t.Fatalf("bucket map holds %d buckets; cap is %d", n, s.maxBuckets)
	}
}

// TestInMemoryStore_FullMapLocksOutNoClient: at a full map, one shared overflow bucket for new keys would
// put every refilled legitimate client's bucket into the attacker-controlled sweep — a global login and
// token-refresh lockout. The full-map sweep must free every refilled bucket instead.
func TestInMemoryStore_FullMapLocksOutNoClient(t *testing.T) {
	s := newInMemoryStore()
	defer s.Stop()
	s.maxBuckets = 50
	ctx := context.Background()
	const rate, capacity = 5.0 / 60, 5.0 // the auth limiter

	// A legitimate client refreshed its token 30 s ago: its bucket has
	// refilled since.
	s.Allow(ctx, "legit", rate, capacity)
	s.buckets["legit"].lastFill = time.Now().Add(-30 * time.Second)

	// An attacker fills the map with minted keys, one request each, and
	// keeps minting.
	for i := 0; len(s.buckets) < s.maxBuckets; i++ {
		s.Allow(ctx, fmt.Sprintf("minted-%d", i), rate, capacity)
	}
	for i := range 20 {
		s.Allow(ctx, fmt.Sprintf("minted-more-%d", i), rate, capacity)
	}

	if !s.Allow(ctx, "legit", rate, capacity) {
		t.Error("the legitimate client's next request was refused while the map was held full")
	}
	if !s.Allow(ctx, "brand-new", rate, capacity) {
		t.Error("a new legitimate client was refused while the map was held full")
	}
	if n := len(s.buckets); n > s.maxBuckets {
		t.Errorf("bucket map holds %d buckets; cap is %d", n, s.maxBuckets)
	}
}

// TestInMemoryStore_FullMapSweepsRefilledBuckets: at the cap, a sweep drops
// the buckets that have refilled (they admit exactly what a fresh bucket
// would), making room without evicting a bucket still in use — and the sweep
// runs at most once per fullSweepInterval, since it walks the whole map under
// the lock; in between, a new key evicts a single bucket.
func TestInMemoryStore_FullMapSweepsRefilledBuckets(t *testing.T) {
	s := newInMemoryStore()
	defer s.Stop()
	s.maxBuckets = 2
	ctx := context.Background()
	const rate, capacity = 10.0, 1.0 // an empty bucket refills in 100 ms
	s.Allow(ctx, "old-1", rate, capacity)
	s.Allow(ctx, "old-2", rate, capacity)
	// Both were emptied a second ago: refilled, but far from bucketIdleTTL.
	for _, b := range s.buckets {
		b.lastFill = time.Now().Add(-time.Second)
	}

	// A sweep just ran: this one is throttled, so the key evicts one bucket
	// — a sweep would have dropped both refilled ones.
	s.lastSweep = time.Now()
	if !s.Allow(ctx, "throttled", rate, capacity) {
		t.Fatal("new key refused at a full map")
	}
	_, old1 := s.buckets["old-1"]
	_, old2 := s.buckets["old-2"]
	if _, ok := s.buckets["throttled"]; !ok || len(s.buckets) != 2 || old1 == old2 {
		t.Fatalf("inside fullSweepInterval a new key should evict exactly one bucket; map now %v", s.buckets)
	}

	s.lastSweep = time.Now().Add(-fullSweepInterval)
	if !s.Allow(ctx, "fresh", rate, capacity) {
		t.Fatal("new key refused after the sweep")
	}
	if _, ok := s.buckets["fresh"]; !ok {
		t.Fatal("new key did not get a bucket of its own")
	}
	for _, k := range []string{"old-1", "old-2"} {
		if _, ok := s.buckets[k]; ok {
			t.Errorf("refilled bucket %s survived the sweep", k)
		}
	}
}
