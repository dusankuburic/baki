package logger

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// ContextAttrs returns the attributes that identify what ctx belongs to; for an HTTP request the server's
// extractor returns its request_id, user_id and trace_id. Core cannot import the packages that know those
// values, so the server registers one at startup (SetContextAttrs), as it does the PanicHook. Implementations
// must be cheap, must not panic and must accept any context.
type ContextAttrs func(ctx context.Context) []slog.Attr

var contextAttrs atomic.Pointer[ContextAttrs]

// SetContextAttrs installs the extractor FromContext uses. Intended to be called once at startup; nil removes it.
func SetContextAttrs(fn ContextAttrs) {
	if fn == nil {
		contextAttrs.Store(nil)
		return
	}
	contextAttrs.Store(&fn)
}

// FromContext returns the default logger carrying the attributes the registered extractor finds on ctx, so a
// line logged while serving a request can be joined with that request's access-log line and trace. Without an
// extractor, or when ctx carries nothing it knows, it is slog.Default() itself.
func FromContext(ctx context.Context) *slog.Logger {
	l := slog.Default()
	fn := contextAttrs.Load()
	if fn == nil || ctx == nil {
		return l
	}
	attrs := (*fn)(ctx)
	if len(attrs) == 0 {
		return l
	}
	return slog.New(l.Handler().WithAttrs(attrs))
}
