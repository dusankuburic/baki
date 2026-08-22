// PAD Analyzer backend — local (Tauri sidecar) and cloud (multi-tenant) modes.
//
//	@title			PAD Analyzer API
//	@version		1.0
//	@description	Static analysis, auto-fix, and AI-assisted review for Power Automate Desktop flows.
//
//	@BasePath	/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"pad-analyzer/internal/api"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/connector/padcloud"
	"pad-analyzer/internal/di"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/mail"
	padmetrics "pad-analyzer/internal/metrics"
	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/scanner"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storagedb "pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/telemetry"
	"pad-core/logger"
)

var (
	Version   = "0.1.0"
	BuildDate = ""
	GitCommit = ""
)

func main() {
	// The parser mints one UUID per block and the apply-fix loop re-parses per
	// fix; batched randomness (still crypto-seeded) avoids a crypto/rand
	// syscall per block on large flows.
	uuid.EnableRandPool()

	// `baki-backend healthcheck` is an in-image liveness/readiness probe for
	// non-ACA deployments (docker-compose, bare `docker run`). It GETs the
	// server's own /readyz and exits 0 on 200, 1 otherwise. The prod target
	// (ACA) uses platform httpGet probes instead, so the Dockerfile ships no
	// HEALTHCHECK directive; this subcommand lets opt-in callers (compose) probe
	// without a shell/wget — which the distroless image doesn't have.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	fx.New(
		// fx.Run() handles both SIGINT and SIGTERM. The default StopTimeout is
		// 15s; raise to 25s so we stay under Azure Container Apps' 30s pod
		// grace period while giving in-flight requests time to drain.
		fx.StopTimeout(25*time.Second),
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
			provideIngester,
			providePadCloudAuth,
			provideScanNowFunc,
			provideIngestNowFunc,
			provideKeyStore,
		),
		di.ServiceModule,
		di.APIModule,
		redisx.Module,
		fx.Invoke(
			initLogger,
			initTelemetry,
			initAuditPool,
			initScanner,
			initIngester,
			initRetentionPurge,
			startServer,
		),
	).Run()
}

