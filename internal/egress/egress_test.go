package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// No test in this file depends on DNS or reaches the network: hosts are
// literal IPs, "localhost" (the hosts file), names answered by the in-process
// fakeDNS, or public documentation addresses (203.0.113.0/24) whose dial is
// refused by a Control hook before connect(), or only ever sent to a local
// proxy.

func TestIsInternalIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"::1", true},
		{"fe80::1", true},
		{"fc00::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"8.8.8.8", false},
		{"2606:4700::1111", false},
		// Special-purpose ranges beyond the net.IP predicates.
		{"168.63.129.16", true}, // Azure wireserver / DNS
		{"168.63.129.17", false},
		{"100.64.0.1", true}, // CGNAT
		{"100.127.255.254", true},
		{"100.128.0.1", false},
		{"0.1.2.3", true}, // "this network"
		{"192.0.0.8", true},
		{"192.0.1.1", false},
		{"198.18.0.1", true}, // benchmarking
		{"198.19.255.255", true},
		{"198.20.0.1", false},
		// IPv4-mapped, NAT64 and 6to4 forms are judged by the embedded IPv4.
		{"::ffff:169.254.169.254", true},
		{"64:ff9b::a9fe:a9fe", true}, // NAT64 169.254.169.254
		{"64:ff9b::7f00:1", true},    // NAT64 127.0.0.1
		{"64:ff9b::a00:1", true},     // NAT64 10.0.0.1
		{"64:ff9b::808:808", false},  // NAT64 8.8.8.8
		{"2002:a9fe:a9fe::1", true},  // 6to4 169.254.169.254
		{"2002:7f00:1::", true},      // 6to4 127.0.0.1
		{"2002:a83f:8110::1", true},  // 6to4 168.63.129.16
		{"2002:808:808::1", false},   // 6to4 8.8.8.8
	}
	for _, c := range cases {
		t.Run(c.ip, func(t *testing.T) {
			ip := net.ParseIP(c.ip)
			if ip == nil {
				t.Fatalf("test bug: %q does not parse", c.ip)
			}
			if got := IsInternalIP(ip); got != c.want {
				t.Errorf("IsInternalIP(%s) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"localhost":   true,
		"127.0.0.1":   true,
		"127.1.2.3":   true,
		"::1":         true,
		"example.com": false,
		"10.0.0.1":    false, // private but not loopback: the hatch is loopback only
		"":            false,
	}
	for host, want := range cases {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestRejectPrivateHost(t *testing.T) {
	t.Run("private literal refused under BlockInternal", func(t *testing.T) {
		err := RejectPrivateHost("10.0.0.5", BlockInternal)
		if err == nil {
			t.Fatal("expected an error for a private literal IP")
		}
		if !strings.Contains(err.Error(), "internal targets are not allowed") {
			t.Errorf("unexpected error text: %v", err)
		}
		if strings.Contains(err.Error(), "alert URL") {
			t.Errorf("egress must not use notify's wording: %v", err)
		}
	})
	t.Run("loopback literal refused under BlockInternal", func(t *testing.T) {
		for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
			if err := RejectPrivateHost(host, BlockInternal); err == nil {
				t.Errorf("RejectPrivateHost(%q, BlockInternal) = nil, want error", host)
			}
		}
	})
	t.Run("loopback passes under AllowLoopback", func(t *testing.T) {
		for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
			if err := RejectPrivateHost(host, AllowLoopback); err != nil {
				t.Errorf("RejectPrivateHost(%q, AllowLoopback) = %v, want nil", host, err)
			}
		}
	})
	t.Run("hatch is loopback only", func(t *testing.T) {
		for _, host := range []string{"169.254.169.254", "10.0.0.5", "192.168.1.10", "0.0.0.0"} {
			if err := RejectPrivateHost(host, AllowLoopback); err == nil {
				t.Errorf("RejectPrivateHost(%q, AllowLoopback) = nil, want error", host)
			}
		}
	})
	t.Run("empty host is an error", func(t *testing.T) {
		for _, policy := range []Policy{BlockInternal, AllowLoopback} {
			if err := RejectPrivateHost("", policy); err == nil {
				t.Errorf("RejectPrivateHost(\"\", %v) = nil, want error", policy)
			}
		}
	})
	t.Run("public literal passes", func(t *testing.T) {
		for _, host := range []string{"8.8.8.8", "2606:4700::1111"} {
			if err := RejectPrivateHost(host, BlockInternal); err != nil {
				t.Errorf("RejectPrivateHost(%q, BlockInternal) = %v, want nil", host, err)
			}
		}
	})
}

// countingListener is a live TCP listener on 127.0.0.1 that counts the
// connections it accepted, so a test can prove the guard refused a dial
// BEFORE connect() rather than after reaching the target.
func countingListener(t *testing.T) (port string, accepted func() int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, func() int32 {
		time.Sleep(20 * time.Millisecond) // let a stray accept be counted
		return n.Load()
	}
}

// fakeDNS is an in-process DNS server for a PreferGo net.Resolver: every A
// query is answered with the next address of answers (the last one repeats),
// AAAA queries with no records. It never touches the network.
type fakeDNS struct {
	mu      sync.Mutex
	answers []net.IP
	queries int
}

func (f *fakeDNS) resolver() *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: f.dial}
}

func (f *fakeDNS) nextA() net.IP {
	f.mu.Lock()
	defer f.mu.Unlock()
	ip := f.answers[min(f.queries, len(f.answers)-1)]
	f.queries++
	return ip.To4()
}

// dial hands the resolver one end of a pipe. A net.Pipe is not a
// PacketConn, so the resolver frames its queries TCP-style (2-byte length).
func (f *fakeDNS) dial(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		for {
			var l [2]byte
			if _, err := io.ReadFull(server, l[:]); err != nil {
				return
			}
			q := make([]byte, binary.BigEndian.Uint16(l[:]))
			if _, err := io.ReadFull(server, q); err != nil {
				return
			}
			i := 12 // header, then the question name's labels
			for q[i] != 0 {
				i += int(q[i]) + 1
			}
			qtype := binary.BigEndian.Uint16(q[i+1:])
			resp := append([]byte{}, q[:i+5]...) // header + the question
			resp[2], resp[3] = 0x81, 0x80        // response, recursion available, NOERROR
			binary.BigEndian.PutUint16(resp[6:], 0)
			binary.BigEndian.PutUint16(resp[8:], 0)
			binary.BigEndian.PutUint16(resp[10:], 0)
			if qtype == 1 { // A
				binary.BigEndian.PutUint16(resp[6:], 1)
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4) // name ptr, A, IN, TTL 0, len 4
				resp = append(resp, f.nextA()...)
			}
			out := binary.BigEndian.AppendUint16(nil, uint16(len(resp)))
			if _, err := server.Write(append(out, resp...)); err != nil {
				return
			}
		}
	}()
	return client, nil
}

