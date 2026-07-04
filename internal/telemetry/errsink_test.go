package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"pad-analyzer/internal/errreport"
)

// OTelSink must satisfy the errreport.Sink contract.
var _ errreport.Sink = (*OTelSink)(nil)

// newRecordingSink installs an in-memory span recorder and returns a sink bound
// to it plus the recorder for assertions.
func newRecordingSink(t *testing.T) (*OTelSink, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return &OTelSink{tracer: tp.Tracer("test")}, sr
}

func TestOTelSink_CaptureError_StandaloneSpan(t *testing.T) {
	sink, sr := newRecordingSink(t)
	sink.CaptureError(context.Background(), errors.New("boom"), errreport.Attrs{"location": "chat"})

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 standalone span, got %d", len(ended))
	}
	span := ended[0]
	if span.Name() != "exception" {
		t.Errorf("span name = %q, want exception", span.Name())
	}
	if len(span.Events()) == 0 {
		t.Error("expected a recorded exception event")
	}
}

func TestOTelSink_CaptureError_UsesRequestSpan(t *testing.T) {
	sink, sr := newRecordingSink(t)
	// Simulate a live request span in context (the HTTP-panic path).
	tracer := otel.Tracer("req")
	ctx, reqSpan := tracer.Start(context.Background(), "GET /x")

	sink.CaptureError(ctx, errors.New("boom"), nil)

	// The sink must NOT end the caller's span; nothing ended yet.
	if got := len(sr.Ended()); got != 0 {
		t.Fatalf("sink ended %d spans, want 0 (must not end request span)", got)
	}
	reqSpan.End()
	ended := sr.Ended()
	if len(ended) != 1 || ended[0].Name() != "GET /x" {
		t.Fatalf("exception should attach to request span, got %+v", ended)
	}
	if len(ended[0].Events()) == 0 {
		t.Error("expected exception event on the request span")
	}
}

func TestOTelSink_CapturePanic(t *testing.T) {
	sink, sr := newRecordingSink(t)
	sink.CapturePanic(context.Background(), "nil deref", []byte("goroutine 1 ..."), errreport.Attrs{"operation": "scanner"})

	ended := sr.Ended()
	if len(ended) != 1 || ended[0].Name() != "panic" {
		t.Fatalf("expected one 'panic' span, got %+v", ended)
	}
	var found bool
	for _, e := range ended[0].Events() {
		if e.Name == "exception" {
			found = true
		}
	}
	if !found {
		t.Error("expected an 'exception' event on the panic span")
	}
}

func TestAttrsToKV(t *testing.T) {
	kv := attrsToKV(errreport.Attrs{
		"s": "str", "b": true, "i": 7, "i64": int64(9), "f": 1.5, "other": []int{1},
	})
	if len(kv) != 6 {
		t.Fatalf("expected 6 kv, got %d", len(kv))
	}
	byKey := map[attribute.Key]attribute.Value{}
	for _, p := range kv {
		byKey[p.Key] = p.Value
	}
	if byKey["s"].AsString() != "str" {
		t.Errorf("string attr = %q", byKey["s"].AsString())
	}
	if !byKey["b"].AsBool() {
		t.Error("bool attr not preserved")
	}
	if byKey["i"].AsInt64() != 7 {
		t.Errorf("int attr = %d", byKey["i"].AsInt64())
	}
	// Unhandled types fall back to a %v string so context is never dropped.
	if byKey["other"].Type() != attribute.STRING {
		t.Errorf("fallback type = %v, want STRING", byKey["other"].Type())
	}
}

func TestTracingEnabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if TracingEnabled("") {
		t.Error("expected disabled with no endpoint")
	}
	if !TracingEnabled("http://collector:4318") {
		t.Error("expected enabled with explicit endpoint")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://env:4318")
	if !TracingEnabled("") {
		t.Error("expected enabled from env fallback")
	}
}

// sanity: standalone spans carry a valid context.
func TestOTelSink_SpanContextValid(t *testing.T) {
	sink, _ := newRecordingSink(t)
	span, end := sink.spanFor(context.Background(), "x")
	defer end()
	if !span.SpanContext().IsValid() {
		t.Error("expected a valid standalone span context")
	}
	if sc := trace.SpanContextFromContext(context.Background()); sc.IsValid() {
		t.Error("empty context should have no valid span")
	}
}