// loadConfig returns an error instead of exiting on a fatal misconfiguration,
// so fx handles the "can't start" case through its normal provider-error path
// (clean, testable, no lifecycle hooks left half-run) rather than the process
// exiting mid-construction.
func loadConfig() (*config.Config, error) {
	var cfg *config.Config

	if path := os.Getenv("PAD_CONFIG"); path != "" {
		// Raw load so Key Vault resolution can happen before validation.
		c, err := config.LoadRaw(path)
		if err != nil {
			return nil, fmt.Errorf("failed to load config from %s: %w", path, err)
		}
		cfg = c
	} else if os.Getenv("PAD_MODE") != "" || os.Getenv("PAD_PORT") != "" {
		c, err := config.LoadFromEnvRaw()
		if err != nil {
			return nil, fmt.Errorf("invalid env config: %w", err)
		}
		cfg = c
	} else {
		cfg = config.Default()
	}

	// Resolve secrets from Azure Key Vault BEFORE validation so that
	// secrets stored in Key Vault (pad-auth-secret, pad-database-url) can
	// satisfy validation requirements without being duplicated in env vars.
	if cfg.Server.KeyVaultURL != "" {
		if err := config.ResolveAzureSecrets(context.Background(), cfg); err != nil {
			// Non-fatal: warn and continue — secrets may already be in env vars.
			fmt.Fprintf(os.Stderr, "warning: Azure Key Vault resolution incomplete: %v\n", err)
		}
	}

	if err := config.Validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

func provideShutdownCh() chan struct{} {
	return make(chan struct{})
}

// provideNotifier builds the governance alert dispatcher from config. With no
// channels configured it is a harmless no-op (Dispatcher.Enabled() == false).
// A non-HTTPS alert URL is a configuration error (governance payloads carry
// internal flow details and must not be sent in plaintext) — fail startup so
// the operator fixes the URL rather than silently leaking alerts.
func provideNotifier(cfg *config.Config, mailer *mail.Service) (*notify.Dispatcher, error) {
	// Backward compatibility: the legacy PAD_WEBHOOK_URL (read directly by the
	// old service.WebhookNotifier) sent Slack-format payloads. Map it onto the
	// new Slack channel when PAD_NOTIFY_SLACK_URL isn't set, so existing
	// deployments keep posting to Slack without a config change.
	slackURL := cfg.Governance.NotifySlackURL
	slackSecret := cfg.Governance.NotifySlackSecret
	if slackURL == "" {
		slackURL = os.Getenv("PAD_WEBHOOK_URL")
		slackSecret = os.Getenv("PAD_WEBHOOK_SECRET")
	}
	// The email channel only fires when both a real SMTP sender and a recipient
	// are configured. mailer is always non-nil (log-only fallback), so gate on
	// its Enabled() + the recipient address.
	var emailSender notify.EmailSender
	if mailer != nil && cfg.Governance.NotifyEmailTo != "" {
		emailSender = mailSvcAdapter{svc: mailer}
	}
	d, err := notify.New(notify.Config{
		WebhookURL:    cfg.Governance.NotifyWebhookURL,
		WebhookSecret: cfg.Governance.NotifyWebhookSecret,
		TeamsURL:      cfg.Governance.NotifyTeamsURL,
		SlackURL:      slackURL,
		SlackSecret:   slackSecret,
		EmailSender:   emailSender,
		EmailTo:       cfg.Governance.NotifyEmailTo,
		JiraURL:       cfg.Governance.NotifyJiraBaseURL,
		JiraEmail:     cfg.Governance.NotifyJiraEmail,
		JiraToken:     cfg.Governance.NotifyJiraAPIToken,
		JiraProject:   cfg.Governance.NotifyJiraProject,
	})
	if err != nil {
		return nil, fmt.Errorf("notify: %w", err)
	}
	return d, nil
}

// mailSvcAdapter bridges *mail.Service to notify.EmailSender. It hides the
// concrete mail type from the notify package and honours Enabled() (the log-only
// mailer reports disabled, so the channel is skipped without SMTP).
type mailSvcAdapter struct{ svc *mail.Service }

func (a mailSvcAdapter) Enabled() bool { return a.svc != nil && a.svc.Enabled() }

func (a mailSvcAdapter) SendAlert(ctx context.Context, to, subject, plainBody, htmlBody string) error {
	return a.svc.SendAlert(ctx, to, subject, plainBody, htmlBody)
}

// provideScanner wires the periodic flow scanner. A zero/invalid interval or a
// missing backend/channel leaves it disabled, so it is opt-in and cloud-only.
func provideScanner(cfg *config.Config, backend storageif.StorageBackend, analysisSvc *service.AnalysisService, notifier *notify.Dispatcher, eventMgr *api.EventManager) *scanner.Scanner {
	var analyze scanner.AnalyzeFunc
	if analysisSvc != nil {
		analyze = analysisSvc.AnalyzeFlow
	}
	s := scanner.New(backend, analyze, notifier, scanInterval(cfg.Governance.ScanInterval))
	// Real-time SSE push: a newly-detected alert pings connected clients who
	// can see the flow so the bell updates instantly. EventManager is always
	// non-nil in the fx graph; safe even in local mode (no scanner runs there).
	s.SetEventNotifier(eventMgr)
	return s
}

// scanInterval parses the configured scan interval; an empty or invalid value
// disables scanning (returns 0) rather than failing startup.
func scanInterval(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid PAD_SCAN_INTERVAL %q; flow scanning disabled\n", s)
		return 0
	}
	return d
}

// initScanner starts the scanner on app start and stops it on shutdown. Start is
// a no-op when scanning is disabled.
func initScanner(lc fx.Lifecycle, s *scanner.Scanner) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { s.Start(); return nil },
		OnStop:  func(context.Context) error { s.Stop(); return nil },
	})
}

