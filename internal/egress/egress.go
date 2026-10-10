// Package egress is the single definition of "an address the outside world
// must not make this server reach": URLs accepted from tenants/users (governance
// alert channels, AI provider endpoints) are SSRF surfaces — cloud metadata,
// RFC1918 services, the server's own loopback (B1.9).
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrInternalAddress is the refusal every layer of the guard returns (wrapped
// with the offending address): a connection the policy does not allow.
var ErrInternalAddress = errors.New("egress: refusing connection to internal address")

// ErrUnverifiedTarget is GuardTransport's refusal of a proxied request whose
// target host does not resolve locally: the proxy would resolve the name to an
// address this process never got to judge.
var ErrUnverifiedTarget = errors.New("egress: refusing proxied request to an unverifiable target")

// Policy decides whether an outbound target may point at the machine (or the
// private network) the server itself runs on: in a multi-tenant deployment ANY
// authenticated user can create an org, add a webhook channel and hit the
// synchronous test endpoint — a loopback port scanner plus a POST of
// attacker-influenced JSON at internal listeners.
type Policy int

const (
	// BlockInternal refuses loopback, private, link-local and unspecified
	// addresses. The ZERO VALUE on purpose: a caller that forgets a policy
	// gets the safe one.
	BlockInternal Policy = iota
	// AllowLoopback additionally permits loopback targets (a literal loopback
	// IP or the name "localhost"). DESKTOP AND LOCAL MODE ONLY — there the
	// "org" is the single user running the app; nothing else is relaxed.
	AllowLoopback
	// AllowPrivateNetwork additionally permits RFC 1918 (10/8, 172.16/12,
	// 192.168/16) and IPv6 unique-local (fc00::/7) — the private-link AI
	// endpoint case (PAD_AI_ALLOW_PRIVATE_EGRESS). An RFC 1918 address
	// embedded in a NAT64 / 6to4 address does not count.
	AllowPrivateNetwork
)

// Refuses reports whether the policy refuses ip: every address IsInternalIP
// names, except — under AllowPrivateNetwork — RFC 1918 and unique-local ones.
// AllowLoopback's loopback allowance is by host name (IsLoopbackHost): a name
// that merely resolves to loopback is refused under every policy.
func (p Policy) Refuses(ip net.IP) bool {
	if !IsInternalIP(ip) {
		return false
	}
	return p != AllowPrivateNetwork || !ip.IsPrivate()
}

// internalPrefixes are the special-purpose ranges IsInternalIP refuses on top
// of the net.IP predicates.
var internalPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),        // "this network" (RFC 1122); 0.x reaches loopback on some stacks
	netip.MustParsePrefix("100.64.0.0/10"),    // carrier-grade NAT shared space (RFC 6598), cloud-internal on several providers
	netip.MustParsePrefix("168.63.129.16/32"), // Azure platform: wireserver, DNS, health probes — reachable from every Azure VM
	netip.MustParsePrefix("192.0.0.0/24"),     // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("198.18.0.0/15"),    // benchmarking (RFC 2544), used for internal plumbing
}

// Prefixes of IPv6 addresses that carry an IPv4 address the network translates
// to: NAT64 (RFC 6052) connects to the last 32 bits, 6to4 (RFC 3056) to bits
// 16–47. IsInternalIP judges the embedded address.
var (
	nat64Prefix   = netip.MustParsePrefix("64:ff9b::/96")
	sixToFourPref = netip.MustParsePrefix("2002::/16")
)

