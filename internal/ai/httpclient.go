package ai

import (
	"net/http"
	"os"
	"strconv"
	"time"
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

// sharedHTTPClient is the single http.Client used by every provider: the factory builds a provider per
// request, and per-instance clients meant TCP/TLS connections were never reused across requests. MaxIdleConnsPerHost
// is raised from 2 because a busy instance talks to few hosts with high concurrency. Timeout is deliberately 0:
// http.Client.Timeout includes reading the body, so any value here would silently kill long SSE responses —
// per-request cancellation is enforced via the context (chat.go's 10-min cap + 90s idle watchdog).
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

// authHTTPClient is used for OAuth device-flow and token-exchange calls.
// It shares the same transport (and therefore connection pool) as sharedHTTPClient
// but with a shorter 30s timeout suited to interactive auth flows.
var authHTTPClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: sharedHTTPClient.Transport,
}
