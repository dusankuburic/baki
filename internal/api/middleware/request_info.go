package middleware

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"pad-analyzer/internal/platform/reqinfo"
)

// RequestIDHeader carries the request ID between the reverse proxy and this service, and back to the client. An
// ID the proxy stamped is kept, so its logs and ours share it.
const RequestIDHeader = "X-Request-ID"

const (
	// maxRequestIDLen: a longer inbound ID is replaced by a fresh one, so a client cannot bloat every log line of
	// its request.
	maxRequestIDLen = 128
	// maxUserAgentLen bounds the user agent kept on the request's Info.
	maxUserAgentLen = 512
)

// RequestInfo installs the request's reqinfo.Info, the record every layer inside shares: the request ID
// (X-Request-ID as received, or a fresh UUID, echoed in the response header), the proxy-aware client IP, the TCP
// peer's IP and a bounded user agent. Authentication records the actor on it later, for the layers outside to
// read. BuildHandler installs it right inside the outermost Recovery. An Info the context already carries is
// reused, not replaced.
func RequestInfo(trustedProxies []string) func(http.Handler) http.Handler {
	proxies := parseProxySet(trustedProxies)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info := reqinfo.From(r.Context())
			if info == nil {
				info = &reqinfo.Info{
					ID:        requestID(r),
					ClientIP:  proxies.clientIP(r),
					PeerIP:    peerIP(r),
					UserAgent: truncateUTF8(r.UserAgent(), maxUserAgentLen),
				}
				r = r.WithContext(reqinfo.With(r.Context(), info))
			}
			if info.ID != "" {
				w.Header().Set(RequestIDHeader, info.ID)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestIDFromContext returns the ID of the request ctx belongs to: its Info's, else the one AccessLog stamped
// when it ran without RequestInfo; "" outside a request.
func RequestIDFromContext(ctx context.Context) string {
	if info := reqinfo.From(ctx); info != nil {
		return info.ID
	}
	if v, ok := ctx.Value(requestIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// LogAttrs is the extractor the server registers with logger.SetContextAttrs, so logger.FromContext lines carry
// the request_id, user_id and trace_id keys of the request's access-log line. user_id appears once the request's
// actor is recorded; outside a request, only an active span's trace_id is added.
func LogAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	if info := reqinfo.From(ctx); info != nil {
		attrs = make([]slog.Attr, 0, 3)
		if info.ID != "" {
			attrs = append(attrs, slog.String("request_id", info.ID))
		}
		if a := info.Actor(); a != nil && a.UserID != "" {
			attrs = append(attrs, slog.String("user_id", a.UserID))
		}
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()))
	}
	return attrs
}

// requestID is the request's X-Request-ID, trimmed, or a fresh UUIDv4 when it is absent or too long.
func requestID(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get(RequestIDHeader))
	if id == "" || len(id) > maxRequestIDLen {
		id = uuid.NewString()
	}
	return id
}

// peerIP is the TCP peer's address without its port, whatever the forwarding headers claim; RemoteAddr as is
// when it does not parse.
func peerIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