// IsInternalIP is the single definition of "an address the outside world should
// not be able to make this server reach": loopback, link-local unicast and
// multicast (169.254.0.0/16 is where cloud metadata lives), RFC 1918 and IPv6
// unique-local, unspecified, plus internalPrefixes. A NAT64/6to4 address is
// judged by the IPv4 it embeds, an IPv4-mapped IPv6 by its IPv4 form. Both
// guard layers use it, so they cannot drift.
func IsInternalIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified() {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if v4, ok := embeddedIPv4(addr); ok {
		return IsInternalIP(net.IP(v4.AsSlice()))
	}
	for _, p := range internalPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// embeddedIPv4 extracts the IPv4 address a NAT64 or 6to4 address carries.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	switch {
	case nat64Prefix.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixToFourPref.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

// IsLoopbackHost reports whether host names the local machine, by literal IP
// or by the conventional name "localhost". Used only to honour AllowLoopback.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// RejectPrivateHost is RejectPrivateHostContext with no deadline and the
// default resolver (boot-time configuration, tests); request paths should pass
// their context so the lookup ends with the request.
func RejectPrivateHost(host string, policy Policy) error {
	return RejectPrivateHostContext(context.Background(), net.DefaultResolver, host, policy)
}

// RejectPrivateHostContext is the validation-time half of the guard: it resolves
// host with r (LookupIPAddr short-circuits literal IPs, so those never touch
// DNS) and refuses it when the policy refuses ANY resolved address. Under
// AllowLoopback a loopback host passes without resolution; an empty host is an
// error; nil r is the default resolver. The lookup honours ctx — the system
// resolver can block ~10 s per unresolvable name on Windows/cgo, and a handler
// holding a pooled DB connection that long is a pool exhaustion vector. The
// error text is deliberately generic; prefix it, wrap with %w.
func RejectPrivateHostContext(ctx context.Context, r *net.Resolver, host string, policy Policy) error {
	if host == "" {
		return fmt.Errorf("host is empty")
	}
	if policy == AllowLoopback && IsLoopbackHost(host) {
		return nil // desktop/local escape hatch, opted into explicitly
	}
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("host %q does not resolve: %w", host, err)
	}
	for _, a := range addrs {
		if policy.Refuses(a.IP) {
			return fmt.Errorf("host %q resolves to a private/loopback address (%s) — internal targets are not allowed", host, a.IP)
		}
	}
	return nil
}

// GuardDialContext returns a DialContext that dials with d and enforces the
// policy at DIAL time: the check runs in the dialer's ControlContext hook,
// after the dialer's own resolution and right before connect(), for each
// candidate address it tries (Happy Eyeballs fallbacks included) — a
// validation-time check alone is vulnerable to DNS rebinding, so the policy
// must run on the address actually dialled. An unparseable address is refused;
// d is copied, never modified (its own Control(ControlContext) hook still
// runs). The ONLY bypass, and it is explicit: under AllowLoopback a loopback
// host (IsLoopbackHost) is dialled with d as is — the dev/test hatch; any other
// name that resolves to loopback is refused under both policies.
func GuardDialContext(d *net.Dialer, policy Policy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	guarded := *d
	prevCtx, prev := d.ControlContext, d.Control
	guarded.Control = nil
	guarded.ControlContext = func(ctx context.Context, network, address string, c syscall.RawConn) error {
		if err := checkDialAddress(address, policy); err != nil {
			return err
		}
		switch {
		case prevCtx != nil:
			return prevCtx(ctx, network, address, c)
		case prev != nil:
			return prev(network, address, c)
		}
		return nil
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if policy == AllowLoopback && IsLoopbackHost(host) {
			return d.DialContext(ctx, network, addr) // explicit dev/test hatch
		}
		return guarded.DialContext(ctx, network, addr)
	}
}

// checkDialAddress is the connect-time verdict on the "ip:port" the dialer is
// about to connect to.
func checkDialAddress(address string, policy Policy) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("egress: refusing connection to unparseable address %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("egress: refusing connection to unparseable address %q", address)
	}
	if policy.Refuses(ip) {
		return fmt.Errorf("%w %s", ErrInternalAddress, ip)
	}
	return nil
}

// newGuardDialer is the dialer GuardedTransport uses: http.DefaultTransport's
// own settings.
func newGuardDialer() *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
}

// GuardTransport installs the dial-time guard on t (modified in place) with d
// as the dialer, for targets a tenant or user controls (the notify webhooks):
// EVERY connection t makes — the hop to a proxy t.Proxy selects (HTTP_PROXY /
// HTTPS_PROXY) included — goes through GuardDialContext and is judged on its
// concrete IP. Behind a proxy the socket reaches the proxy, which resolves the
// target itself: this process can neither see nor pin that address, so a
// proxied request fails closed — a proxy on an internal address is refused by
// the dial guard, and behind a public proxy the TARGET is also judged in the
// Proxy hook, strictly (it must resolve locally and hold no internal address;
// checkProxiedTarget). Deployments whose egress proxy sits on a private address
// must list the tenant target hosts in NO_PROXY so they are dialled directly;
// operator allow-listed targets can instead trust the proxy hop
// (GuardAllowListedTransport). t.DialTLSContext is cleared so every connection
// goes through the guarded DialContext.
func GuardTransport(t *http.Transport, d *net.Dialer, policy Policy) {
	t.DialTLSContext = nil
	t.DialContext = GuardDialContext(d, policy)
	if t.Proxy == nil {
		return
	}
	selectProxy := t.Proxy
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		u, err := selectProxy(req)
		if err != nil || u == nil {
			return u, err // direct connection: the dial guard judges the target
		}
		if err := checkProxiedTarget(req.Context(), d.Resolver, req.URL.Hostname(), policy, true); err != nil {
			return nil, err
		}
		return u, nil // the hop to the proxy itself is judged by the dial guard
	}
}

