// Package egress is the single definition of "an address the outside world
// must not make this server reach".
//
// Several features accept a URL from a tenant or user and then make the server
// connect to it — governance alert channels (webhook / Teams / Slack / Jira in
// package notify) and user-supplied AI provider endpoints (Azure). Each of
// those is an SSRF surface: in a multi-tenant deployment any authenticated
// user can point such a URL at cloud-internal endpoints
// (https://169.254.169.254/... metadata, private RFC1918 services, the
// server's own loopback listeners), use the synchronous response as a
// reachability oracle, or POST attacker-influenced bodies at internal
// listeners (B1.9).
//
// The guard has two layers, and both lean on the same IsInternalIP so they
// cannot drift:
//
//   - RejectPrivateHost is the validation-time check: resolve the configured
//     host once and refuse it when any address is internal. Run it when the
//     URL is saved so the operator gets a clear error instead of a silently
//     dead channel.
//   - GuardDialContext / GuardTransport / GuardedTransport is the dial-time
//     check: the validation-time resolution alone is vulnerable to DNS
//     rebinding between validation and request (or to the record simply
//     changing later), so the dialer's own resolution is judged — the policy
//     runs on the concrete IP of every connection attempt, right before
//     connect(). There is no second lookup whose answer could differ from the
//     address actually dialled.
//   - Behind an operator-configured HTTP(S) proxy the socket reaches the
//     proxy, not the target, and the proxy resolves the target name itself.
//     For tenant-controlled targets (GuardTransport / GuardedTransport) the
//     hop to the proxy stays under the dial guard, so a proxy on an internal
//     address fails closed, and the target is judged strictly in the Proxy
//     hook. Only transports whose targets are restricted to an operator
//     allow-list (GuardAllowListedTransport) trust the proxy hop.
//
// NoRedirects completes the picture for HTTP clients: only the CONFIGURED URL
// ever passed validation, and 307/308 replay the method AND body, so a
// policy-passing https endpoint must not be able to answer
// "307 -> http://127.0.0.1:2379/..." and have the payload delivered there.
//
// Policy is the only knob. BlockInternal — the zero value — is what every
// multi-tenant (cloud) deployment must use; AllowLoopback exists for the
// desktop/local mode where the "tenant" is the single user running the app on
// their own machine and a target on localhost is exactly what they meant. The
// loopback allowance is loopback only: link-local and RFC1918 stay refused
// under both policies.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"
)

// ErrInternalAddress is the refusal every layer of the guard returns (wrapped
// with the offending address): a connection the policy does not allow.
var ErrInternalAddress = errors.New("egress: refusing connection to internal address")

// ErrUnverifiedTarget is GuardTransport's refusal of a proxied request whose
// target host does not resolve locally: the proxy would resolve the name on
// its own, to an address this process never got to judge.
var ErrUnverifiedTarget = errors.New("egress: refusing proxied request to an unverifiable target")

// Policy decides whether an outbound target may point at the machine (or the
// private network) the server itself runs on.
//
// It exists because the notify package's "http://localhost for dev" escape
// hatch was originally unconditional. In a multi-tenant deployment ANY
// authenticated user can create an org and become its admin (handleOrgCreate
// takes no role), add a webhook channel, and hit the synchronous test endpoint
// — whose response distinguishes connection-refused from non-2xx from 2xx.
// That is a loopback port scanner plus the ability to POST
// attacker-influenced JSON at internal listeners.
type Policy int

const (
	// BlockInternal refuses loopback, private, link-local and unspecified
	// addresses. (URL validators layered on top, such as notify, additionally
	// refuse plaintext http:// entirely under this policy.)
	//
	// This is the ZERO VALUE on purpose: a caller that forgets to pass a policy
	// gets the safe one. Opting into the permissive behaviour has to be typed
	// out, which is also why the tests that need a loopback httptest server say
	// so explicitly.
	BlockInternal Policy = iota
	// AllowLoopback additionally permits loopback targets (a literal loopback
	// IP or the name "localhost"). DESKTOP AND LOCAL MODE ONLY — there the
	// "org" is the single user running the app, so a target pointing at their
	// own machine is exactly what they meant. Nothing else is relaxed:
	// link-local and private ranges stay refused.
	AllowLoopback
)

