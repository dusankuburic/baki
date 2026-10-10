package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// resolveEndpoint returns the OTLP endpoint to use, falling back from the
// explicitly-configured value to the standard OTEL_EXPORTER_OTLP_ENDPOINT env
// var. An empty result means tracing/export is disabled.
func resolveEndpoint(otlpEndpoint string) string {
	if otlpEndpoint != "" {
		return otlpEndpoint
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
}

// TracingEnabled reports whether an OTLP exporter is configured (so spans are
// actually exported). Used to decide whether to register the exception sink:
// with no exporter, forwarding exceptions to spans would be dropped anyway, so
// errreport stays in its metrics-only default.
func TracingEnabled(otlpEndpoint string) bool {
	return resolveEndpoint(otlpEndpoint) != ""
}

// newTraceExporter builds the OTLP trace exporter for the configured endpoint.
//
// Two things the previous single-line otlptracehttp.New(WithEndpoint(...)) got
// wrong, both of which matter on Azure Container Apps:
//
//  1. Protocol. WithEndpoint always speaks OTLP/HTTP, but the ACA managed
//     OpenTelemetry agent only accepts gRPC and advertises that by injecting
//     OTEL_EXPORTER_OTLP_PROTOCOL=grpc alongside its endpoint. Honour the
//     standard variable ("grpc", "http/protobuf", "http/json") and default to
//     HTTP so existing OTLP/HTTP collectors keep working.
//
//  2. Endpoint form. WithEndpoint expects a bare "host:port" and assumes TLS;
//     handed a full URL it treats the whole string as a hostname and every
//     export fails DNS. The ACA agent injects a URL
//     ("http://otel.service.k8se-apps:4317/"), and so does anyone following
//     the OpenTelemetry spec. Use WithEndpointURL for anything with a scheme,
//     which also derives TLS-vs-plaintext from it.
func newTraceExporter(ctx context.Context, endpoint string) (*otlptrace.Exporter, error) {
	hasScheme := strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://")
	// A bare "host:port" with no scheme keeps the historical meaning: TLS on.
	insecure := false
	if hasScheme {
		if u, err := url.Parse(endpoint); err == nil && u.Scheme == "http" {
			insecure = true
		}
	}

	if isGRPCProtocol(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")) {
		opts := []otlptracegrpc.Option{}
		if hasScheme {
			opts = append(opts, otlptracegrpc.WithEndpointURL(endpoint))
		} else {
			opts = append(opts, otlptracegrpc.WithEndpoint(endpoint))
		}
		if insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	}

	opts := []otlptracehttp.Option{}
	if hasScheme {
		opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
	} else {
		opts = append(opts, otlptracehttp.WithEndpoint(endpoint))
	}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, opts...)
}

// loggableEndpoint is endpoint as the boot log line shows it: credentials in it
// (a collector behind basic auth, a token as the user name) masked.
func loggableEndpoint(endpoint string) string {
	if !strings.Contains(endpoint, "@") {
		return endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "[unparseable endpoint with credentials]"
	}
	if u.User == nil {
		return endpoint
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		u.User = url.User("xxxxx")
	}
	return u.Redacted()
}

// isGRPCProtocol reports whether the OTEL_EXPORTER_OTLP_PROTOCOL value selects
// gRPC. The spec's values are "grpc", "http/protobuf" and "http/json"; anything
// unrecognised (including empty) falls through to HTTP.
func isGRPCProtocol(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "grpc")
}

// Options configures InitWith. The resource attributes name this process in every span it exports.
type Options struct {
	// ServiceName is service.name. OTEL_SERVICE_NAME overrides it.
	ServiceName string
	// Version is service.version, the build's version stamp.
	Version string
	// Environment is deployment.environment: the deployment profile of the loaded configuration (cloud, local+db,
	// desktop), whether it came from PAD_MODE, a PAD_CONFIG file or the default.
	Environment string
	// InstanceID is service.instance.id, this replica's id from the configuration. Empty falls back to the host
	// name (the replica name in a container), else a random id, so two replicas' spans never share one.
	InstanceID string
	// OTLPEndpoint is PAD_OTLP_ENDPOINT; empty falls back to OTEL_EXPORTER_OTLP_ENDPOINT, and with neither tracing
	// is off.
	OTLPEndpoint string
}

