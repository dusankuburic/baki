// Package errreport is the application's exception-aggregation funnel: panics
// recovered at the HTTP boundary and notable errors from service code are
// logged (slog), counted (pad_panics_total / pad_errors_reported_total), and
// forwarded to a pluggable Sink (Sentry, App Insights, …) when one has been
// registered. The default nil sink keeps metrics always on without any
// external dependency; Register adds deduplicating crash triage without
// touching call sites — one place owns exception aggregation.
package errreport

import (
	"context"
	"log/slog"
	"sync"

	"pad-analyzer/internal/metrics"
)

// Sink receives forwarded exceptions. A concrete implementation (Sentry, App
// Insights, …) deduplicates and groups them for triage. Methods must be safe
// for concurrent use and must not panic (the caller is already on an error
// path); a Sink that fails should no-op rather than propagate.
type Sink interface {
	CaptureError(ctx context.Context, err error, attrs Attrs)
	CapturePanic(ctx context.Context, recovered any, stack []byte, attrs Attrs)
}

// Attrs is the structured context attached to a forwarded event. Keys are
// arbitrary; values must be JSON-serializable by the Sink.
type Attrs = map[string]any

var (
	mu     sync.RWMutex
	active Sink
)

// Register installs the exception-aggregation sink used by CaptureError and
// CapturePanic. Passing nil unregisters. Safe to call at any time; intended to
// be called once at startup from main.
func Register(s Sink) {
	mu.Lock()
	active = s
	mu.Unlock()
}

// CaptureError forwards a notable error. It always bumps the
// pad_errors_reported_total{location} counter and logs, then forwards to the
// registered sink if any. location is a short call-site tag ("chat.stream",
// "scanner"). nil errors are ignored.
func CaptureError(ctx context.Context, location string, err error, attrs Attrs) {
	if err == nil {
		return
	}
	metrics.RecordError(location)
	slog.ErrorContext(ctx, "error reported", "location", location, "err", err, "attrs", attrs)
	forward(func(s Sink) { s.CaptureError(ctx, err, attrs) })
}

// CapturePanic forwards a recovered panic. It always bumps the
// pad_panics_total{location} counter and logs the stack, then forwards to the
// registered sink if any. location is a short call-site tag ("http", "scanner").
func CapturePanic(ctx context.Context, location string, recovered any, stack []byte, attrs Attrs) {
	metrics.RecordPanic(location)
	slog.ErrorContext(ctx, "panic reported",
		"location", location, "panic", recovered, "stack", string(stack), "attrs", attrs)
	forward(func(s Sink) { s.CapturePanic(ctx, recovered, stack, attrs) })
}

func forward(do func(Sink)) {
	mu.RLock()
	s := active
	mu.RUnlock()
	if s == nil {
		return
	}
	defer func() { _ = recover() }() // a sink must never break the caller's recovery
	do(s)
}