// provideMigrationRunner wires the optional local→cloud data migration runner.
// The migrator needs BOTH a source filesystem backend (PAD_STORAGE_DATA_DIR
// pointing at the old local data) and the active cloud (Postgres) destination.
// With either absent the runner is disabled — nil — and the admin endpoint
// reports 503. Cloud mode only: local mode has no destination backend.
func provideMigrationRunner(cfg *config.Config, dst storageif.StorageBackend) *api.MigrationRunner {
	if dst == nil || cfg.Mode != config.ModeCloud || cfg.Storage.DataDir == "" {
		return api.NewMigrationRunner(nil)
	}
	src, err := filesystem.NewLocalStorageBackend(cfg.Storage.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: migration source unavailable (%s): %v; migration disabled\n", cfg.Storage.DataDir, err)
		return api.NewMigrationRunner(nil)
	}
	m := migration.New(src, dst).
		WithConversationsDir(filepath.Join(cfg.Storage.DataDir, "conversations"))
	runner := api.NewMigrationRunner(m)
	// Cross-replica single-run guard: the Postgres backend exposes an advisory
	// lock; without it two pods could both pass the in-process guard and run
	// the same migration concurrently.
	if locker, ok := dst.(api.GlobalLocker); ok {
		runner = runner.WithLocker(locker)
	}
	return runner
}

// providePadCloudAuth builds the optional Power Platform device-flow
// authenticator. Returns nil when the connector isn't configured (cloud mode
// required + all of tenant/clientID/dataverseURL/ingestInterval). The same
// authenticator is shared between the ingester (for sweeps) and the admin
// endpoints (for the one-time device-flow bootstrap).
func providePadCloudAuth(cfg *config.Config, backend storageif.StorageBackend) *padcloud.Authenticator {
	pc := cfg.PowerPlatform
	if backend == nil || cfg.Mode != config.ModeCloud {
		return nil
	}
	if pc.TenantID == "" || pc.ClientID == "" || pc.DataverseURL == "" || pc.IngestInterval == "" {
		return nil
	}
	// DB-backed token store so the token (access + refresh) survives process
	// restarts. Without this, every restart loses auth and requires manual
	// re-approval of the device-code flow. Tokens are encrypted at rest with
	// the same key selection as the provider keystore (dedicated encryption
	// key, falling back to the auth secret).
	var store padcloud.TokenStore
	if sqlBackend, ok := backend.(storagedb.SQLProvider); ok {
		encKey := cfg.Auth.EncryptionKey
		if encKey == "" {
			encKey = cfg.Auth.Secret
		}
		s, err := padcloud.NewDBTokenStore(sqlBackend.DB(), []byte(encKey))
		if err != nil {
			// Refuse to persist tokens in plaintext — run the authenticator
			// in-memory only (no cross-restart persistence) instead.
			slog.Warn("padcloud: disabling token persistence — using in-memory auth", "error", err)
		} else {
			store = s
		}
	}
	return padcloud.NewAuthenticator(pc.TenantID, pc.ClientID, pc.Scope, nil, store)
}

// provideIngester wires the optional Power Platform (PAD-cloud) connector.
// EXPERIMENTAL: the cloud→FlowDocument converter and Dataverse endpoints are
// built defensively and are NOT yet validated against a real tenant — enable
// only for evaluation. It requires cloud mode, a DB backend, and the
// authenticator (providePadCloudAuth) to be configured; otherwise it returns
// nil (disabled) and the lifecycle hook is a no-op.
func provideIngester(auth *padcloud.Authenticator, cfg *config.Config, backend storageif.StorageBackend) *padcloud.Ingester {
	if auth == nil || backend == nil || cfg.Mode != config.ModeCloud {
		return nil
	}
	client := padcloud.NewHTTPClient(auth, cfg.PowerPlatform.DataverseURL)
	// An empty owner makes every ingested flow "unowned" — visible through the
	// admin/portfolio surfaces rather than scoped to a service account. Fine
	// for evaluation, but loud enough that nobody ships a tenant that way
	// without noticing.
	if cfg.PowerPlatform.OwnerUserID == "" {
		logger.Warn("padcloud: PAD_PP_OWNER_USER is not set — ingested flows will be unowned (broadly visible); point it at a service account before real-tenant use")
	}
	store := padcloud.NewLibraryStore(backend, cfg.PowerPlatform.OwnerUserID, cfg.PowerPlatform.OwnerOrgID)
	return padcloud.NewIngester(client, padcloud.NewCloudConverter(), store)
}