// newResource builds the resource of every exported span. The standard OTEL_RESOURCE_ATTRIBUTES and
// OTEL_SERVICE_NAME are applied last, so they override the attributes set here, as the OpenTelemetry specification
// has it: an operator relabels a deployment without a release.
func newResource(ctx context.Context, opts Options) (*resource.Resource, error) {
	instance := opts.InstanceID
	if instance == "" {
		instance = defaultInstanceID()
	}
	attrs := []attribute.KeyValue{semconv.ServiceName(opts.ServiceName), semconv.ServiceInstanceID(instance)}
	if opts.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(opts.Version))
	}
	if opts.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironment(opts.Environment))
	}
	return resource.New(ctx, resource.WithAttributes(attrs...), resource.WithFromEnv())
}

// defaultInstanceID is the host name, which a container runtime sets to the replica (pod) name, else a random id.
func defaultInstanceID() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return uuid.NewString()
}

// InitWith initializes the OpenTelemetry SDK.
// It sets up an OTLP trace exporter when PAD_OTLP_ENDPOINT or the standard
// OTEL_EXPORTER_OTLP_ENDPOINT is set; on Azure Container Apps the managed
// OpenTelemetry agent injects the latter, so tracing turns on with no explicit
// configuration. Returns a shutdown function to be called on application exit.
//
// No metrics go through OpenTelemetry: nothing sets a MeterProvider (App
// Insights takes no OTLP metrics), so runtime instrumentation would feed the
// no-op provider. Go runtime metrics are on /metrics (internal/metrics).
func InitWith(ctx context.Context, opts Options) (func(), error) {
	// 1. Create Resource. A malformed OTEL_RESOURCE_ATTRIBUTES entry is
	// skipped, not fatal: tracing still starts with the rest.
	res, err := newResource(ctx, opts)
	if err != nil {
		if !errors.Is(err, resource.ErrPartialResource) {
			return nil, fmt.Errorf("failed to create resource: %w", err)
		}
		slog.Warn("telemetry: some resource attributes were not applied", "error", err)
	}

	// 2. Setup Propagator (W3C TraceContext)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	var tracerProvider *sdktrace.TracerProvider

	// 3. Setup Exporter
	// PAD_OTLP_ENDPOINT, else the standard OTEL_EXPORTER_OTLP_ENDPOINT — which
	// is what the ACA managed OpenTelemetry agent injects when the environment
	// forwards traces to Application Insights (see infra/main.bicep).
	endpoint := resolveEndpoint(opts.OTLPEndpoint)
	if endpoint != "" {
		protocol := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
		if protocol == "" {
			protocol = "http/protobuf"
		}
		slog.Info("telemetry: initializing OTLP exporter", "endpoint", loggableEndpoint(endpoint), "protocol", protocol)
		exporter, err := newTraceExporter(ctx, endpoint)
		if err != nil {
			return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
		}

		tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res),
		)
	} else {
		slog.Warn("telemetry: no OTLP endpoint configured, tracing will be disabled")
		// No exporter means nothing consumes spans, but otelhttp still asks
		// the provider for a tracer per request — with the default
		// ParentBased(AlwaysSample) sampler it gets a real RECORDING span
		// started, scoped and ended for pure overhead. NeverSample makes
		// those no-ops.
		tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.NeverSample()),
		)
	}

	otel.SetTracerProvider(tracerProvider)

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracerProvider.Shutdown(ctx); err != nil {
			slog.Error("telemetry: failed to shutdown tracer provider", "error", err)
		}
	}

	return shutdown, nil
}
