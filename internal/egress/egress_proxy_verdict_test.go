package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// L-misc-5: the validation-time lookup honours the caller's context — a
// resolver that never answers returns at the deadline, not at its own
// timeout, so a handler (and its pooled DB connection) is not held.
func TestRejectPrivateHostContext_HonoursDeadline(t *testing.T) {
	stuck := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := RejectPrivateHostContext(ctx, stuck, "never-answers.test", BlockInternal)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a lookup that never answers must fail")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("returned after %s, want at the 50 ms deadline", elapsed)
	}
	var dnsErr *net.DNSError
	if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &dnsErr) || !dnsErr.IsTimeout) {
		t.Errorf("error = %v, want the deadline", err)
	}

	t.Run("literal IPs never touch the resolver", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := RejectPrivateHostContext(ctx, stuck, "203.0.113.10", BlockInternal); err != nil {
			t.Errorf("public literal: %v", err)
		}
		if err := RejectPrivateHostContext(ctx, stuck, "10.0.0.1", BlockInternal); err == nil {
			t.Error("private literal must be refused")
		}
		if err := RejectPrivateHostContext(ctx, stuck, "", BlockInternal); err == nil {
			t.Error("empty host must be refused")
		}
		if err := RejectPrivateHostContext(ctx, stuck, "localhost", AllowLoopback); err != nil {
			t.Errorf("loopback escape hatch: %v", err)
		}
	})
	t.Run("nil resolver is the default", func(t *testing.T) {
		if err := RejectPrivateHostContext(context.Background(), nil, "127.0.0.1", BlockInternal); err == nil {
			t.Error("loopback literal must be refused")
		}
	})
}

// PERF-14: net/http calls the proxy hook on every request; a target's pass is
// reused for a minute, so one lookup serves a whole chat turn's calls. A
// refusal is never cached.
func TestGuardAllowListedTransport_ProxyVerdictCached(t *testing.T) {
	proxyURL, proxied, _ := countingProxy(t)
	dns := &fakeDNS{answers: []net.IP{net.ParseIP("203.0.113.10")}}
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	GuardAllowListedTransport(tr, &net.Dialer{Timeout: 2 * time.Second, Resolver: dns.resolver()}, BlockInternal)
	c := &http.Client{Transport: tr, CheckRedirect: NoRedirects, Timeout: 5 * time.Second}

	queries := func() int {
		dns.mu.Lock()
		defer dns.mu.Unlock()
		return dns.queries
	}
	if err := getOK(c, "http://cached.test/one"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	after1 := queries()
	if after1 == 0 {
		t.Fatal("the first request must resolve the target")
	}
	for i := 0; i < 3; i++ {
		if err := getOK(c, "http://cached.test/again"); err != nil {
			t.Fatalf("request %d: %v", i+2, err)
		}
	}
	if q := queries(); q != after1 {
		t.Errorf("%d lookups after 4 requests, want the first request's %d (the pass is cached)", q, after1)
	}
	if n := proxied(); n != 4 {
		t.Errorf("the proxy saw %d requests, want 4", n)
	}

	t.Run("a refusal is re-judged every time", func(t *testing.T) {
		private := &fakeDNS{answers: []net.IP{net.ParseIP("10.1.2.3")}}
		tr := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		GuardAllowListedTransport(tr, &net.Dialer{Timeout: 2 * time.Second, Resolver: private.resolver()}, BlockInternal)
		c := &http.Client{Transport: tr, CheckRedirect: NoRedirects, Timeout: 5 * time.Second}
		before := proxied()
		for i := 0; i < 2; i++ {
			if err := getOK(c, "http://privatelink.test/"); !errors.Is(err, ErrInternalAddress) {
				t.Errorf("want an internal-address refusal, got %v", err)
			}
		}
		private.mu.Lock()
		q := private.queries
		private.mu.Unlock()
		if q < 2 {
			t.Errorf("%d lookups for two refused requests, want one per request", q)
		}
		if proxied() != before {
			t.Error("a refused target must never reach the proxy")
		}
	})
}

// PERF-14: the verdict cache is bounded.
func TestProxyVerdicts_Bounded(t *testing.T) {
	v := newProxyVerdicts()
	for i := 0; i < proxyVerdictMax+5; i++ {
		v.pass("host-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)))
	}
	if n := len(v.until); n > proxyVerdictMax {
		t.Errorf("cache holds %d entries, want at most %d", n, proxyVerdictMax)
	}
	v.pass("fresh.test")
	if !v.passed("fresh.test") || v.passed("unknown.test") {
		t.Error("pass/passed disagree")
	}
}