// initIngester starts/stops the periodic PAD-cloud ingest loop. No-op when the
// connector isn't configured (nil ingester) or the interval is invalid.
func initIngester(lc fx.Lifecycle, ing *padcloud.Ingester, auth *padcloud.Authenticator, cfg *config.Config) {
	if ing == nil {
		return
	}
	interval := parseGovernanceDuration("PAD_PP_INGEST_INTERVAL", cfg.PowerPlatform.IngestInterval)
	if interval <= 0 {
		return
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// Restore the persisted token so a restart doesn't lose auth and
			// require manual re-approval of the device-code flow.
			if auth != nil {
				if err := auth.LoadCachedToken(ctx); err != nil {
					logger.Warn("padcloud: failed to load cached token — manual re-auth may be needed", "error", err)
				}
			}
			logger.Info("starting Power Platform connector (experimental)", "interval", interval)
			ing.Start(interval)
			return nil
		},
		OnStop: func(context.Context) error { ing.Stop(); return nil },
	})
}

// provideScanNowFunc wires the admin "scan now" action to the scanner. The
// scanner is always constructed, but a manual trigger is only meaningful in
// cloud mode (governance is a cloud feature); in local mode it returns nil so
// the admin endpoint reports 503.
func provideScanNowFunc(s *scanner.Scanner, cfg *config.Config) api.ScanNowFunc {
	if s == nil || cfg.Mode != config.ModeCloud {
		return nil
	}
	return s.ScanOnce
}

// provideIngestNowFunc wires the admin "ingest now" action to the PAD-cloud
// ingester. Nil (→ 503) when the connector isn't configured. The wrapper logs
// per-pass errors so an async trigger is still observable.
func provideIngestNowFunc(ing *padcloud.Ingester) api.IngestNowFunc {
	if ing == nil {
		return nil
	}
	return func(ctx context.Context) {
		res, err := ing.Ingest(ctx)
		if err != nil {
			logger.Error("padcloud: manual ingest failed", "error", err)
			return
		}
		if res.Failed > 0 {
			logger.Warn("padcloud: manual ingest completed with failures",
				"ingested", res.Ingested, "failed", res.Failed, "skipped", res.Skipped)
		} else {
			logger.Info("padcloud: manual ingest completed",
				"ingested", res.Ingested, "skipped", res.Skipped)
		}
	}
}

// parseGovernanceDuration parses an optional duration string, returning 0
// (disabled) on empty/invalid with a stderr warning naming the env var.
func parseGovernanceDuration(env, val string) time.Duration {
	if val == "" {
		return 0
	}
	d, err := time.ParseDuration(val)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "warning: invalid %s %q; disabled\n", env, val)
		return 0
	}
	return d
}

