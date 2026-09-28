package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// StreamRequestTimeout is the outer deadline for chat/analysis streaming
// responses (*/stream). A model reply can legitimately take minutes, so these
// are exempt from the normal per-request deadline — but "exempt" used to mean
// "no deadline at all", and a stalled upstream then held the goroutine, the
// RLS transaction, and its pooled connection forever. Generous, and finite.
const StreamRequestTimeout = 15 * time.Minute

// BulkRequestTimeout bounds the knowledge-base bulk routes (upload,
// re-index): a re-index page walk is up to ~100 sequential embedding calls
// (10 000 chunks / 100 per batch) plus a multi-statement update per page, an
// upload up to 6, and any 429 makes the retrying provider back off — the
// default 30 s deadline tripped after the embeddings had been bought, and
// the deadline error rendered as a masked 500. Bounded and long, like the
// stream deadline; the DB statement_timeout is per statement and unaffected.
const BulkRequestTimeout = 5 * time.Minute

func RequestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isLongLivedPath(r.URL.Path) {
				// WebSocket and SSE sessions live as long as the tab does.
				next.ServeHTTP(w, r)
				return
			}
			deadline := d
			switch {
			case isStreamingPath(r.URL.Path):
				deadline = StreamRequestTimeout
			case isBulkPath(r.URL.Path):
				deadline = BulkRequestTimeout
			}
			ctx, cancel := context.WithTimeout(r.Context(), deadline)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// isLongLivedPath: connections that are open-ended by design and get no
// deadline (the client, not a timer, ends them).
func isLongLivedPath(path string) bool {
	return path == "/ws" || path == "/api/events"
}

// isStreamingPath: request/response streams that are long but bounded.
func isStreamingPath(path string) bool {
	return strings.HasSuffix(path, "/stream")
}

// isBulkPath: the knowledge-base bulk routes (POST /api/orgs/{id}/knowledge/
// upload and /reindex), whose work is a sequence of provider calls.
func isBulkPath(path string) bool {
	return strings.HasSuffix(path, "/knowledge/upload") || strings.HasSuffix(path, "/knowledge/reindex")
}
