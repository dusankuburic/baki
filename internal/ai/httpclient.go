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
var sharedHTTPClient = &http.Client{
	Timeout: 120 * time.Second,
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
