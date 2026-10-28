package ai

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"pad-analyzer/internal/egress"
)

// maxConnsPerHostFromEnv reads PAD_AI_MAX_CONNS_PER_HOST: idle pooling was already capped, but ACTIVE
// connections were unbounded — N users × 3 concurrent streams each hold a connection for up to the 10-minute
// stream cap, and a small deploy could exhaust its fd budget. Default 256; 0 disables the cap.
func maxConnsPerHostFromEnv() int {
	if v := os.Getenv("PAD_AI_MAX_CONNS_PER_HOST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 256
}

// sharedHTTPClient is the tuned base client every provider transport derives from: the factory builds a
// provider per request, and per-instance clients meant TCP/TLS connections were never reused across requests.
// MaxIdleConnsPerHost is raised from 2 because a busy instance talks to few hosts with high concurrency.
// Timeout is deliberately 0: http.Client.Timeout includes reading the body, so any value here would
// silently kill long SSE responses — per-request cancellation is enforced via the context (chat.go's
// 10-min cap + 90s idle watchdog). Fixed-host providers go through fixedHostClient(), the guarded view of
// this client; localModelsClient and the unconfigured Azure client use this one directly and must be able
// to reach loopback.
var sharedHTTPClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// The connect and the TLS handshake are bounded on the base, so every clone inherits them: Go dials
		// through a zero net.Dialer (no timeout) without a DialContext, and arms no handshake timer without
		// TLSHandshakeTimeout, so a gateway that accepts TCP and stalls the handshake held a call until the
		// caller's own deadline. The egress guards replace DialContext with a dialer of the same timeout.
		DialContext:         (&net.Dialer{Timeout: providerDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: providerTLSHandshakeTimeout,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     maxConnsPerHostFromEnv(),
		IdleConnTimeout:     90 * time.Second,
		// B1.8: a header-only floor — a call site that forgets a per-request context gets bounded headers
		// without killing long SSE bodies the way Client.Timeout would.
		ResponseHeaderTimeout: streamingHeaderTimeout,
		ForceAttemptHTTP2:     true,
	},
}

// The provider transports' connection bounds (R-provider-4) and header floors (R-provider-5).
const (
	providerDialTimeout         = 30 * time.Second
	providerTLSHandshakeTimeout = 15 * time.Second
	// streamingHeaderTimeout is the header floor of a streaming request: the upstream answers the headers as
	// soon as it starts generating.
	streamingHeaderTimeout = 30 * time.Second
	// longWaitHeaderTimeout is the header floor of a non-streaming chat request (a stream-refusing deployment's
	// fallback, streamViaChat): a reasoning model can think for minutes before the first header, and the
	// streaming floor aborted the call while its tokens were billed. The same as Azure's.
	longWaitHeaderTimeout = 5 * time.Minute
)

// doProviderRequest is client.Do for a provider call. A timeout the transport itself hit while the caller's
// context is still live — the headers never came, the connect or the handshake stalled — is the upstream not
// answering: ErrProviderDown, which the retry wrapper retries and the breaker counts (R-provider-3). A bare
// header timeout matches context.DeadlineExceeded, which both treat as the caller's own. The caller's own
// cancel or deadline is returned unchanged.
func doProviderRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil && req.Context().Err() == nil && isTransportTimeout(err) {
		return nil, &upstreamTimeoutError{cause: err, noReplay: waitsLong(client)}
	}
	return resp, err
}

// upstreamTimeoutError is ErrProviderDown for a request the provider did not answer in time. It keeps the
// transport's text but does not unwrap to it: the cause matches context.DeadlineExceeded, which every layer above
// reads as the caller's own deadline (no retry, a timeout code), the opposite of what this is.
type upstreamTimeoutError struct {
	cause error
	// noReplay marks a request on a long-wait client (a non-streaming answer the upstream may have produced and
	// billed by the time the floor fired): retrying it would bill again and run into the stream's wall clock.
	noReplay bool
}

// waitsLong reports a client whose header floor is the long one: its timed-out requests are not replayed.
func waitsLong(client *http.Client) bool {
	t, ok := client.Transport.(*http.Transport)
	return ok && t.ResponseHeaderTimeout >= longWaitHeaderTimeout
}

// replayForbidden reports an error the retry wrapper must not replay (upstreamTimeoutError.noReplay); the breaker
// still counts it as the outage it is.
func replayForbidden(err error) bool {
	var timeout *upstreamTimeoutError
	return errors.As(err, &timeout) && timeout.noReplay
}