// initRetentionPurge periodically purges expired tokens/invites and aged-out
// audit rows (GDPR data-minimisation). Disabled when no interval is set or there
// is no backend. An immediate sweep runs on start so a freshly-started instance
// clears stale rows right away.
func initRetentionPurge(lc fx.Lifecycle, cfg *config.Config, backend storageif.StorageBackend) {
	interval := parseGovernanceDuration("PAD_RETENTION_PURGE_INTERVAL", cfg.Governance.RetentionPurgeInterval)
	if interval <= 0 || backend == nil {
		return
	}
	stop := make(chan struct{})
	sweep := func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("retention purge panicked", "err", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := backend.PurgeExpiredData(ctx, cfg.Governance.AuditRetentionDays)
		if err != nil {
			logger.Warn("retention purge failed", "err", err)
			return
		}
		logger.Info("retention purge complete",
			"refresh_tokens", res.RefreshTokens, "api_tokens", res.APITokens,
			"user_tokens", res.UserTokens, "org_invites", res.OrgInvites,
			"audit_events", res.AuditEvents,
			"flow_analysis_history", res.FlowAnalysisHistory,
			"usage_metrics", res.UsageMetrics, "token_blacklist", res.TokenBlacklist)
		padmetrics.RecordBackgroundLoopTick("retention_purge")
	}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go sweep()
			go func() {
				t := time.NewTicker(interval)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
						sweep()
					}
				}
			}()
			return nil
		},
		OnStop: func(context.Context) error { close(stop); return nil },
	})
}

func provideAuthManager(lc fx.Lifecycle, cfg *config.Config, backend storageif.StorageBackend) (*auth.Manager, error) {
	token := cfg.Auth.Secret
	if token == "" {
		if cfg.Mode == config.ModeCloud && cfg.Auth.Enabled {
			return nil, fmt.Errorf("fatal: PAD_AUTH_SECRET must be set in cloud mode (auto-generation disabled)")
		}
		token = uuid.NewString()
		cfg.Auth.Secret = token
	}

	var blacklist auth.BlacklistStore
	if cfg.Auth.Enabled {
		if sqlBackend, ok := backend.(storagedb.SQLProvider); ok && sqlBackend != nil {
			pgBl := storagedb.NewPostgresBlacklist(sqlBackend.DB())
			blacklist = pgBl
			lc.Append(fx.Hook{
				OnStop: func(ctx context.Context) error {
					pgBl.Stop()
					return nil
				},
			})
		} else {
			memBl := auth.NewTokenBlacklist()
			blacklist = memBl
			lc.Append(fx.Hook{
				OnStop: func(ctx context.Context) error {
					memBl.Stop()
					return nil
				},
			})
		}
	}

	return auth.NewManager(token, blacklist), nil
}

func provideOrgService(backend storageif.StorageBackend) *collaboration.OrgService {
	orgStore := collaboration.NewMemOrgStore()
	if s, ok := backend.(collaboration.OrgStore); ok {
		orgStore = s
	}
	return collaboration.NewOrgService(orgStore)
}

func initLogger(cfg *config.Config) {
	_ = logger.InitWith(logger.Options{
		Level:      cfg.Log.Level,
		StdoutOnly: cfg.Mode == config.ModeCloud,
	})
	if cfg.Mode != config.ModeCloud {
		configDir, _ := storage.ConfigDir()
		_ = logger.Init(configDir, false)
	}
	// Forward recovered background-goroutine panics to the error-reporting
	// funnel so they surface in the exception metrics / sink alongside HTTP
	// panics, not only in the log.
	logger.SetPanicHook(func(operation string, r any, stack []byte) {
		errreport.CapturePanic(context.Background(), operation, r, stack, errreport.Attrs{"operation": operation})
	})
}

