package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/config"
)

// StreamRequestTimeout is the outer deadline for chat/analysis streaming responses (*/stream). A model reply can legitimately take minutes, so these are exempt from the normal per-request deadline — but "exempt" used to mean "no deadline at all", and a stalled upstream then held the goroutine, the RLS transaction, and its pooled connection forever. Generous, and finite.
const StreamRequestTimeout = 15 * time.Minute

// BulkRequestTimeout bounds the knowledge-base bulk routes (upload, re-index): an upload is up to 25 sequential embedding calls (2 500 chunks / 100 per batch), and any 429 makes the retrying provider back off — the default 30 s deadline tripped after the embeddings had been bought, and the deadline error rendered as a masked 500.
// A re-index additionally stops itself after rag's reindexRequestBudget (90 s) of wall clock — the Azure Container Apps HTTP ingress caps every request at 240 s with no knob, so the walk answers with a resume cursor well inside the cap. Bounded and long, like the stream deadline; the DB statement_timeout is per statement and unaffected.
const BulkRequestTimeout = 5 * time.Minute

// RequestTimeout gives each request the deadline of its class (route.Deadline), d by default: in declarative mode
// the class of the policy resolved for it, otherwise the legacy path classifier's (TimeoutClass), which shadow mode
// checks against the policy.
func RequestTimeout(d time.Duration, mode config.RoutePolicyMode) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			deadline := d
			switch deadlineClass(r, mode) {
			case route.DeadlineNone:
				// WebSocket and SSE sessions live as long as the tab does.
				next.ServeHTTP(w, r)
				return
			case route.DeadlineStream:
				deadline = StreamRequestTimeout
			case route.DeadlineBulk:
				deadline = BulkRequestTimeout
			}
			ctx, cancel := context.WithTimeout(r.Context(), deadline)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// deadlineClass is r's deadline class under mode.
func deadlineClass(r *http.Request, mode config.RoutePolicyMode) route.Deadline {
	p, resolved := RequestPolicy(r)
	if mode == config.RoutePolicyDeclarative {
		return p.Deadline
	}
	legacy := TimeoutClass(r)
	if mode == config.RoutePolicyShadow && resolved && p.Kind == route.KindEndpoint && legacy != p.Deadline.String() {
		ReportPolicyMismatch(r, p, DecisionDeadline, legacy, p.Deadline.String())
	}
	switch legacy {
	case TimeoutNone:
		return route.DeadlineNone
	case TimeoutStream:
		return route.DeadlineStream
	case TimeoutBulk:
		return route.DeadlineBulk
	}
	return route.DeadlineDefault
}

// Deadline classes the legacy classifier assigns (TimeoutClass); the same names as route.Deadline's.
const (
	TimeoutNone    = "none"    // WebSocket and SSE: open-ended by design
	TimeoutStream  = "stream"  // StreamRequestTimeout
	TimeoutBulk    = "bulk"    // BulkRequestTimeout
	TimeoutDefault = "default" // the configured request timeout
)

// TimeoutClass is the legacy deadline class of r, judged on the path chi dispatches on (route.DispatchPath): this
// layer runs outside the mux, before its path.Clean and /api/v1 alias strip. On the raw string GET /api/v1/events
// got the 30 s default and the EventSource reconnected every 30 s; on the cleaned decoded path an escaped
// ".../versions/1%2F..%2F..%2Fapi%2Fevents" read as /api/events while chi served it from a parameter route, which
// then ran with no deadline. Exported for the route-policy golden in package api.
func TimeoutClass(r *http.Request) string {
	p := route.DispatchPath(r.URL)
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

// canonicalPath is route.CanonicalPath, for the layers here that log a request's path.
func canonicalPath(p string) string { return route.CanonicalPath(p) }

// isLongLivedPath: connections that are open-ended by design and get no
// deadline (the client, not a timer, ends them). Takes a dispatch path.
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
