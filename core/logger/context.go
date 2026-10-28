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

// attrsKey carries the attributes WithAttrs added to a context.
type attrsKey struct{}

// WithAttrs returns ctx carrying attrs (after any it already carries): every FromContext line under it has them. For
// what a layer knows that the request does not, such as the chat stream a detached worker serves.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	prev, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	return context.WithValue(ctx, attrsKey{}, append(prev[:len(prev):len(prev)], attrs...))
}

// SetContextAttrs installs the extractor FromContext uses. Intended to be called once at startup; nil removes it.
func SetContextAttrs(fn ContextAttrs) {
	if fn == nil {
		contextAttrs.Store(nil)
		return
	}
	contextAttrs.Store(&fn)
}

// FromContext returns the default logger carrying the attributes the registered extractor finds on ctx, then those
// WithAttrs added, so a line logged while serving a request can be joined with that request's access-log line and
// trace. When ctx carries none, it is slog.Default() itself.
func FromContext(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if ctx == nil {
		return l
	}
	var attrs []slog.Attr
	if fn := contextAttrs.Load(); fn != nil {
		attrs = (*fn)(ctx)
	}
	if own, _ := ctx.Value(attrsKey{}).([]slog.Attr); len(own) > 0 {
		attrs = append(attrs[:len(attrs):len(attrs)], own...)
	}
	if len(attrs) == 0 {
		return l
	}
	return slog.New(l.Handler().WithAttrs(attrs))
}