func (e *upstreamTimeoutError) Error() string {
	return ErrProviderDown.Error() + ": the provider did not answer in time: " + e.cause.Error()
}

func (e *upstreamTimeoutError) Unwrap() error { return ErrProviderDown }

// isTransportTimeout reports a timeout error: context.DeadlineExceeded (net/http's header timeout matches it)
// or a net.Error that says it timed out.
func isTransportTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// authHTTPClient is used for OAuth device-flow and token-exchange calls (fixed
// public hosts). Its own guarded clone of the shared settings — not the raw
// shared transport — so a 307/308 from a token endpoint cannot replay the
// credential-bearing body toward an internal address (the same class the
// Azure/fixed-host clients close). Allow-listed guard variant for the same
// operator-trust reason as ConfigureFixedHostClient; 30s timeout suits
// interactive auth flows.
var authHTTPClient = &http.Client{
	Timeout:       30 * time.Second,
	Transport:     guardedAuthTransport(),
	CheckRedirect: egress.NoRedirects,
}

func guardedAuthTransport() *http.Transport {
	t := sharedTransportBase().Clone()
	egress.GuardAllowListedTransport(t, &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}, egress.BlockInternal)
	return t
}

// fixedHostRuntime is the client every fixed-host provider (Claude, OpenAI, Gemini, …) uses.
// sharedHTTPClient itself stays unguarded because localModelsClient and the unconfigured
// Azure client derive from its transport and must be able to reach loopback; the fixed-host
// providers get a guarded CLONE. At boot ConfigureProviders installs that clone under the
// deployment's egress policy (BlockInternal in cloud, AllowLoopback on desktop,
// AllowPrivateNetwork when PAD_AI_ALLOW_PRIVATE_EGRESS is set) plus NoRedirects — pure
// defense-in-depth: the hosts are constants in the binary or operator *_API_URL overrides,
// so the guard can only refuse a misconfiguration (or a redirect) that would otherwise send
// a credential-bearing request to an internal address. Until that boot call runs (tests,
// tools) the base client serves, which is the long-standing behavior.
var fixedHostRuntime = struct {
	mu     sync.RWMutex
	client *http.Client
	// longWait is the client for non-streaming chat requests (longWaitHeaderTimeout); otherwise identical.
	longWait *http.Client
}{client: sharedHTTPClient, longWait: longWaitClient(sharedTransportBase(), nil)}

func fixedHostClient() *http.Client {
	fixedHostRuntime.mu.RLock()
	defer fixedHostRuntime.mu.RUnlock()
	return fixedHostRuntime.client
}

// fixedHostClientFor is the fixed-host client for a request that streams (the streaming header floor) or waits
// for the whole answer before its first header (the long one).
func fixedHostClientFor(stream bool) *http.Client {
	if stream {
		return fixedHostClient()
	}
	fixedHostRuntime.mu.RLock()
	defer fixedHostRuntime.mu.RUnlock()
	return fixedHostRuntime.longWait
}

// longWaitClient clones base with the long header floor; checkRedirect is the client's redirect policy.
func longWaitClient(base *http.Transport, checkRedirect func(*http.Request, []*http.Request) error) *http.Client {
	t := base.Clone()
	t.ResponseHeaderTimeout = longWaitHeaderTimeout
	return &http.Client{Timeout: 0, Transport: t, CheckRedirect: checkRedirect}
}

// ConfigureFixedHostClient swaps in the guarded client for every fixed-host
// provider. One call at boot, from ConfigureProviders. It uses the
// allow-listed guard variant, not the strict one: fixed hosts are operator
// trusted (binary constants or *_API_URL env), so a corporate proxy on a
// private address must keep working — the TARGET host is still judged on its
// concrete IP for direct connections and in the proxy hook for proxied ones.
func ConfigureFixedHostClient(policy egress.Policy) {
	base := sharedTransportBase().Clone()
	egress.GuardAllowListedTransport(base, &net.Dialer{Timeout: providerDialTimeout, KeepAlive: 30 * time.Second}, policy)
	c := &http.Client{Timeout: 0, Transport: base, CheckRedirect: egress.NoRedirects}
	// The long-wait client carries the same guard and redirect policy: only the header floor differs. Its pool is
	// its own, so PAD_AI_MAX_CONNS_PER_HOST bounds streaming and non-streaming connections to a host separately.
	long := longWaitClient(base, egress.NoRedirects)
	fixedHostRuntime.mu.Lock()
	fixedHostRuntime.client = c
	fixedHostRuntime.longWait = long
	fixedHostRuntime.mu.Unlock()
}
