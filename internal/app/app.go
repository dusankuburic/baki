// Package app is the backend's fx application: the providers, the hooks and the ordered list of invokes that main.go
// runs. main.go keeps the build stamps, the healthcheck subcommand and the swag general-API annotations.
//
// One file per concern: config, observability (logger, telemetry), storage, keystore (and field encryption), auth,
// governance (notifier, scanner, migration runner), the background jobs (retention, backfill, re-seal) and the HTTP
// server with its shutdown. docs/architecture/di-wiring.md maps them.
package app

import (
	"context"
	"time"

	"go.uber.org/fx"

	"pad-analyzer/internal/api"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/di"
	"pad-analyzer/internal/invalidation"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/service"
)

// Build is what the linker stamps into package main (the Dockerfile's -X main.Version and -X main.GitCommit).
type Build struct {
	Version string
	// Commit is stamped for symmetry; nothing reads it yet.
	Commit string
}

// Options is the whole application graph: main runs it, TestAppGraph validates it without running anything.
func Options(build Build) []fx.Option {
	return []fx.Option{
		// fx.Run() handles SIGINT and SIGTERM. StopTimeout bounds the whole stop, under the platform's grace period
		// (docs/architecture/operations.md); at it fx skips the hooks not yet run, so ADR 0024's budget table fits
		// the stop inside it. Change them together.
		fx.StopTimeout(28 * time.Second),
		// The stamps stay main's (ldflags -X needs a package-level var there); telemetry reports the version.
		fx.Supply(build),
		// The logger and telemetry, installed before anything the lists below build (observability.go).
		observability,
		fx.Provide(
			func() context.Context { return context.Background() },
			loadConfig,
			func(cfg *config.Config) config.DeploymentMode { return cfg.Mode },
			provideStorageBackend,
			provideShutdownCh,
			func(m *api.EventManager) service.EventNotifier { return m },
			provideAuthManager,
			provideOrgService,
			provideNotifier,
			provideScanner,
			provideMigrationRunner,
			provideScanNowFunc,
			provideKeyStore,
			audit.Provide,
		),
		di.ServiceModule,
		di.APIModule,
		redisx.Module,
		// After the services and API modules, whose appliers it delivers to; before startServer, so it stops after it.
		invalidation.Module,
		fx.Invoke(
			initAuditTrail,
			initRetentionPurge,
			initFieldEncryption,
			startServer,
			// After startServer, so their stop hooks run before the server's drains the lifecycle runners: the
			// scanner's sweep hands its org-channel deliveries to the "notify" runner and waits for them, and the
			// backfill and the re-seal pass are cancelled. Before it, a sweep running at shutdown fed a closed runner and
			// its alerts, already recorded, were never delivered.
			initScanner,
			audit.ScheduleOrgBackfill,
			scheduleFieldEncryptionBackfill,
			scheduleSecretReseal,
			// Last, so its stop hook runs first: the server drains, and the chat streams end over the event streams
			// still open, before anything else stops (ADR 0024).
			registerDrain,
		),
	}
}

// initAuditTrail constructs the audit recorder early, so its pool starts before the hooks registered after it
// and drains after the HTTP server's stop hook (and the background work it drains), which still records.
func initAuditTrail(*audit.Recorder) {}
