package ai

import (
	"net/http"
	"os"
	"strconv"
	"time"
)

// maxConnsPerHostFromEnv reads PAD_AI_MAX_CONNS_PER_HOST. Idle pooling was
// already capped, but ACTIVE outbound connections were unbounded: N users × 3
// concurrent streams each hold one provider connection for up to the 10-minute
// stream cap, and a small deploy could exhaust its fd budget. The default
// (256) covers healthy multi-tenant load; 0 disables the cap for operators
// who prefer the old behaviour.
func maxConnsPerHostFromEnv() int {
	if v := os.Getenv("PAD_AI_MAX_CONNS_PER_HOST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 256
}

// sharedHTTPClient is the single http.Client used by every provider.
//
// Previously each provider instance constructed its own http.Client, and the
// ProviderFactory builds a fresh provider per request — so TCP/TLS connections
// were never reused across requests (each client had its own idle-connection
// pool that was discarded with the provider). Sharing one client lets the
// underlying transport pool and reuse keep-alive connections, cutting latency
// and file-descriptor churn under load.
//
// MaxIdleConnsPerHost is raised from the default of 2 because a busy instance
// talks to a small number of provider hosts with high concurrency.
//
// Timeout is deliberately 0 (no client-level deadline). Go's http.Client.Timeout
// includes reading the response body, so a 120s value here would silently kill
// any streaming response (SSE) that took longer than 120s — even though the chat
// layer advertises a 10-minute stream cap. Per-request cancellation is enforced
// via the context passed to each provider's Stream/Chat call (chat.go sets a
// 10-min context timeout + a 90s idle-timeout watchdog).
var sharedHTTPClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     maxConnsPerHostFromEnv(),
		IdleConnTimeout:     90 * time.Second,
		// B1.8: a header-only floor — a future call site that forgets a
		// per-request context gets bounded headers (30s) without killing
		// long SSE bodies the way Client.Timeout would.
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// authHTTPClient is used for OAuth device-flow and token-exchange calls.
// It shares the same transport (and therefore connection pool) as sharedHTTPClient
// but with a shorter 30s timeout suited to interactive auth flows.
var authHTTPClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: sharedHTTPClient.Transport,
}