func initTelemetry(lc fx.Lifecycle, cfg *config.Config) {
	var shutdown func()
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			otelShutdown, otelErr := telemetry.Init(ctx, "baki-backend", Version, cfg.Runtime.OTLPEndpoint)
			if otelErr != nil {
				fmt.Fprintf(os.Stderr, "failed to initialize telemetry: %v\n", otelErr)
			} else {
				shutdown = otelShutdown
				// Forward captured panics/errors to the OTLP pipeline as
				// exception events (App Insights, etc.). Only when an exporter
				// is configured — otherwise errreport stays metrics-only.
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

type StorageResult struct {
	fx.Out
	Backend storageif.StorageBackend
}

func provideStorageBackend(lc fx.Lifecycle, cfg *config.Config) (StorageResult, error) {
	if cfg.Storage.Backend != config.StorageDatabase {
		return StorageResult{Backend: nil}, nil
	}

	// Build pool config, applying operator overrides from PAD_DB_* env vars.
	dbCfg := storagedb.DefaultConfig(cfg.Storage.DatabaseURL)
	// Resolve the TLS requirement: explicit PAD_DB_REQUIRE_SSL wins; otherwise
	// cloud mode requires TLS by default (credentials over plaintext is a
	// security bug), local mode leaves it optional for bundled/dev Postgres.
	switch strings.ToLower(cfg.Storage.DBRequireSSL) {
	case "true", "1", "yes":
		dbCfg.RequireSSL = true
	case "false", "0", "no":
		dbCfg.RequireSSL = false
	default:
		dbCfg.RequireSSL = cfg.Mode == config.ModeCloud
	}
	if cfg.Storage.DBMaxOpenConns > 0 {
		dbCfg.MaxOpenConns = cfg.Storage.DBMaxOpenConns
	}
	if cfg.Storage.DBMaxIdleConns > 0 {
		dbCfg.MaxIdleConns = cfg.Storage.DBMaxIdleConns
	}
	if cfg.Storage.DBConnMaxLifetime != "" {
		d, err := time.ParseDuration(cfg.Storage.DBConnMaxLifetime)
		if err != nil {
			return StorageResult{}, fmt.Errorf("invalid PAD_DB_CONN_MAX_LIFETIME %q: %w", cfg.Storage.DBConnMaxLifetime, err)
		}
		dbCfg.ConnMaxLifetime = d
	}
	if cfg.Storage.DBConnMaxIdleTime != "" {
		d, err := time.ParseDuration(cfg.Storage.DBConnMaxIdleTime)
		if err != nil {
			return StorageResult{}, fmt.Errorf("invalid PAD_DB_CONN_MAX_IDLE_TIME %q: %w", cfg.Storage.DBConnMaxIdleTime, err)
		}
		dbCfg.ConnMaxIdleTime = d
	}

	// Apply Azure Blob Storage config if available. Blob storage is enabled when a
	// container is set together with an auth source: an account name (Managed
	// Identity) or a connection string (emulator / non-MI).
	if cfg.Storage.AzureStorageContainer != "" &&
		(cfg.Storage.AzureStorageAccount != "" || cfg.Storage.AzureBlobConnectionString != "") {
		dbCfg.AzureStorageAccount = cfg.Storage.AzureStorageAccount
		dbCfg.AzureStorageContainer = cfg.Storage.AzureStorageContainer
		dbCfg.AzureBlobConnectionString = cfg.Storage.AzureBlobConnectionString
	}
	// Knowledge-base embedding dimension. Gates which chunks are pgvector-
	// searchable (default 1536 applied in the storage layer when 0).
	dbCfg.EmbeddingDim = cfg.Storage.EmbeddingDim

	pgBackend, err := storagedb.New(context.Background(), dbCfg)
	if err != nil {
		return StorageResult{}, fmt.Errorf("failed to connect to database: %w", err)
	}

	poolStop := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			go func() {
				t := time.NewTicker(15 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-poolStop:
						return
					case <-t.C:
						padmetrics.ObservePostgresPool(pgBackend.DB())
					}
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			close(poolStop)
			return pgBackend.Close()
		},
	})

	return StorageResult{Backend: pgBackend}, nil
}