func TestGuardDialContext(t *testing.T) {
	ctx := context.Background()
	port, accepted := countingListener(t)
	d := &net.Dialer{Timeout: 2 * time.Second}

	t.Run("BlockInternal refuses a loopback literal before connecting", func(t *testing.T) {
		conn, err := GuardDialContext(d, BlockInternal)(ctx, "tcp", "127.0.0.1:"+port)
		if conn != nil {
			conn.Close()
			t.Error("expected a nil conn")
		}
		if !errors.Is(err, ErrInternalAddress) || !strings.Contains(err.Error(), "internal address") {
			t.Errorf("expected an internal-address refusal, got %v", err)
		}
		if n := accepted(); n != 0 {
			t.Errorf("the listener accepted %d connections, want 0", n)
		}
	})
	t.Run("BlockInternal refuses a name the dialer itself resolves to loopback", func(t *testing.T) {
		_, err := GuardDialContext(d, BlockInternal)(ctx, "tcp", "localhost:"+port)
		if !errors.Is(err, ErrInternalAddress) {
			t.Errorf("expected an internal-address refusal, got %v", err)
		}
		if n := accepted(); n != 0 {
			t.Errorf("the listener accepted %d connections, want 0", n)
		}
	})
	t.Run("an empty host is refused (it means this machine)", func(t *testing.T) {
		_, err := GuardDialContext(d, BlockInternal)(ctx, "tcp", ":"+port)
		if !errors.Is(err, ErrInternalAddress) {
			t.Errorf("expected an internal-address refusal, got %v", err)
		}
		if n := accepted(); n != 0 {
			t.Errorf("the listener accepted %d connections, want 0", n)
		}
	})
	t.Run("an address without a port fails without connecting", func(t *testing.T) {
		if _, err := GuardDialContext(d, BlockInternal)(ctx, "tcp", "192.168.1.10"); err == nil {
			t.Error("expected an error")
		}
	})
	t.Run("AllowLoopback still refuses private and link-local", func(t *testing.T) {
		for _, addr := range []string{"10.0.0.5:443", "169.254.169.254:80"} {
			_, err := GuardDialContext(d, AllowLoopback)(ctx, "tcp", addr)
			if !errors.Is(err, ErrInternalAddress) {
				t.Errorf("%s: expected an internal-address refusal, got %v", addr, err)
			}
		}
	})
	t.Run("AllowLoopback lets loopback hosts through", func(t *testing.T) {
		for _, host := range []string{"127.0.0.1", "localhost"} {
			conn, err := GuardDialContext(d, AllowLoopback)(ctx, "tcp", net.JoinHostPort(host, port))
			if err != nil {
				t.Errorf("%s: %v", host, err)
				continue
			}
			conn.Close()
		}
		if n := accepted(); n != 2 {
			t.Errorf("the listener accepted %d connections, want 2", n)
		}
		// [::1] is not listened on here: whatever fails, it is not the guard.
		if _, err := GuardDialContext(d, AllowLoopback)(ctx, "tcp", "[::1]:"+port); errors.Is(err, ErrInternalAddress) {
			t.Errorf("[::1] must bypass the guard under AllowLoopback: %v", err)
		}
	})
	t.Run("the caller's dialer is left untouched and its hook still runs", func(t *testing.T) {
		// The dialer's own hook refuses everything, so no dial below ever
		// reaches connect() (and the network).
		var seen atomic.Int32
		own := &net.Dialer{Timeout: 2 * time.Second, Control: func(string, string, syscall.RawConn) error {
			seen.Add(1)
			return errors.New("own hook")
		}}
		dial := GuardDialContext(own, BlockInternal)
		if _, err := dial(ctx, "tcp", "203.0.113.10:443"); err == nil || !strings.Contains(err.Error(), "own hook") || seen.Load() != 1 {
			t.Errorf("an allowed address must reach the dialer's own hook: err=%v calls=%d", err, seen.Load())
		}
		if _, err := dial(ctx, "tcp", "10.0.0.5:443"); !errors.Is(err, ErrInternalAddress) || seen.Load() != 1 {
			t.Errorf("the guard runs first: err=%v calls=%d", err, seen.Load())
		}
		if own.ControlContext != nil {
			t.Error("GuardDialContext must not modify the caller's dialer")
		}
	})
}

