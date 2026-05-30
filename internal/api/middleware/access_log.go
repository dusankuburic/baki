// Package middleware provides cross-cutting HTTP middleware: recovery from
// panics, per-IP rate limiting, request-ID propagation, and structured
// access logs.
package middleware

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/logger"
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

// statusRecorder wraps an http.ResponseWriter to capture the status code
// and bytes written so the access log line can include them. It explicitly
// keeps the http.Flusher interface so SSE handlers still work.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	if !sr.wroteHeader {
		sr.status = code
		sr.wroteHeader = true
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(p []byte) (int, error) {
	if !sr.wroteHeader {
		// Mirror net/http's default — first Write implies 200.
		sr.status = http.StatusOK
		sr.wroteHeader = true
	}
	n, err := sr.ResponseWriter.Write(p)
	sr.bytes += n
	return n, err
}

// Flush forwards Flush calls when the underlying writer is an http.Flusher
// (required for SSE: without this delegate the recorder would silently
// swallow flushes and stream events would never reach the client).
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying writer so WebSocket upgrades work.
// The /ws route hijacks the connection; without this method the upgrader
// fails with "ResponseWriter does not support Hijacker".
//
// We intentionally use a type assertion at runtime (rather than embedding
// http.Hijacker) so the recorder still works behind writers that don't
// support hijacking — those handlers just won't call Hijack.
//
// Implemented via an interface check below.

// remoteIP extracts the immediate-peer IP from the request, ignoring any
// X-Forwarded-For. Access-log IP attribution intentionally records the
// peer, not the spoofable header — the rate limiter already handles XFF
// trust separately, but the access log should reflect what actually
// connected so abuse from a misconfigured trusted proxy is detectable.
func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
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
func AccessLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept upstream-supplied ID; only generate one if absent. Trim
		// to a sane length so a malicious client can't bloat log lines.
		reqID := strings.TrimSpace(r.Header.Get(RequestIDHeader))
		if reqID == "" || len(reqID) > 128 {
			reqID = uuid.NewString()
		}
		w.Header().Set(RequestIDHeader, reqID)

		ctx := context.WithValue(r.Context(), requestIDCtxKey{}, reqID)
		r = r.WithContext(ctx)

		// Use the access-log recorder so the after-handler log line knows
		// the status / byte count.
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		start := time.Now()
		h.ServeHTTP(maybeHijackable(sr), r)
		latency := time.Since(start)

		// Pull user_id from claims if the request was authenticated; the
		// auth middleware stashes claims before the handler runs.
		var userID string
		if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
			userID = claims.UserID
		}

		// Use the most-specific log level that's not noisy: info for
		// successful requests, warn for 4xx, error for 5xx. Health
		// probes log at debug to avoid drowning the access log.
		level := levelFor(r.URL.Path, sr.status)
		logAt(level, "access",
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.status,
			"bytes", sr.bytes,
			"latency_ms", latency.Milliseconds(),
			"user_id", userID,
			"remote_ip", remoteIP(r),
		)
	})
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

// hijackableRecorder is statusRecorder + http.Hijacker. We only return it
// when the underlying writer supports hijacking — otherwise wrapping it
// would falsely advertise an interface the next layer expects to use.
type hijackableRecorder struct {
	*statusRecorder
	http.Hijacker
}

// maybeHijackable returns sr unchanged if the underlying writer doesn't
// support hijacking. For WebSocket upgrades on a normal listener, the
// underlying writer is http.Hijacker; expose that to the next layer.
func maybeHijackable(sr *statusRecorder) http.ResponseWriter {
	if h, ok := sr.ResponseWriter.(http.Hijacker); ok {
		return &hijackableRecorder{statusRecorder: sr, Hijacker: h}
	}
	return sr
}