// provideKeyStore builds the provider-API-key secret store: an encrypted,
// database-backed keystore in cloud+database mode (preferring a dedicated
// PAD_ENCRYPTION_KEY over PAD_AUTH_SECRET so rotating the JWT secret doesn't
// brick stored provider keys), falling back to the OS-keychain default
// everywhere else — including cloud+database mode with no encryption key
// configured, matching the historical behavior.
//
// This is a plain fx provider (not an Invoke that mutates a package-level
// global) so cloud-mode secret storage no longer depends on fx *invoke*
// ordering: every consumer gets the same DAG-resolved value regardless of
// when it's first needed, and a construction failure surfaces as a normal fx
// provider error instead of an inline os.Exit.
func provideKeyStore(cfg *config.Config, backend storageif.StorageBackend) (service.KeyStore, error) {
	if cfg.Storage.Backend != config.StorageDatabase || backend == nil {
		return storage.NewKeyringSecretStore(), nil
	}
	ksBackend, ok := backend.(storagedb.KeyStoreProvider)
	if !ok {
		return storage.NewKeyringSecretStore(), nil
	}
	encKey := cfg.Auth.EncryptionKey
	if encKey == "" {
		if cfg.Auth.Secret != "" {
			encKey = cfg.Auth.Secret
			logger.Warn("PAD_ENCRYPTION_KEY unset — falling back to PAD_AUTH_SECRET for keystore. " +
				"Set a dedicated PAD_ENCRYPTION_KEY so rotating the JWT secret doesn't brick stored provider keys.")
		}
	}
	if encKey == "" {
		return storage.NewKeyringSecretStore(), nil
	}
	ks, err := ksBackend.NewEncryptedKeyStore([]byte(encKey))
	if err != nil {
		return nil, fmt.Errorf("failed to init encrypted keystore: %w", err)
	}
	logger.Info("encrypted database keystore enabled")
	return ks, nil
}

func initAuditPool(lc fx.Lifecycle, cfg *config.Config, backend storageif.StorageBackend) {
	// Enable the on-disk spill queue unless explicitly disabled. Lets a burst
	// that overflows the in-memory pool be replayed instead of diverted to logs.
	if cfg.Governance.AuditSpillDir != "off" {
		if err := api.SetAuditSpillConfig(cfg.Governance.AuditSpillDir, 0); err != nil {
			slog.Warn("audit spill queue disabled (using log fallback only)", "error", err)
		}
	}
	api.InitAuditPool(backend)
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			api.ShutdownAuditPool()
			return nil
		},
	})
}

