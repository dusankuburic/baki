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
	"go.opentelemetry.io/otel/trace"
	"pad-analyzer/internal/auth"
	"pad-core/logger"
)

// requestIDKey is the private context key for the per-request ID.
type requestIDCtxKey struct{}

// RequestIDHeader is the HTTP header used to convey the per-request ID between the load balancer / reverse proxy and this service, and between the service
// and its clients. The middleware accepts whatever the proxy stamped (so trace context is preserved) and generates a new UUIDv4 only when the header is absent.
const RequestIDHeader = "X-Request-ID"

// RequestIDFromContext returns the request ID stamped on ctx by the access
// log middleware. Empty string if the middleware wasn't applied.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// AccessLog wraps h with request-ID propagation and a one-line structured log per request. The ID comes from the X-Request-ID header if present (reverse-proxy correlation) or a fresh UUIDv4, and is echoed back in the response header.
// Logging runs after the handler returns, so the line carries the final status, bytes and latency; SSE/WebSocket upgrades still get an entry when the goroutine eventually exits.
func AccessLog(trustedProxies []string) func(http.Handler) http.Handler {
	// Parsed once: ClientIP re-parses every CIDR per call.
	proxies := parseProxySet(trustedProxies)
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

			// Log the canonical dispatch path, taken before the handler runs, as Metrics does and for the same reason: a request refused before the router (429)
			// kept its raw spelling, and redactPath, which matches the canonical /api/invites/ only, wrote the invite credential of /api/v1/invites/{token}/accept into the log.
			p := canonicalPath(r.URL.Path)

			start := time.Now()
			h.ServeHTTP(maybeHijackable(sr), r)
			latency := time.Since(start)

			var userID string
			if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
				userID = claims.UserID
			}

			level := levelFor(p, sr.status)
			args := []any{
				"request_id", reqID,
				"method", r.Method,
				"path", redactPath(p),
				"status", sr.status,
				"bytes", sr.bytes,
				"latency_ms", latency.Milliseconds(),
				"user_id", userID,
				"remote_ip", proxies.clientIP(r),
			}
			// trace_id lets an operator pivot from a trace (App Insights / any OTLP backend) to
			// this request's log lines and back; otelhttp is outermost, so the span is on the ctx.
			if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
				args = append(args, "trace_id", sc.TraceID().String())
			}
			logAt(level, "access", args...)
		})
	}
}

// levelFor picks the log level for an access line: health-probe chatter debug; 4xx warn; 5xx error — except 501 and 503, which are warn: refusals BY DESIGN (a feature this deployment does not run, a connection cap, a declared-unavailable dependency), several reachable by any caller at will, so an ERROR line per request let anyone flood the error log and its alerting (render.ErrorUnlogged deliberately left it out; the access log must not put it back).
// Where a 503 does stand for a real failure, the site answers through render.Error*, which logs the cause at ERROR; http_requests_total still counts it as a 5xx. 500/502/504 stay error.
func levelFor(path string, status int) string {
	if path == "/healthz" || path == "/readyz" || path == "/api/health" {
		return "debug"
	}
	switch {
	case status == http.StatusNotImplemented, status == http.StatusServiceUnavailable:
		return "warn"
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

// hijackableRecorder is ResponseRecorder + http.Hijacker, returned only when the underlying writer
// supports hijacking — otherwise wrapping would falsely advertise an interface the next layer uses.
type hijackableRecorder struct {
	*ResponseRecorder
	http.Hijacker
}

// maybeHijackable returns sr unchanged if the underlying writer can't hijack; on a normal listener
// it exposes the Hijacker to the next layer (WebSocket upgrades).
func maybeHijackable(sr *ResponseRecorder) http.ResponseWriter {
	if h, ok := sr.ResponseWriter.(http.Hijacker); ok {
		return &hijackableRecorder{ResponseRecorder: sr, Hijacker: h}
	}
	return sr
}

// redactPath removes credentials that live in a URL PATH segment before the path is logged: only /api/invites/{token}/accept embeds one — the single-use invite credential mailed to the recipient — and logs are shipped, retained, and read by more people than the mail, so a full-path line republished a live invite.
// Query strings are already excluded (r.URL.Path, not RequestURI), so no other route needs handling. Path shape preserved for debugging.
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
