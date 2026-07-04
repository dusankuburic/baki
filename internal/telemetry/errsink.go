package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"pad-analyzer/internal/errreport"
)

// OTelSink is an errreport.Sink that forwards captured errors and panics to
// OpenTelemetry as exception events, so an OTLP backend (Azure Application
// Insights, Jaeger, …) aggregates and groups them for crash triage. This closes
// the "errors live only in logs/metrics" gap without adding an external SDK: it
// reuses the tracer provider already installed by Init.
//
// When the ctx carries a live request span (the common HTTP-panic path), the
// exception is recorded on that span so it correlates with the surrounding
// trace; otherwise (background-goroutine panics, which arrive on a detached
// context) it starts a short standalone span.
type OTelSink struct {
	tracer trace.Tracer
}

// NewOTelSink builds a sink bound to the global tracer provider. Register it
// only when TracingEnabled is true — with no exporter the spans are dropped and
// forwarding is pointless overhead.
func NewOTelSink() *OTelSink {
	return &OTelSink{tracer: otel.Tracer("pad-analyzer/errreport")}
}

// CaptureError records a reported error as an exception on the request span, or
// on a standalone "exception" span when the context has none.
func (s *OTelSink) CaptureError(ctx context.Context, err error, attrs errreport.Attrs) {
	span, end := s.spanFor(ctx, "exception")
	defer end()
	span.SetAttributes(attrsToKV(attrs)...)
	span.RecordError(err, trace.WithStackTrace(true))
	span.SetStatus(codes.Error, err.Error())
}

// CapturePanic records a recovered panic as an exception event (with its stack)
// on the request span, or on a standalone "panic" span when none is present.
func (s *OTelSink) CapturePanic(ctx context.Context, recovered any, stack []byte, attrs errreport.Attrs) {
	msg := fmt.Sprint(recovered)
	span, end := s.spanFor(ctx, "panic")
	defer end()
	span.SetAttributes(attrsToKV(attrs)...)
	span.AddEvent("exception", trace.WithAttributes(
		attribute.String("exception.type", "panic"),
		attribute.String("exception.message", msg),
		attribute.String("exception.stacktrace", string(stack)),
	))
	span.SetStatus(codes.Error, msg)
}

// spanFor returns the recording span from ctx when there is one (so the
// exception attaches to the live trace), or a new standalone span. The returned
// end func only ends spans this sink created — it must not end a caller-owned
// request span.
func (s *OTelSink) spanFor(ctx context.Context, name string) (trace.Span, func()) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		return span, func() {}
	}
	_, span := s.tracer.Start(ctx, name)
	return span, func() { span.End() }
}

// attrsToKV converts the untyped errreport.Attrs into OTel key/values. Non-
// string values are rendered with %v so the sink never drops context.
func attrsToKV(attrs errreport.Attrs) []attribute.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	kv := make([]attribute.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		switch val := v.(type) {
		case string:
			kv = append(kv, attribute.String(k, val))
		case bool:
			kv = append(kv, attribute.Bool(k, val))
		case int:
			kv = append(kv, attribute.Int(k, val))
		case int64:
			kv = append(kv, attribute.Int64(k, val))
		case float64:
			kv = append(kv, attribute.Float64(k, val))
		default:
			kv = append(kv, attribute.String(k, fmt.Sprintf("%v", val)))
		}
	}
	return kv
}