// Finding #41: the dial is judged on the address actually connected to, so a
// record that answers a public address to the validation-time lookup and an
// internal one to the dialer (DNS rebinding, TTL 0) is refused.
func TestGuardDialContext_DNSRebinding(t *testing.T) {
	ctx := context.Background()
	port, accepted := countingListener(t)
	dns := &fakeDNS{answers: []net.IP{net.ParseIP("203.0.113.10"), net.ParseIP("127.0.0.1")}}
	d := &net.Dialer{Timeout: 2 * time.Second, Resolver: dns.resolver()}

	addrs, err := d.Resolver.LookupIPAddr(ctx, "rebind.test")
	if err != nil || len(addrs) != 1 || IsInternalIP(addrs[0].IP) {
		t.Fatalf("the first answer must look public: %v %v", addrs, err)
	}
	_, err = GuardDialContext(d, BlockInternal)(ctx, "tcp", "rebind.test:"+port)
	if !errors.Is(err, ErrInternalAddress) || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the rebound address must be refused at connect time, got %v", err)
	}
	if n := accepted(); n != 0 {
		t.Errorf("the listener accepted %d connections, want 0", n)
	}
}

func TestCheckDialAddress(t *testing.T) {
	for addr, ok := range map[string]bool{
		"8.8.8.8:443":           true,
		"[2606:4700::1111]:443": true,
		"127.0.0.1:80":          false,
		"[::1]:80":              false,
		"169.254.169.254:80":    false,
		"[fe80::1%eth0]:443":    false, // zoned: not parseable as an IP
		"example.com:443":       false, // the hook only ever sees IPs
		"no-port":               false,
	} {
		if err := checkDialAddress(addr, BlockInternal); (err == nil) != ok {
			t.Errorf("checkDialAddress(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

// countingProxy is a forward proxy on loopback — standing in for a corporate
// egress proxy on a private address — that answers every request itself and
// counts it (a CONNECT included), so a test can prove a refused request never
// reached the proxy.
func countingProxy(t *testing.T) (proxyURL *url.URL, proxied func() int32, lastTarget func() string) {
	t.Helper()
	var n atomic.Int32
	var last atomic.Value
	last.Store("")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		last.Store(r.URL.String())
		_, _ = io.WriteString(w, "via proxy")
	}))
	t.Cleanup(proxy.Close)
	u, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u, n.Load, func() string { return last.Load().(string) }
}

// getOK GETs target and turns a non-200 answer into an error.
func getOK(c *http.Client, target string) error {
	resp, err := c.Get(target)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

// noDNS is a resolver whose every lookup fails without touching the network:
// a name that "does not resolve locally".
var noDNS = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("no DNS in unit tests")
}}