// GuardAllowListedTransport is GuardTransport for a transport whose targets are
// restricted to an operator allow-list of hosts whose DNS no tenant or user
// controls — the Azure AI Foundry client (*.openai.azure.com and friends, plus
// the operator's own additions). Direct connections are judged identically;
// the difference is the proxy: here the operator-configured proxy is TRUSTED —
// judging its socket would judge the PROXY, and every request behind a proxy on
// a private address (the normal corporate setup) was refused. For a proxied
// request the TARGET host is judged in the Proxy hook (internal addresses
// refused; a local resolution failure is not a verdict, the proxy resolves the
// name), and the hop to exactly the host:port the hook selected goes through d
// unguarded. Never use it for tenant-supplied hosts (there the local lookup
// and the proxy's own can differ — use GuardTransport). The Proxy hook runs on
// every request; a PASS is reused for proxyVerdictTTL per target host, a
// refusal is never cached.
func GuardAllowListedTransport(t *http.Transport, d *net.Dialer, policy Policy) {
	guarded := GuardDialContext(d, policy)
	t.DialTLSContext = nil
	if t.Proxy == nil {
		t.DialContext = guarded
		return
	}
	var proxies sync.Map // canonical host:port of every proxy the hook selected
	verdicts := newProxyVerdicts()
	selectProxy := t.Proxy
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		u, err := selectProxy(req)
		if err != nil || u == nil {
			return u, err // direct connection: the dial guard judges the target
		}
		host := req.URL.Hostname()
		if !verdicts.passed(host) {
			lctx, cancel := context.WithTimeout(req.Context(), proxyLookupTimeout)
			err := checkProxiedTarget(lctx, d.Resolver, host, policy, false)
			cancel()
			if err != nil {
				return nil, err
			}
			verdicts.pass(host)
		}
		proxies.Store(proxyDialAddr(u), struct{}{})
		return u, nil
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, ok := proxies.Load(addr); ok {
			return d.DialContext(ctx, network, addr) // the operator-configured proxy hop
		}
		return guarded(ctx, network, addr)
	}
}

// proxyVerdictTTL is how long GuardAllowListedTransport's proxy hook reuses a
// target host's pass. Only passes are cached; refusals are re-judged.
const proxyVerdictTTL = time.Minute

// proxyLookupTimeout bounds the local lookup in that hook: a resolver that
// cannot answer must not hold each request for its own timeout.
const proxyLookupTimeout = 2 * time.Second

// proxyVerdictMax bounds the verdict cache; when full it is cleared (the
// hosts are operator-allow-listed, a handful in practice).
const proxyVerdictMax = 256

// proxyVerdicts is the bounded per-transport cache of proxied-target passes.
type proxyVerdicts struct {
	mu    sync.Mutex
	until map[string]time.Time // host -> the pass is valid until
}

func newProxyVerdicts() *proxyVerdicts {
	return &proxyVerdicts{until: make(map[string]time.Time)}
}

func (v *proxyVerdicts) passed(host string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	until, ok := v.until[host]
	return ok && time.Now().Before(until)
}

func (v *proxyVerdicts) pass(host string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.until) >= proxyVerdictMax {
		clear(v.until)
	}
	v.until[host] = time.Now().Add(proxyVerdictTTL)
}

// checkProxiedTarget judges the target of a proxied request: its local
// resolution must not contain an internal address (a literal IP is judged
// without DNS). A name that does not resolve locally is refused when strict
// (GuardTransport: what cannot be checked is not allowed) and passes otherwise
// (the allow-listed transport: the proxy resolves it).
func checkProxiedTarget(ctx context.Context, r *net.Resolver, host string, policy Policy, strict bool) error {
	if host == "" {
		return errors.New("egress: request has no target host")
	}
	if policy == AllowLoopback && IsLoopbackHost(host) {
		return nil
	}
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupIPAddr(ctx, host)
	if err == nil && len(addrs) == 0 {
		err = errors.New("no addresses")
	}
	if err != nil {
		if strict {
			return fmt.Errorf("%w: target %q does not resolve locally, so its address cannot be checked before the proxy connects to it: %w", ErrUnverifiedTarget, host, err)
		}
		return nil // a local resolution failure is not a verdict: the proxy resolves the name
	}
	for _, a := range addrs {
		if policy.Refuses(a.IP) {
			return fmt.Errorf("%w %s (target %q, via proxy)", ErrInternalAddress, a.IP, host)
		}
	}
	return nil
}

