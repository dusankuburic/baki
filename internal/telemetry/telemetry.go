package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
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
//     HTTP so existing OTLP/HTTP collectors keep working unchanged.
//
//  2. Endpoint form. WithEndpoint expects a bare "host:port" and assumes TLS;
//     handed a full URL it treats the whole string as a hostname and every
//     export fails DNS. The ACA agent injects a URL
//     ("http://otel.service.k8se-apps:4317/"), and so does anyone following the
//     OpenTelemetry spec for this variable. Use WithEndpointURL for anything
//     with a scheme, which also derives TLS-vs-plaintext from it.
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

// isGRPCProtocol reports whether the OTEL_EXPORTER_OTLP_PROTOCOL value selects
// gRPC. The spec's values are "grpc", "http/protobuf" and "http/json"; anything
// unrecognised (including empty) falls through to HTTP.
func isGRPCProtocol(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "grpc")
}

// Init initializes the OpenTelemetry SDK.
// It sets up an OTLP trace exporter when PAD_OTLP_ENDPOINT or the standard
// OTEL_EXPORTER_OTLP_ENDPOINT is set; on Azure Container Apps the managed
// OpenTelemetry agent injects the latter, so tracing turns on with no explicit
// configuration. Returns a shutdown function to be called on application exit.
func Init(ctx context.Context, serviceName, version, otlpEndpoint string) (func(), error) {
	// 1. Create Resource
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
			semconv.DeploymentEnvironment(os.Getenv("PAD_MODE")),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
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
	endpoint := resolveEndpoint(otlpEndpoint)
	if endpoint != "" {
		protocol := os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
		if protocol == "" {
			protocol = "http/protobuf"
		}
		slog.Info("telemetry: initializing OTLP exporter", "endpoint", endpoint, "protocol", protocol)
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
		tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
		)
	}

	otel.SetTracerProvider(tracerProvider)

	// 4. Runtime Metrics
	if err := runtime.Start(); err != nil {
		slog.Warn("telemetry: failed to start runtime metrics", "error", err)
	}

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracerProvider.Shutdown(ctx); err != nil {
			slog.Error("telemetry: failed to shutdown tracer provider", "error", err)
		}
	}

	return shutdown, nil
}
