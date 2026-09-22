package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestInit_NoEndpoint(t *testing.T) {
	t.Parallel()

	shutdown, err := Init(context.Background(), "test-service", "0.0.1", "")
	if err != nil {
		t.Fatalf("expected no error with empty endpoint, got %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}
	shutdown()
}

func TestIsGRPCProtocol(t *testing.T) {
	t.Parallel()

	// The OTEL_EXPORTER_OTLP_PROTOCOL spec values, plus the forms Azure
	// Container Apps' managed agent injects. Anything unrecognised must fall
	// through to HTTP rather than guessing gRPC.
	cases := map[string]bool{
		"grpc":          true,
		"GRPC":          true,
		" grpc ":        true,
		"http/protobuf": false,
		"http/json":     false,
		"":              false,
		"nonsense":      false,
	}
	for in, want := range cases {
		if got := isGRPCProtocol(in); got != want {
			t.Errorf("isGRPCProtocol(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewTraceExporter_EndpointForms(t *testing.T) {
	// Not parallel: mutates OTEL_EXPORTER_OTLP_PROTOCOL.
	//
	// The regression this guards: otlptracehttp.WithEndpoint takes a bare
	// "host:port" and assumes TLS, so passing it the full URL that the ACA
	// managed agent injects ("http://otel.service.k8se-apps:4317/") made every
	// export fail. Each form below must construct an exporter cleanly; the
	// exporters connect lazily, so no collector is needed.
	cases := []struct {
		name     string
		protocol string
		endpoint string
	}{
		{"http bare host:port", "", "localhost:4318"},
		{"http url", "http/protobuf", "http://localhost:4318"},
		{"https url", "http/protobuf", "https://collector.example.com:4318"},
		{"grpc bare host:port", "grpc", "localhost:4317"},
		{"grpc url (ACA managed agent)", "grpc", "http://otel.service.k8se-apps:4317/"},
		{"grpc tls url", "grpc", "https://collector.example.com:4317"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", tc.protocol)
			exp, err := newTraceExporter(context.Background(), tc.endpoint)
			if err != nil {
				t.Fatalf("newTraceExporter(%q) with protocol %q: %v", tc.endpoint, tc.protocol, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = exp.Shutdown(ctx)
		})
	}
}