// Fix round for finding #13 (regression): GuardTransport — the guard notify's
// GuardedTransport installs on tenant webhook targets — must not trust a
// proxy. Behind a proxy on an internal address every request fails closed,
// whatever the target, because the proxy's own resolution of the name is
// beyond this process. Behind a proxy the dial guard allows, the target is
// judged strictly.
func TestGuardTransport_ProxyFailsClosed(t *testing.T) {
	proxyURL, proxied, lastTarget := countingProxy(t)
	client := func(r *net.Resolver, policy Policy) (*http.Client, *http.Transport) {
		tr := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		GuardTransport(tr, &net.Dialer{Timeout: 2 * time.Second, Resolver: r}, policy)
		return &http.Client{Transport: tr, CheckRedirect: NoRedirects, Timeout: 5 * time.Second}, tr
	}

	t.Run("BlockInternal: a proxy on an internal address is refused for every target", func(t *testing.T) {
		public := (&fakeDNS{answers: []net.IP{net.ParseIP("203.0.113.10")}}).resolver()
		c, _ := client(public, BlockInternal)
		for _, target := range []string{
			"http://203.0.113.10/latest/meta-data", // public literal: passes the hook, not the proxy hop
			"http://resolves-public.test/hook",     // public name: likewise
		} {
			err := getOK(c, target)
			if !errors.Is(err, ErrInternalAddress) || !strings.Contains(err.Error(), "proxyconnect") {
				t.Errorf("%s: want the proxy hop refused, got %v", target, err)
			}
		}
		if n := proxied(); n != 0 {
			t.Errorf("the private proxy received %d requests, want 0", n)
		}
	})
	t.Run("BlockInternal: a target that does not resolve locally never reaches the proxy", func(t *testing.T) {
		c, _ := client(noDNS, BlockInternal)
		err := getOK(c, "http://internal-only.invalid/latest/meta-data")
		if !errors.Is(err, ErrUnverifiedTarget) {
			t.Errorf("want an unverifiable-target refusal, got %v", err)
		}
		if n := proxied(); n != 0 {
			t.Errorf("the proxy received %d requests, want 0", n)
		}
	})
	t.Run("BlockInternal: the proxy's address is never exempted from the dial guard", func(t *testing.T) {
		_, tr := client(noDNS, BlockInternal)
		_ = getOK(&http.Client{Transport: tr, Timeout: 5 * time.Second}, "http://203.0.113.10/")
		if _, err := tr.DialContext(context.Background(), "tcp", proxyURL.Host); !errors.Is(err, ErrInternalAddress) {
			t.Errorf("dialling the proxy must stay guarded, got %v", err)
		}
		if n := proxied(); n != 0 {
			t.Errorf("the proxy received %d requests, want 0", n)
		}
	})

	// Under AllowLoopback the dial guard lets the loopback proxy through, so it
	// stands in for a proxy on a public address: the strict target check is
	// what decides.
	t.Run("a public target reaches a proxy the dial guard allows", func(t *testing.T) {
		c, _ := client(noDNS, AllowLoopback)
		before := proxied()
		if err := getOK(c, "http://203.0.113.10/hook"); err != nil {
			t.Fatalf("request through the proxy: %v", err)
		}
		if proxied() != before+1 || lastTarget() != "http://203.0.113.10/hook" {
			t.Errorf("the proxy did not receive the request: %d %q", proxied()-before, lastTarget())
		}
	})
	t.Run("a target that does not resolve locally is refused, not left to the proxy", func(t *testing.T) {
		c, _ := client(noDNS, AllowLoopback)
		before := proxied()
		if err := getOK(c, "http://only-the-proxy-knows.test/x"); !errors.Is(err, ErrUnverifiedTarget) {
			t.Errorf("want an unverifiable-target refusal, got %v", err)
		}
		if proxied() != before {
			t.Error("a refused target must never reach the proxy")
		}
	})
	t.Run("internal targets are refused before the proxy", func(t *testing.T) {
		before := proxied()
		c, _ := client(noDNS, AllowLoopback)
		for _, target := range []string{"http://10.0.0.1/", "http://169.254.169.254/latest/meta-data/", "http://[fe80::1%25eth0]/"} {
			if err := getOK(c, target); !errors.Is(err, ErrInternalAddress) {
				t.Errorf("%s: want an internal-address refusal, got %v", target, err)
			}
		}
		c, _ = client((&fakeDNS{answers: []net.IP{net.ParseIP("10.1.2.3")}}).resolver(), AllowLoopback)
		if err := getOK(c, "http://resolves-private.test/"); !errors.Is(err, ErrInternalAddress) {
			t.Errorf("a name resolving to an internal address: want a refusal, got %v", err)
		}
		if proxied() != before {
			t.Error("a refused target must never reach the proxy")
		}
	})
	t.Run("direct connections keep the dial guard", func(t *testing.T) {
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("a guarded direct request reached the loopback server")
		}))
		t.Cleanup(target.Close)
		tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
		GuardTransport(tr, &net.Dialer{Timeout: 2 * time.Second}, BlockInternal)
		if err := getOK(&http.Client{Transport: tr, Timeout: 5 * time.Second}, target.URL); !errors.Is(err, ErrInternalAddress) {
			t.Errorf("want an internal-address refusal, got %v", err)
		}
	})
}