// proxyDialAddr is the address net/http dials for proxy u (its canonicalAddr):
// the explicit port, else the scheme's default.
func proxyDialAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// GuardedTransport returns a clone of http.DefaultTransport guarded by
// GuardTransport with the default transport's dialer settings. Pair it with
// NoRedirects on the http.Client so a redirect cannot steer the request around
// the guard.
func GuardedTransport(policy Policy) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	GuardTransport(t, newGuardDialer(), policy)
	return t
}

// transportFailureMarkers recognise a connection failure in an error whose
// chain was flattened to text (azidentity reports a failed token request as a
// string): the net/net/http phrasings, Windows' (connectex, wsarecv), and this package's own refusals.
var transportFailureMarkers = []string{
	"dial tcp", "dial udp", "proxyconnect", "no such host", "connection refused",
	"connection reset", "i/o timeout", "network is unreachable", "host is unreachable",
	"actively refused", "forcibly closed", "did not properly respond",
	"tls: ", "x509: ", "egress: refusing",
}

// DescribeFailure maps a failed outbound connection to fixed, client-safe copy
// ("the host name does not resolve"): the raw text names addresses — the
// target's resolved IP, the operator's HTTP(S) proxy — that must stay in the
// server log, never in a response. ok is false when err is not a connection
// failure. Recognised: this package's refusals, *url.Error / *net.OpError /
// *net.DNSError anywhere in the chain, and the same failures flattened to text.
func DescribeFailure(err error) (msg string, ok bool) {
	if err == nil {
		return "", false
	}
	text := err.Error()
	switch {
	case errors.Is(err, ErrInternalAddress) || strings.Contains(text, ErrInternalAddress.Error()):
		return "the host resolves to an internal network address (private, loopback or link-local), which this server does not connect to", true
	case errors.Is(err, ErrUnverifiedTarget) || strings.Contains(text, ErrUnverifiedTarget.Error()):
		return "the host could not be verified before the server's outbound proxy connected to it", true
	}
	var (
		urlErr *url.Error
		opErr  *net.OpError
		dnsErr *net.DNSError
	)
	typed := errors.As(err, &urlErr) || errors.As(err, &opErr) || errors.As(err, &dnsErr)
	lower := strings.ToLower(text)
	if !typed {
		flattened := false
		for _, m := range transportFailureMarkers {
			if strings.Contains(lower, m) {
				flattened = true
				break
			}
		}
		if !flattened {
			return "", false
		}
	}
	var netErr net.Error
	switch {
	case dnsErr != nil || strings.Contains(lower, "no such host"):
		return "the host name does not resolve", true
	case strings.Contains(lower, "proxyconnect"):
		return "the connection through the server's outbound proxy failed", true
	case strings.Contains(lower, "x509: ") || strings.Contains(lower, "tls: ") || strings.Contains(lower, "certificate"):
		return "the TLS handshake failed (an untrusted certificate or an intercepting proxy)", true
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) ||
		strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "did not properly respond"):
		return "the connection timed out", true
	// Windows words a refused connection "No connection could be made because the target machine actively refused
	// it" and a reset one "An existing connection was forcibly closed by the remote host".
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "actively refused"):
		return "the connection was refused", true
	case strings.Contains(lower, "connection reset") || strings.Contains(lower, "forcibly closed") || strings.Contains(lower, "eof"):
		return "the connection was closed unexpectedly", true
	case strings.Contains(lower, "unreachable"):
		return "the network is unreachable", true
	}
	return "the connection failed", true
}

// NoRedirects is an http.Client CheckRedirect that refuses to follow 3xx:
// only the CONFIGURED url passed validation, and 307/308 replay the method AND
// body — a passing https endpoint must not be able to answer "307 ->
// http://127.0.0.1:2379/" and have the payload delivered there. Returning the
// 3xx as the final response is safer and more debuggable.
func NoRedirects(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
