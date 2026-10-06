package middleware

import (
	"context"
	"net/http"
	"path"
	"strings"
	"time"
)

// StreamRequestTimeout is the outer deadline for chat/analysis streaming responses (*/stream). A model reply can legitimately take minutes, so these are exempt from the normal per-request deadline — but "exempt" used to mean "no deadline at all", and a stalled upstream then held the goroutine, the RLS transaction, and its pooled connection forever. Generous, and finite.
const StreamRequestTimeout = 15 * time.Minute

// BulkRequestTimeout bounds the knowledge-base bulk routes (upload, re-index): an upload is up to 25 sequential embedding calls (2 500 chunks / 100 per batch), and any 429 makes the retrying provider back off — the default 30 s deadline tripped after the embeddings had been bought, and the deadline error rendered as a masked 500.
// A re-index additionally stops itself after rag's reindexRequestBudget (90 s) of wall clock — the Azure Container Apps HTTP ingress caps every request at 240 s with no knob, so the walk answers with a resume cursor well inside the cap. Bounded and long, like the stream deadline; the DB statement_timeout is per statement and unaffected.
const BulkRequestTimeout = 5 * time.Minute

func RequestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Classify on the path the router will dispatch on (dispatchPath), not the raw one: this layer runs outside the mux, before its path.Clean and /api/v1 alias strip.
			// On the raw string, GET /api/v1/events — the SSE bus, served by the same handler as /api/events — got the 30 s default and the EventSource reconnected every 30 s, and a non-canonical spelling of a */stream or knowledge bulk route (".../stream/", ".../knowledge/./upload") got the short deadline its handler cannot finish inside.
			deadline := d
			switch TimeoutClass(r) {
			case TimeoutNone:
				// WebSocket and SSE sessions live as long as the tab does.
				next.ServeHTTP(w, r)
				return
			case TimeoutStream:
				deadline = StreamRequestTimeout
			case TimeoutBulk:
				deadline = BulkRequestTimeout
			}
			ctx, cancel := context.WithTimeout(r.Context(), deadline)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Deadline classes RequestTimeout assigns (TimeoutClass).
const (
	TimeoutNone    = "none"    // WebSocket and SSE: open-ended by design
	TimeoutStream  = "stream"  // StreamRequestTimeout
	TimeoutBulk    = "bulk"    // BulkRequestTimeout
	TimeoutDefault = "default" // the configured request timeout
)

// TimeoutClass is the deadline class RequestTimeout gives r, judged on the path the router will dispatch on.
// Exported for the route-policy golden in package api.
func TimeoutClass(r *http.Request) string {
	p := dispatchPath(r)
	switch {
	case isLongLivedPath(p):
		return TimeoutNone
	case isStreamingPath(p):
		return TimeoutStream
	case isBulkPath(p):
		return TimeoutBulk
	}
	return TimeoutDefault
}

// dispatchPath returns the path chi will route r on: RawPath as sent (path.Clean rewrites only r.URL.Path; the /api/v1 alias strip applies to both), else canonicalPath of r.URL.Path. It mirrors inboundRoutePath in package api (see canonicalPath).
// Classifying on the cleaned decoded path instead let encoded separators and dot segments — ".../versions/1%2F..%2F..%2F..%2F..%2F..%2Fapi%2Fevents" — read as /api/events or /ws while chi served the request from an ordinary param-terminated route, which then ran with no deadline at all.
func dispatchPath(r *http.Request) string {
	if r.URL.RawPath != "" {
		return stripVersionAlias(r.URL.RawPath)
	}
	return canonicalPath(r.URL.Path)
}

// canonicalPath returns the path the router will actually dispatch on for a raw request path without percent-escapes (see dispatchPath): path.Clean (Router.ServeHTTP), then the /api/v1 alias strip (registerRoutes), in that order.
// It mirrors canonicalRequestPath in package api, which cannot be called from here (api imports middleware); the two must stay in step, so a normalization the router grows belongs in both.
func canonicalPath(p string) string {
	return stripVersionAlias(path.Clean(p))
}

// stripVersionAlias is the /api/v1 → /api alias strip on its own — "/api/v1" only as a whole segment — as the router applies it to r.URL.RawPath, which it does not clean. It mirrors stripVersionAlias in package api.
func stripVersionAlias(p string) string {
	if rest := strings.TrimPrefix(p, "/api/v1/"); rest != p {
		return "/api/" + rest
	}
	if p == "/api/v1" {
		return "/api"
	}
	return p
}

// isLongLivedPath: connections that are open-ended by design and get no
// deadline (the client, not a timer, ends them). Takes a dispatchPath.
func isLongLivedPath(path string) bool {
	return path == "/ws" || path == "/api/events"
}

// isStreamingPath: request/response streams that are long but bounded.
func isStreamingPath(path string) bool {
	return strings.HasSuffix(path, "/stream")
}

// isBulkPath: the knowledge-base bulk routes (POST /api/orgs/{id}/knowledge/upload and /reindex), whose work is a sequence of provider calls.
func isBulkPath(path string) bool {
	return strings.HasSuffix(path, "/knowledge/upload") || strings.HasSuffix(path, "/knowledge/reindex")
}
