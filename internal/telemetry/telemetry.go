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
	endpoint := otlpEndpoint
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
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
