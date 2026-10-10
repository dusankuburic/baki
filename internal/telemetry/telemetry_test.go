package telemetry

import (
	"context"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

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

func resourceAttrs(t *testing.T, opts Options) map[attribute.Key]string {
	t.Helper()
	res, err := newResource(context.Background(), opts)
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	out := map[attribute.Key]string{}
	for _, kv := range res.Attributes() {
		out[kv.Key] = kv.Value.String()
	}
	return out
}

// TestResource_HasInstanceIDAndEnvironment (R15): spans from two replicas were indistinguishable (no
// service.instance.id), and deployment.environment came from os.Getenv("PAD_MODE"), empty when the mode was set in
// a PAD_CONFIG file or left at its default.
func TestResource_HasInstanceIDAndEnvironment(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("PAD_MODE", "")
	got := resourceAttrs(t, Options{ServiceName: "baki-backend", Version: "1.2.3", Environment: "cloud", InstanceID: "replica-7"})
	for key, want := range map[attribute.Key]string{
		semconv.ServiceNameKey:           "baki-backend",
		semconv.ServiceVersionKey:        "1.2.3",
		semconv.DeploymentEnvironmentKey: "cloud",
		semconv.ServiceInstanceIDKey:     "replica-7",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// Without an instance id from the caller the resource still names this process: the host name (the replica name
// in a container), else a random id. Two processes must never share one.
func TestResource_InstanceIDDefaults(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	got := resourceAttrs(t, Options{ServiceName: "baki-backend"})
	id := got[semconv.ServiceInstanceIDKey]
	if host, err := os.Hostname(); err == nil && host != "" {
		if id != host {
			t.Errorf("service.instance.id = %q, want the host name %q", id, host)
		}
	} else if id == "" {
		t.Error("service.instance.id is empty")
	}
	if got[semconv.ServiceNameKey] != "baki-backend" {
		t.Errorf("service.name = %q", got[semconv.ServiceNameKey])
	}
}

// The standard OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME override what the code sets, as the OpenTelemetry
// specification has it: an operator relabels a deployment without a release.
func TestResource_OTELEnvironmentWins(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=from-env,deployment.environment=staging,team=platform")
	t.Setenv("OTEL_SERVICE_NAME", "baki-staging")
	got := resourceAttrs(t, Options{ServiceName: "baki-backend", Environment: "cloud", InstanceID: "replica-7"})
	for key, want := range map[attribute.Key]string{
		semconv.ServiceNameKey:           "baki-staging",
		semconv.ServiceInstanceIDKey:     "from-env",
		semconv.DeploymentEnvironmentKey: "staging",
		"team":                           "platform",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

func TestInitWith_NoEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	shutdown, err := InitWith(context.Background(), Options{ServiceName: "test-service", Version: "0.0.1", Environment: "local", InstanceID: "test"})
	if err != nil {
		t.Fatalf("InitWith: %v", err)
	}
	shutdown()
}

// A malformed OTEL_RESOURCE_ATTRIBUTES entry costs that entry, not tracing.
func TestInitWith_MalformedResourceAttributesStillStarts(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "no-equals-sign,team=platform")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	shutdown, err := InitWith(context.Background(), Options{ServiceName: "test-service", InstanceID: "test"})
	if err != nil {
		t.Fatalf("InitWith: %v", err)
	}
	shutdown()
}

// The boot log line names the OTLP endpoint; a collector reached with credentials in its URL must not put them in
// the log.
func TestLoggableEndpoint_MasksCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"http://otel.service.k8se-apps:4317/":        "http://otel.service.k8se-apps:4317/",
		"localhost:4318":                             "localhost:4318",
		"10.0.0.7:4317":                              "10.0.0.7:4317",
		"https://user:Sentinel9@collector:4318/v1":   "https://user:xxxxx@collector:4318/v1",
		"https://Sentinel9@collector:4318":           "https://xxxxx@collector:4318",
		"https://user:Sentinel9@collector:43x8%zz/v": "[unparseable endpoint with credentials]",
	} {
		if got := loggableEndpoint(in); got != want {
			t.Errorf("loggableEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
