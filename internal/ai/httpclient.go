package ai

import (
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
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     maxConnsPerHostFromEnv(),
		IdleConnTimeout:     90 * time.Second,
		// B1.8: a header-only floor — a call site that forgets a per-request context gets bounded headers
		// without killing long SSE bodies the way Client.Timeout would.
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	},
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
}{client: sharedHTTPClient}

func fixedHostClient() *http.Client {
	fixedHostRuntime.mu.RLock()
	defer fixedHostRuntime.mu.RUnlock()
	return fixedHostRuntime.client
}

// ConfigureFixedHostClient swaps in the guarded client for every fixed-host
// provider. One call at boot, from ConfigureProviders. It uses the
// allow-listed guard variant, not the strict one: fixed hosts are operator
// trusted (binary constants or *_API_URL env), so a corporate proxy on a
// private address must keep working — the TARGET host is still judged on its
// concrete IP for direct connections and in the proxy hook for proxied ones.
func ConfigureFixedHostClient(policy egress.Policy) {
	base := sharedTransportBase().Clone()
	egress.GuardAllowListedTransport(base, &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}, policy)
	c := &http.Client{Timeout: 0, Transport: base, CheckRedirect: egress.NoRedirects}
	fixedHostRuntime.mu.Lock()
	fixedHostRuntime.client = c
	fixedHostRuntime.mu.Unlock()
}