// startServer builds the full request-handling middleware stack (via
// api.BuildHandler — see internal/api/middleware_chain.go for the complete,
// resolved layer order), binds a listener, and registers the fx lifecycle
// hooks that start/stop the HTTP server.
//
// Returns an error rather than calling os.Exit so fx can surface boot failures
// through its normal error path (which flushes deferred log/telemetry and lets
// tests assert against boot failures without killing the test process).
func startServer(lc fx.Lifecycle, cfg *config.Config, router *api.Router, chatSvc *service.ChatService, redisClient *redis.Client) error {
	handler, rateLimiters := api.BuildHandler(router, cfg, redisClient)

	var listener net.Listener
	var err error

	if cfg.Mode == config.ModeCloud {
		addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", addr, err)
		}
	} else {
		// Local mode binds loopback only. The port is normally 0 (ephemeral,
		// reported to the Tauri shell via the CONFIG line below), but an
		// explicitly configured port (PAD_PORT / config file) is honoured so
		// browser-based local development can use a stable address.
		listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Server.Port))
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}

		port := listener.Addr().(*net.TCPAddr).Port
		tokenPath, err := writeSessionSecret(cfg.Auth.Secret)
		if err != nil {
			// Writing the secret to a 0600 file is how the Tauri shell receives
			// it without the signing key landing in stdout/logs. If that fails
			// we cannot safely hand off the credential, so fail closed.
			return fmt.Errorf("persist session secret: %w", err)
		}
		startupInfo := map[string]any{
			"port":      port,
			"tokenPath": tokenPath,
		}
		infoJSON, _ := json.Marshal(startupInfo)
		fmt.Printf("CONFIG:%s\n", string(infoJSON))
	}

	server := &http.Server{
		Handler: handler,
		// ReadHeaderTimeout guards against Slowloris (headers dribbled slowly).
		ReadHeaderTimeout: 5 * time.Second,
		// ReadTimeout covers the full request body read. Long-lived streaming
		// connections (SSE, WebSocket) use http.Hijack / http.Flusher and are
		// not affected by this deadline; only normal JSON endpoints benefit.
		ReadTimeout: 30 * time.Second,
		// WriteTimeout is deliberately 0 (unlimited) because SSE and chat-stream
		// responses are long-lived. Per-handler write deadlines are set via
		// http.ResponseController.SetWriteDeadline for non-streaming routes.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		// MaxHeaderBytes caps total request header size to guard against
		// header-bomb DoS. Made explicit rather than relying on the 1 MB default.
		MaxHeaderBytes: 1 << 20,
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			go func() {
				useTLS := cfg.Server.TLSCert != "" && cfg.Server.TLSKey != ""
				scheme := "http"
				if useTLS {
					scheme = "https"
				}
				logger.Info("backend server starting",
					"addr", listener.Addr().String(),
					"mode", cfg.Mode,
					"scheme", scheme,
				)

				var serveErr error
				if useTLS {
					serveErr = server.ServeTLS(listener, cfg.Server.TLSCert, cfg.Server.TLSKey)
				} else {
					serveErr = server.Serve(listener)
				}
				if serveErr != nil && serveErr != http.ErrServerClosed {
					logger.Error("server failed", "error", serveErr)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			logger.Info("shutting down")
			router.Shutdown()
			chatSvc.CancelAll()
			for _, rl := range rateLimiters {
				rl.Stop()
			}
			// Gracefully drain WebSocket clients FIRST. http.Server.Shutdown
			// does not close hijacked (WebSocket) sockets, so without this
			// every rolling restart with even one live WS client takes the
			// full shutdownCtx budget and drops in-flight collab state
			// silently. Budget: 5s — enough for clients to receive the Close
			// frame + pumps to exit; the remainder of shutdownCtx goes to
			// server.Shutdown for non-WS requests.
			wsCtx, wsCancel := context.WithTimeout(ctx, 5*time.Second)
			if err := router.ShutdownWebSocket(wsCtx); err != nil {
				logger.Warn("websocket shutdown exceeded budget or was cancelled", "error", err)
			}
			wsCancel()
			// fx.StopTimeout (25s) bounds the parent context. Reserve a small
			// budget for the goroutines above to finish their teardown, then
			// give server.Shutdown the remainder. If ctx already has a short
			// deadline (rapid restart loop) we still respect it.
			shutdownCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			return server.Shutdown(shutdownCtx)
		},
	})

	return nil
}

// writeSessionSecret persists the auto-generated JWT signing secret to a 0600
// file under the app config dir and returns its path. The Tauri shell reads the
// secret back from this path instead of receiving it on the backend's stdout —
// emitting the signing key on stdout meant anyone with access to the backend
// process's stdout (container logs, systemd journal, the parent shell's
// scrollback, or the Tauri app's own stdout mirror log) could forge arbitrary
// JWTs. Writing to a permission-restricted file confines the secret to the
// filesystem ACL instead of the process's log streams.
//
// Used only in local/desktop mode (cloud mode requires an explicit
// PAD_AUTH_SECRET and never reaches the stdout CONFIG handshake).
func writeSessionSecret(secret string) (string, error) {
	configDir, err := storage.ConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	path := filepath.Join(configDir, "session.key")
	// Write via a temp file + rename so a crash mid-write can't leave a partial
	// secret that the shell would fail to parse.
	tmp, err := os.CreateTemp(configDir, ".session-key-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp session key: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("chmod temp session key: %w", err)
	}
	if _, err := tmp.Write([]byte(secret)); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("write session key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("close temp session key: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return "", fmt.Errorf("rename session key: %w", err)
	}
	return path, nil
}

// runHealthcheck implements the `baki-backend healthcheck` subcommand: GET the
// server's own /readyz and return 0 on 200, 1 otherwise. Used by docker-compose
// (and bare-docker operators) because the distroless image has no shell/wget.
// Bounded by a short timeout so a hung server is reported unhealthy, not hung.
func runHealthcheck() int {
	port := os.Getenv("PAD_PORT")
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://localhost:" + port + "/readyz") // #nosec G114 -- short-timeout local probe of a known endpoint
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}
