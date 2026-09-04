// Package middleware provides cross-cutting HTTP middleware: recovery from
// panics, per-IP rate limiting, request-ID propagation, and structured
// access logs.
package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/auth"
	"pad-core/logger"
)

// requestIDKey is the private context key for the per-request ID.
type requestIDCtxKey struct{}

// RequestIDHeader is the HTTP header used to convey the per-request ID
// between the load balancer / reverse proxy and this service, and between
// the service and its clients. The middleware accepts whatever the proxy
// stamped (so trace context is preserved) and generates a new UUIDv4 only
// when the header is absent.
const RequestIDHeader = "X-Request-ID"

// RequestIDFromContext returns the request ID stamped on ctx by the access
// log middleware. Empty string if the middleware wasn't applied.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// AccessLog wraps h with request-ID propagation and a one-line structured
// log per request. The request ID is taken from the X-Request-ID header
// if present (so a reverse proxy / load balancer can stamp it for
// cross-service correlation), or a fresh UUIDv4 otherwise. It is echoed
// back in the response header so clients can quote it when reporting
// issues.
//
// Logging is performed *after* the handler returns, so the line carries
// the final status, bytes written, and total latency. SSE / WebSocket
// upgrades that never return a normal response still get a log entry
// when the goroutine eventually exits.
func AccessLog(trustedProxies []string) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := strings.TrimSpace(r.Header.Get(RequestIDHeader))
			if reqID == "" || len(reqID) > 128 {
				reqID = uuid.NewString()
			}
			w.Header().Set(RequestIDHeader, reqID)

			ctx := context.WithValue(r.Context(), requestIDCtxKey{}, reqID)
			r = r.WithContext(ctx)

			sr := NewResponseRecorder(w)

			start := time.Now()
			h.ServeHTTP(maybeHijackable(sr), r)
			latency := time.Since(start)

			var userID string
			if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
				userID = claims.UserID
			}

			level := levelFor(r.URL.Path, sr.status)
			logAt(level, "access",
				"request_id", reqID,
				"method", r.Method,
				"path", redactPath(r.URL.Path),
				"status", sr.status,
				"bytes", sr.bytes,
				"latency_ms", latency.Milliseconds(),
				"user_id", userID,
				"remote_ip", ClientIP(r, trustedProxies),
			)
		})
	}
}

// levelFor picks the log level for an access-log line. Health-probe
// chatter is demoted to debug; 4xx is warn; 5xx is error.
func levelFor(path string, status int) string {
	if path == "/healthz" || path == "/readyz" || path == "/api/health" {
		return "debug"
	}
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "warn"
	}
	return "info"
}

func logAt(level, msg string, args ...any) {
	switch level {
	case "error":
		logger.Error(msg, args...)
	case "warn":
		logger.Warn(msg, args...)
	case "debug":
		logger.Debug(msg, args...)
	default:
		logger.Info(msg, args...)
	}
}

// hijackableRecorder is ResponseRecorder + http.Hijacker. We only return it
// when the underlying writer supports hijacking — otherwise wrapping it
// would falsely advertise an interface the next layer expects to use.
type hijackableRecorder struct {
	*ResponseRecorder
	http.Hijacker
}

// maybeHijackable returns sr unchanged if the underlying writer doesn't
// support hijacking. For WebSocket upgrades on a normal listener, the
// underlying writer is http.Hijacker; expose that to the next layer.
func maybeHijackable(sr *ResponseRecorder) http.ResponseWriter {
	if h, ok := sr.ResponseWriter.(http.Hijacker); ok {
		return &hijackableRecorder{ResponseRecorder: sr, Hijacker: h}
	}
	return sr
}

// redactPath removes credentials that live in a URL PATH segment before the
// path is written to a log line.
//
// Only one route embeds a secret this way: /api/invites/{token}/accept, where
// the segment is the single-use invite credential mailed to the recipient.
// Logs are shipped, retained, and read by more people than the mail is, so a
// full-path access line effectively republished a live invite. Query strings
// are already excluded from the access log (r.URL.Path, not RequestURI), which
// is why no other route needs handling here.
//
// The path shape is preserved so the line stays useful for debugging.
func redactPath(p string) string {
	const invitePrefix = "/api/invites/"
	if !strings.HasPrefix(p, invitePrefix) {
		return p
	}
	rest := p[len(invitePrefix):]
	if rest == "" {
		return p
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return invitePrefix + "[redacted]" + rest[i:]
	}
	return invitePrefix + "[redacted]"
}