// IsInternalIP is the single definition of "an address the outside world
// should not be able to make this server reach": loopback, link-local (unicast
// and multicast — 169.254.0.0/16 is where cloud metadata lives), private
// (RFC 1918 and IPv6 unique-local) and unspecified (0.0.0.0 / ::, which some
// stacks route to loopback).
//
// Both layers (validation-time RejectPrivateHost and dial-time
// GuardDialContext) use it, so they cannot drift — previously the dial guard
// omitted IsLoopback while the resolver check included it, which is precisely
// how a literal http://127.0.0.1 passed both.
func IsInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified()
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
// default resolver (boot-time configuration, tests). Request paths should
// pass their context so the lookup ends with the request.
func RejectPrivateHost(host string, policy Policy) error {
	return RejectPrivateHostContext(context.Background(), net.DefaultResolver, host, policy)
}

// RejectPrivateHostContext is the validation-time half of the guard: it
// resolves host with r (LookupIPAddr short-circuits literal IPs, so those
// never touch DNS) and refuses it when ANY resolved address is internal per
// IsInternalIP. Under AllowLoopback a loopback host passes without
// resolution — the desktop/local escape hatch, opted into explicitly. An
// empty host is an error. The lookup honours ctx (the request's deadline,
// the client's disconnect): on Windows/cgo the system resolver can block
// ~10 s per unresolvable name, and a handler holding a pooled DB connection
// for that long is a pool exhaustion vector. A nil r is the default
// resolver.
//
// The error text is deliberately generic ("host %q ...") so a caller can
// prefix it with what the host is (notify says "alert URL host ..."); wrap it
// with %w to keep the chain.
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
		if IsInternalIP(a.IP) {
			return fmt.Errorf("host %q resolves to a private/loopback address (%s) — internal targets are not allowed", host, a.IP)
		}
	}
	return nil
}

