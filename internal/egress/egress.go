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
//   - GuardDialContext / GuardedTransport is the dial-time check: the
//     validation-time resolution alone is vulnerable to DNS rebinding between
//     validation and request (or to the record simply changing later), so
//     every connection re-resolves and refuses internal addresses right
//     before the socket is opened.
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
	"fmt"
	"net"
	"net/http"
)

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

// RejectPrivateHost is the validation-time half of the guard: it resolves host
// (net.LookupIP short-circuits literal IPs, so those never touch DNS) and
// refuses it when ANY resolved address is internal per IsInternalIP. Under
// AllowLoopback a loopback host passes without resolution — the desktop/local
// escape hatch, opted into explicitly. An empty host is an error.
//
// The error text is deliberately generic ("host %q ...") so a caller can
// prefix it with what the host is (notify says "alert URL host ..."); wrap it
// with %w to keep the chain.
func RejectPrivateHost(host string, policy Policy) error {
	if host == "" {
		return fmt.Errorf("host is empty")
	}
	if policy == AllowLoopback && IsLoopbackHost(host) {
		return nil // desktop/local escape hatch, opted into explicitly
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("host %q does not resolve: %w", host, err)
	}
	for _, ip := range ips {
		if IsInternalIP(ip) {
			return fmt.Errorf("host %q resolves to a private/loopback address (%s) — internal targets are not allowed", host, ip)
		}
	}
	return nil
}

// GuardDialContext wraps base so that every connection re-resolves the target
// host and refuses internal addresses at DIAL time (B1.9): a validation-time
// resolution check alone is vulnerable to DNS rebinding between validation and
// request. The material SSRF surfaces (cloud metadata 169.254.0.0/16, RFC1918
// services, loopback listeners) are therefore blocked on both layers.
//
// The ONLY bypass, and it is explicit: under AllowLoopback a loopback host
// (see IsLoopbackHost) goes straight to base — the documented dev escape hatch
// (http://localhost) and the test harness. The previous notify code skipped
// the whole check whenever the host was literally "localhost", and its IP test
// omitted IsLoopback — so "127.0.0.1" was not skipped but passed anyway. Both
// holes are closed: under BlockInternal nothing is skipped, and IsInternalIP
// covers loopback.
//
// A resolution failure is not a verdict: it falls through to base, whose own
// error path reports it.
func GuardDialContext(base func(ctx context.Context, network, addr string) (net.Conn, error), policy Policy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if policy == AllowLoopback && IsLoopbackHost(host) {
			return base(ctx, network, addr)
		}
		if ips, lerr := net.LookupIP(host); lerr == nil {
			for _, ip := range ips {
				if IsInternalIP(ip) {
					return nil, fmt.Errorf("egress: refusing connection to internal address %s", ip)
				}
			}
		}
		return base(ctx, network, addr)
	}
}

// GuardedTransport returns a clone of http.DefaultTransport whose DialContext
// is wrapped by GuardDialContext. Pair it with NoRedirects on the http.Client
// so a redirect cannot steer the request around the guard.
func GuardedTransport(policy Policy) *http.Transport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = GuardDialContext(base.DialContext, policy)
	return base
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