// Finding #13: for allow-listed targets (the Azure client) the target is
// judged in the Proxy hook and the hop to the proxy itself (here on loopback,
// like a corporate egress proxy on a private address) is allowed; direct
// connections keep the dial guard.
func TestGuardAllowListedTransport_Proxy(t *testing.T) {
	proxyURL, proxied, lastTarget := countingProxy(t)
	client := func(r *net.Resolver) (*http.Client, *http.Transport) {
		tr := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		GuardAllowListedTransport(tr, &net.Dialer{Timeout: 2 * time.Second, Resolver: r}, BlockInternal)
		return &http.Client{Transport: tr, CheckRedirect: NoRedirects, Timeout: 5 * time.Second}, tr
	}

	t.Run("a public target reaches the private proxy", func(t *testing.T) {
		c, _ := client(noDNS)
		before := proxied()
		if err := getOK(c, "http://203.0.113.10/openai/v1/chat/completions"); err != nil {
			t.Fatalf("request through the proxy: %v", err)
		}
		if proxied() != before+1 || lastTarget() != "http://203.0.113.10/openai/v1/chat/completions" {
			t.Errorf("the proxy did not receive the request: %d %v", proxied()-before, lastTarget())
		}
	})
	t.Run("a target that does not resolve locally is left to the proxy", func(t *testing.T) {
		c, _ := client(noDNS)
		before := proxied()
		if err := getOK(c, "http://only-the-proxy-knows.test/x"); err != nil {
			t.Fatalf("request through the proxy: %v", err)
		}
		if proxied() != before+1 {
			t.Error("the proxy did not receive the request")
		}
	})
	t.Run("an internal literal target is refused before the proxy", func(t *testing.T) {
		c, _ := client(noDNS)
		before := proxied()
		for _, target := range []string{"http://10.0.0.1/", "http://169.254.169.254/latest/meta-data/"} {
			if err := getOK(c, target); !errors.Is(err, ErrInternalAddress) {
				t.Errorf("%s: want an internal-address refusal, got %v", target, err)
			}
		}
		if proxied() != before {
			t.Error("a refused target must never reach the proxy")
		}
	})
	t.Run("a target name resolving to an internal address is refused", func(t *testing.T) {
		c, _ := client((&fakeDNS{answers: []net.IP{net.ParseIP("10.1.2.3")}}).resolver())
		before := proxied()
		if err := getOK(c, "http://privatelink.test/"); !errors.Is(err, ErrInternalAddress) {
			t.Errorf("want an internal-address refusal, got %v", err)
		}
		if proxied() != before {
			t.Error("a refused target must never reach the proxy")
		}
	})
	t.Run("only the selected proxy's address skips the dial guard", func(t *testing.T) {
		c, tr := client(noDNS)
		if err := getOK(c, "http://203.0.113.10/"); err != nil {
			t.Fatal(err)
		}
		port, accepted := countingListener(t)
		if _, err := tr.DialContext(context.Background(), "tcp", "127.0.0.1:"+port); !errors.Is(err, ErrInternalAddress) {
			t.Errorf("a direct loopback dial must stay guarded, got %v", err)
		}
		if n := accepted(); n != 0 {
			t.Errorf("the listener accepted %d connections, want 0", n)
		}
	})
	t.Run("direct connections keep the dial guard", func(t *testing.T) {
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("a guarded direct request reached the loopback server")
		}))
		t.Cleanup(target.Close)
		tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
		GuardAllowListedTransport(tr, &net.Dialer{Timeout: 2 * time.Second}, BlockInternal)
		c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
		if err := getOK(c, target.URL); !errors.Is(err, ErrInternalAddress) {
			t.Errorf("want an internal-address refusal, got %v", err)
		}
	})
}

func TestProxyDialAddr(t *testing.T) {
	for raw, want := range map[string]string{
		"http://10.0.0.4:3128":   "10.0.0.4:3128",
		"http://proxy.corp":      "proxy.corp:80",
		"https://proxy.corp":     "proxy.corp:443",
		"socks5://127.0.0.1":     "127.0.0.1:1080",
		"socks5h://[fd00::1]":    "[fd00::1]:1080",
		"http://[fd00::1]:8080/": "[fd00::1]:8080",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyDialAddr(u); got != want {
			t.Errorf("proxyDialAddr(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestGuardedTransport(t *testing.T) {
	tr := GuardedTransport(BlockInternal)
	if tr == nil || tr == http.DefaultTransport {
		t.Fatal("expected a fresh clone of http.DefaultTransport")
	}
	if tr.DialContext == nil {
		t.Fatal("expected a wrapped DialContext")
	}
	_, err := tr.DialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil || !strings.Contains(err.Error(), "internal address") {
		t.Errorf("expected the dial guard on the transport, got %v", err)
	}
}

func TestNoRedirects(t *testing.T) {
	if err := NoRedirects(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("NoRedirects() = %v, want http.ErrUseLastResponse", err)
	}
}
