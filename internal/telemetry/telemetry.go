package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
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

// Init initializes the OpenTelemetry SDK.
// It sets up the OTLP exporter if APPLICATIONINSIGHTS_CONNECTION_STRING or
// OTEL_EXPORTER_OTLP_ENDPOINT is provided.
// Returns a shutdown function to be called on application exit.
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
	// We check for OTEL_EXPORTER_OTLP_ENDPOINT (standard)
	// Azure Application Insights can be configured to receive OTLP via this endpoint.
	endpoint := resolveEndpoint(otlpEndpoint)
	if endpoint != "" {
		slog.Info("telemetry: initializing OTLP exporter", "endpoint", endpoint)
		exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(endpoint))
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
