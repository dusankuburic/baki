package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestAccessLog_CarriesTraceID: the access line and the OTel trace shared no
// key, so a trace could not be pivoted to its log lines. otelhttp (outermost)
// puts the span on the context; the access log must print its trace id.
func TestAccessLog_CarriesTraceID(t *testing.T) {
	buf := captureSlog(t)
	handler := AccessLog(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil).WithContext(trace.ContextWithSpanContext(context.Background(), sc))
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(buf.String(), "trace_id=4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("access line carries no trace_id:\n%s", buf.String())
	}

	// No span on the context → no trace_id key (not an empty one).
	buf.Reset()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/y", nil))
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("trace_id must be omitted when there is no span:\n%s", buf.String())
	}
}