// GuardDialContext returns a DialContext that dials with d and enforces the
// policy at DIAL time (B1.9) on the concrete address of every connection
// attempt: the check runs in the dialer's ControlContext hook, after the
// dialer's own resolution and right before connect(), for each candidate
// address it tries (Happy Eyeballs fallbacks included). A validation-time
// resolution check alone is vulnerable to DNS rebinding between validation and
// request, and so was the previous form of this guard, which resolved the
// name once to judge it and then let the dialer resolve it AGAIN to connect —
// a TTL-0 record alternating between a public and an internal address passed
// the first lookup and was connected to on the second. The material SSRF
// surfaces (cloud metadata 169.254.0.0/16, RFC1918 services, loopback
// listeners) are therefore blocked on both layers.
//
// An address the hook cannot parse as an IP (a zoned IPv6 literal, say) is
// refused; a name that does not resolve fails in the dialer itself, so nothing
// falls through unchecked. d is copied, never modified; a Control or
// ControlContext hook it already carries still runs after the check.
//
// The ONLY bypass, and it is explicit: under AllowLoopback a loopback host
// (see IsLoopbackHost) is dialled with d as is — the documented dev escape
// hatch (http://localhost) and the test harness. Any other name that resolves
// to a loopback address is still refused under both policies. The previous
// notify code skipped the whole check whenever the host was literally
// "localhost", and its IP test omitted IsLoopback — so "127.0.0.1" was not
// skipped but passed anyway. Both holes stay closed: under BlockInternal
// nothing is skipped, and IsInternalIP covers loopback.
func GuardDialContext(d *net.Dialer, policy Policy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	guarded := *d
	prevCtx, prev := d.ControlContext, d.Control
	guarded.Control = nil
	guarded.ControlContext = func(ctx context.Context, network, address string, c syscall.RawConn) error {
		if err := checkDialAddress(address); err != nil {
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
func checkDialAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("egress: refusing connection to unparseable address %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("egress: refusing connection to unparseable address %q", address)
	}
	if IsInternalIP(ip) {
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
// as the dialer. It is the guard for targets a tenant or user controls (the
// notify webhooks): EVERY connection t makes — the hop to a proxy that t.Proxy
// selects (HTTP_PROXY / HTTPS_PROXY) included — goes through GuardDialContext
// and is judged on its concrete IP.
//
// Behind a proxy the socket reaches the proxy, not the target, and the proxy
// resolves the target name again on its own: this process can neither see nor
// pin the address the proxy connects to, so it cannot close the DNS-rebinding
// window there (a name that resolves to a public address here and to an
// internal one for the proxy, or does not resolve here at all). A proxied
// request therefore fails closed:
//
//   - a proxy on an internal address — the usual corporate egress proxy or
//     sidecar, which can reach this server's private network — is refused by
//     the dial guard, so no request is routed through it;
//   - behind a proxy on a public address the TARGET is judged in the Proxy hook
//     as well, strictly: it must resolve locally (with d's resolver) and no
//     address may be internal under the same policy. A local resolution
//     failure is a refusal — RejectPrivateHost demands a resolving host at
//     validation time too. What remains is the gap between that lookup and the
//     proxy's own, and a proxy outside the private network cannot turn it into
//     a probe of this one.
//
// Deployments whose egress goes through a proxy on a private address must
// list the tenant target hosts in NO_PROXY, so they are dialled directly and
// judged on the address actually connected to. Transports whose targets are
// restricted to an operator allow-list can trust the proxy hop instead: see
// GuardAllowListedTransport.
//
// t.DialTLSContext is cleared so every connection goes through the guarded
// DialContext.
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
// the operator's own additions). Connections dialled directly are judged by
// GuardDialContext on the concrete IP, exactly as in GuardTransport.
//
// The difference is the proxy: here the operator-configured proxy is trusted.
// Judging its socket would judge the PROXY — every request behind a proxy on a
// private address, the normal corporate setup, was refused, while behind a
// public proxy the real target went unchecked. So for a request that t.Proxy
// routes through a proxy the TARGET host is judged in the Proxy hook (resolved
// with d's resolver; internal addresses are refused under the same policy; a
// local resolution failure is not a verdict, since the proxy resolves the
// name itself and may see names this host cannot), and the hop to exactly the
// host:port the hook selected goes through d unguarded.
//
// Never use it for tenant-supplied hosts: there the local lookup and the
// proxy's own can differ (rebinding, or a name that does not resolve here),
// and a private proxy then connects wherever the name points. Use
// GuardTransport.
//
// net/http calls the Proxy hook on every request, pooled connection or not,
// so behind a proxy every chat call, tool iteration, embedding and listing
// page would do its own lookup bounded only by the request's context (the
// chat context lasts minutes). The lookup is bounded by proxyLookupTimeout
// and a PASS is reused for proxyVerdictTTL per target host; a refusal is
// never cached. The hosts are operator-allow-listed and the proxy resolves
// them itself anyway.
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

// proxyVerdictTTL is how long GuardAllowListedTransport's proxy hook reuses
// a target host's pass (its local resolution held no internal address).
// Only passes are cached; a refusal is re-judged on the next request.
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
// resolution must not contain an internal address. A literal IP is judged
// without DNS. A name that does not resolve locally is refused when strict
// (GuardTransport: an address that cannot be checked is not allowed) and
// passes otherwise (GuardAllowListedTransport: the proxy resolves it).
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
		if IsInternalIP(a.IP) {
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
// GuardTransport (with the default transport's dialer settings): the
// tenant-target guard, under which a request routed through a proxy on an
// internal address fails closed. Pair it with NoRedirects on the http.Client
// so a redirect cannot steer the request around the guard.
func GuardedTransport(policy Policy) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	GuardTransport(t, newGuardDialer(), policy)
	return t
}

// NoRedirects is an http.Client CheckRedirect that refuses to follow 3xx.
//
// Only the CONFIGURED url passes through a caller's validation; a redirect
// target never did, and 307/308 replay the method AND body — so a perfectly
// legitimate https://attacker.example/hook could answer
// "307 -> http://127.0.0.1:2379/..." and have a signed payload delivered there
// instead. An endpoint that redirects is already misconfigured, so returning
// the 3xx as the final response (where it fails the caller's 2xx check with a
// clear status) is both safer and more debuggable than following it.
func NoRedirects(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
