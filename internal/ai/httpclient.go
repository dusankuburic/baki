package ai

import (
	"net/http"
	"time"
)

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
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

// authHTTPClient is used for OAuth device-flow and token-exchange calls.
// It shares the same transport (and therefore connection pool) as sharedHTTPClient
// but with a shorter 30s timeout suited to interactive auth flows.
var authHTTPClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: sharedHTTPClient.Transport,
}
