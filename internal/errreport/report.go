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
	"maps"
	"sync"

	"pad-core/logger"
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
	// instanceID names this replica on every forwarded event (SetInstanceID).
	instanceID string
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
// pad_errors_reported_total{location} counter and logs (with ctx's request
// attributes, logger.FromContext), then forwards to the registered sink if
// any. location is a short call-site tag ("chat.stream", "scanner"). nil
// errors are ignored.
func CaptureError(ctx context.Context, location string, err error, attrs Attrs) {
	if err == nil {
		return
	}
	recordError(location)
	logger.FromContext(ctx).ErrorContext(ctx, "error reported", "location", location, "err", err, "attrs", attrs)
	forward(func(s Sink) { s.CaptureError(ctx, err, attrs) })
}

// CapturePanic forwards a recovered panic. It always bumps the
// pad_panics_total{location} counter and logs the stack, then forwards to the
// registered sink if any. location is a short call-site tag ("http", "scanner").
func CapturePanic(ctx context.Context, location string, recovered any, stack []byte, attrs Attrs) {
	recordPanic(location)
	logger.FromContext(ctx).ErrorContext(ctx, "panic reported",
		"location", location, "panic", recovered, "stack", string(stack), "attrs", attrs)
	forward(func(s Sink) { s.CapturePanic(ctx, recovered, stack, attrs) })
}

// SetInstanceID names this replica in the attributes of every event forwarded to the sink ("instance_id"), so a
// sink that groups the events of several replicas tells them apart. The log line needs nothing: the logger's base
// attributes put the id on every line. "" stops adding it. Intended to be called once at startup.
func SetInstanceID(id string) {
	mu.Lock()
	instanceID = id
	mu.Unlock()
}

func forward(do func(Sink)) {
	mu.RLock()
	s, id := active, instanceID
	mu.RUnlock()
	if s == nil {
		return
	}
	if id != "" {
		s = withInstance{Sink: s, id: id}
	}
	defer func() { _ = recover() }() // a sink must never break the caller's recovery
	do(s)
}

// withInstance adds the instance id to the attributes of the events it forwards, in a copy: call sites may share
// their map. A call site's own instance_id is kept.
type withInstance struct {
	Sink
	id string
}

func (w withInstance) CaptureError(ctx context.Context, err error, attrs Attrs) {
	w.Sink.CaptureError(ctx, err, w.attrs(attrs))
}

func (w withInstance) CapturePanic(ctx context.Context, recovered any, stack []byte, attrs Attrs) {
	w.Sink.CapturePanic(ctx, recovered, stack, w.attrs(attrs))
}

func (w withInstance) attrs(attrs Attrs) Attrs {
	out := make(Attrs, len(attrs)+1)
	out["instance_id"] = w.id
	maps.Copy(out, attrs)
	return out
}
