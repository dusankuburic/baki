// Package middleware provides cross-cutting HTTP middleware: recovery from
// panics, per-IP rate limiting, request-ID propagation, and structured
// access logs.
package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/reqinfo"
	"pad-core/logger"
)

// requestIDCtxKey carries the request ID AccessLog stamps when no RequestInfo layer runs outside it.
type requestIDCtxKey struct{}

// AccessLog writes one structured line per request. It runs after the handler returns, so the line carries the
// final status, bytes and latency, and the user the request acted as, which authentication further in records on
// the request's reqinfo.Info (a context value set in there never reaches this layer). SSE/WebSocket upgrades
// still get an entry when the goroutine eventually exits.
// The request ID and client IP are the Info's (RequestInfo). Used on its own, without an Info, AccessLog stamps
// and echoes the ID itself.
func AccessLog(trustedProxies []string) func(http.Handler) http.Handler {
	// Parsed once: ClientIP re-parses every CIDR per call.
	proxies := parseProxySet(trustedProxies)
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := reqinfo.From(r.Context())
			var reqID string
			if info != nil {
				reqID = info.ID
			} else {
				reqID = requestID(r)
				w.Header().Set(RequestIDHeader, reqID)
				r = r.WithContext(context.WithValue(r.Context(), requestIDCtxKey{}, reqID))
			}

			sr := NewResponseRecorder(w)

			// Log the canonical dispatch path, taken before the handler runs, as Metrics does and for the same reason: a request refused before the router (429)
			// kept its raw spelling, and redactPath, which matches the canonical /api/invites/ only, wrote the invite credential of /api/v1/invites/{token}/accept into the log.
			p := canonicalPath(r.URL.Path)

			start := time.Now()
			h.ServeHTTP(maybeHijackable(sr), r)
			latency := time.Since(start)

			var userID, remoteIP string
			if info != nil {
				if a := info.Actor(); a != nil {
					userID = a.UserID
				}
				remoteIP = info.ClientIP
			} else {
				if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
					userID = claims.UserID
				}
				remoteIP = proxies.clientIP(r)
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
				"remote_ip", remoteIP,
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
