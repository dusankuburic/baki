package app

import (
	"context"
	"errors"
	"log/slog"

	"go.uber.org/fx"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/telemetry"
	"pad-core/logger"
)

// observability installs the logger and telemetry before anything else is built. fx runs a module's invokes before
// those of the application's own list, which builds the storage (the migrations), the audit trail and the router:
// their lines are JSON with the instance id from the first, and telemetry's hooks register first, so it starts
// before them and flushes after them (after the audit drain and the pool close).
var observability = fx.Module("observability",
	fx.Invoke(
		initLogger,
		initTelemetry,
	),
)

func initLogger(cfg *config.Config) {
	// This replica's name: on every log line, on the events forwarded to the error sink, in the job state's holder
	// column. Set before anything is built, so before any job runs.
	lifecycle.SetInstanceID(cfg.Runtime.InstanceID)
	errreport.SetInstanceID(cfg.Runtime.InstanceID)
	opts := logger.Options{
		Level: cfg.Log.Level,
		// A container's orchestrator collects stdout; elsewhere stdout is the desktop's CONFIG: handshake, and the
		// lines go to the log file and stderr.
		StdoutOnly: capability.Of(cfg).Profile() == capability.Cloud,
		BaseAttrs:  []slog.Attr{slog.String("instance_id", cfg.Runtime.InstanceID)},
	}
	var dirErr error
	if !opts.StdoutOnly {
		opts.AppDataDir, dirErr = storage.ConfigDir()
	}
	// Without the log file the logger is still installed, on stderr, at the configured level.
	if err := logger.InitWith(opts); err != nil {
		logger.Warn("log file unavailable; logging to stderr only", "error", errors.Join(dirErr, err))
	}
	// Forward recovered background-goroutine panics to the error-reporting
	// funnel so they surface in the exception metrics / sink, not only the log.
	logger.SetPanicHook(func(operation string, r any, stack []byte) {
		errreport.CapturePanic(context.Background(), operation, r, stack, errreport.Attrs{"operation": operation})
	})
	// logger.FromContext lines on the request path carry the request_id, user_id and trace_id keys of the
	// request's access-log line.
	logger.SetContextAttrs(middleware.LogAttrs)
}

// telemetryOptions names this process in every exported span. deployment.environment is the capability profile:
// the same "cloud" PAD_MODE gave the one deployment that exports traces, and it tells local+db from the desktop, which
// share the mode "local".
func telemetryOptions(cfg *config.Config, build Build) telemetry.Options {
	return telemetry.Options{
		ServiceName:  "baki-backend",
		Version:      build.Version,
		Environment:  string(capability.Of(cfg).Profile()),
		InstanceID:   cfg.Runtime.InstanceID,
		OTLPEndpoint: cfg.Runtime.OTLPEndpoint,
	}
}

func initTelemetry(lc fx.Lifecycle, cfg *config.Config, build Build) {
	var shutdown func()
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			otelShutdown, otelErr := telemetry.InitWith(ctx, telemetryOptions(cfg, build))
			if otelErr != nil {
				logger.Error("failed to initialize telemetry", "error", otelErr)
			} else {
				shutdown = otelShutdown
				// Forward captured panics/errors to the OTLP pipeline as
				// exception events — only when an exporter is configured,
				// else errreport stays metrics-only.
				if telemetry.TracingEnabled(cfg.Runtime.OTLPEndpoint) {
					errreport.Register(telemetry.NewOTelSink())
					logger.Info("errreport: OpenTelemetry exception sink registered")
				}
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if shutdown != nil {
				shutdown()
			}
			return nil
		},
	})
}
