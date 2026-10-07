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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	// The runtime image is distroless: without the embedded zone database
	// time.LoadLocation knows only UTC, and the analytics tz parameter fails.
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"pad-analyzer/internal/api"
	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/di"
	"pad-analyzer/internal/errreport"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/mail"
	padmetrics "pad-analyzer/internal/metrics"
	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/platform/eventbus"
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
	// The parser mints one UUID per block and the apply-fix loop re-parses
	// per fix; batched randomness (still crypto-seeded) avoids a syscall per block.
	uuid.EnableRandPool()

	// `baki-backend healthcheck` GETs the server's own /readyz (exit 0 on 200)
	// so non-ACA deployments (compose, bare docker run) can probe without a
	// shell/wget — which the distroless image doesn't have. ACA uses platform
	// httpGet probes instead.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	fx.New(appOptions()...).Run()
}

// appOptions is the whole application graph: main runs it, TestAppGraph validates it without running anything.
func appOptions() []fx.Option {
	return []fx.Option{
		// fx.Run() handles SIGINT and SIGTERM. StopTimeout raised from 15s to
		// 25s: under ACA's 30s pod grace period, in-flight requests get to drain.
		fx.StopTimeout(25 * time.Second),
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
		fx.Invoke(
			initLogger,
			initTelemetry,
			initAuditTrail,
			initScanner,
			initRetentionPurge,
			initFieldEncryption,
			startServer,
			// After startServer: its stop hook cancels the backfill before the server's drains the runners.
			audit.ScheduleOrgBackfill,
		),
	}
}

// loadConfig returns an error instead of exiting on a fatal misconfiguration,
// so fx handles "can't start" through its provider-error path.
func loadConfig() (*config.Config, error) {
	var cfg *config.Config

	if path := os.Getenv("PAD_CONFIG"); path != "" {
		// Raw load: validation runs below, after env overrides are applied.
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

	if err := config.Validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	// Once, here: providers read the config and never fill it in later (the keystore used to pick up a
	// secret the auth manager minted, depending on construction order).
	config.ResolveRuntimeSecrets(cfg)

	return cfg, nil
}

func provideShutdownCh() chan struct{} {
	return make(chan struct{})
}

// egressPolicy is the deployment's single answer to "may an outbound
// notification reach this server's own machine or private network". Cloud is
// multi-tenant — an org's channel URL is untrusted input, so loopback and
// private targets are refused. Local/desktop is the single user's own machine,
// where a webhook pointing at localhost is exactly what they meant.
func egressPolicy(cfg *config.Config) notify.TargetPolicy {
	if cfg.Mode == config.ModeLocal {
		return notify.AllowLoopbackTargets
	}
	return notify.BlockInternalTargets
}

func provideNotifier(cfg *config.Config, mailer *mail.Service) (*notify.Dispatcher, error) {
	// Backward compatibility: the legacy PAD_WEBHOOK_URL sent Slack-format
	// payloads; map it onto the Slack channel when PAD_NOTIFY_SLACK_URL isn't set.
	slackURL := cfg.Governance.NotifySlackURL
	slackSecret := cfg.Governance.NotifySlackSecret
	if slackURL == "" {
		slackURL = os.Getenv("PAD_WEBHOOK_URL")
		slackSecret = os.Getenv("PAD_WEBHOOK_SECRET")
	}
	// The email channel only fires when both a real SMTP sender and a recipient
	// are configured; mailer is always non-nil (log-only fallback), so gate on
	// Enabled() + the recipient address.
	var emailSender notify.EmailSender
	if mailer != nil && cfg.Governance.NotifyEmailTo != "" {
		emailSender = mailSvcAdapter{svc: mailer}
	}
	d, err := notify.New(notify.Config{
		Policy:        egressPolicy(cfg),
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

// mailSvcAdapter bridges *mail.Service to notify.EmailSender, hiding the concrete
// type and honouring Enabled() (the log-only mailer reports disabled).
type mailSvcAdapter struct{ svc *mail.Service }

func (a mailSvcAdapter) Enabled() bool { return a.svc != nil && a.svc.Enabled() }

func (a mailSvcAdapter) SendAlert(ctx context.Context, to, subject, plainBody, htmlBody string) error {
	return a.svc.SendAlert(ctx, to, subject, plainBody, htmlBody)
}

// provideScanner wires the periodic flow scanner. A zero/invalid interval or a
// missing backend leaves it disabled (opt-in, cloud-only). The env-level
// notifier is optional: alerts always record in-app and route to each org's own
// channels; PAD_NOTIFY_* only adds a deployment-wide sink.
func provideScanner(cfg *config.Config, backend storageif.StorageBackend, analysisSvc *service.AnalysisService, notifier *notify.Dispatcher, eventMgr *api.EventManager, ruleProfiles *service.RuleProfileResolver) *scanner.Scanner {
	var analyze scanner.AnalyzeFunc
	if analysisSvc != nil {
		// Background, not AnalyzeFlow: the sweep's context carries no user, so
		// AnalyzeFlow's progress events went to EmitTo("") — a cross-tenant
		// broadcast of every scanned flow's progress.
		analyze = analysisSvc.AnalyzeFlowBackground
	}
	s := scanner.New(backend, analyze, notifier, scanInterval(cfg.Governance.ScanInterval), egressPolicy(cfg))
	// Real-time SSE push: a newly-detected alert pings connected clients who
	// can see the flow. EventManager is always non-nil in the fx graph; safe
	// even in local mode.
	s.SetEventNotifier(eventMgr)
	// Lets a sweep skip flows whose scan inputs are unchanged. The digest is
	// of the profile the AnalysisService itself analyzes with, so a rule
	// change is seen as a changed input.
	if ruleProfiles != nil {
		s.SetRuleSetDigest(func(ctx context.Context, orgID string) string {
			p := ruleProfiles.Resolve(ctx, orgID)
			return scanner.DigestRuleSet(p.Settings, p.Rules)
		})
	}
	return s
}

// scanInterval parses the configured scan interval; empty/invalid disables
// scanning (returns 0) rather than failing startup.
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

// initScanner starts the scanner on app start and stops it on shutdown; Start
// is a no-op when scanning is disabled.
func initScanner(lc fx.Lifecycle, s *scanner.Scanner) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { s.Start(); return nil },
		OnStop:  func(context.Context) error { s.Stop(); return nil },
	})
}

// provideMigrationRunner wires the optional local→cloud data migration runner:
// it needs BOTH a source filesystem backend (PAD_DATA_DIR at a legacy
// filesystem-backed server's data directory) and the active cloud (Postgres)
// destination. With either absent the runner is nil and the admin endpoint
// reports 503. Cloud mode only.
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
	// Cross-replica single-run guard: without the Postgres advisory lock two
	// pods could both pass the in-process guard and run the same migration.
	if locker, ok := dst.(api.GlobalLocker); ok {
		runner = runner.WithLocker(locker)
	}
	return runner
}

// provideScanNowFunc wires the admin "scan now" action to the scanner; only
// meaningful in cloud mode — in local mode it returns nil so the endpoint 503s.
func provideScanNowFunc(s *scanner.Scanner, cfg *config.Config) api.ScanNowFunc {
	if s == nil || cfg.Mode != config.ModeCloud {
		return nil
	}
	return s.ScanOnce
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
// audit rows (GDPR data-minimisation); disabled with no interval or backend.
// An immediate sweep runs on start so a fresh instance clears stale rows.
func initRetentionPurge(lc fx.Lifecycle, cfg *config.Config, backend storageif.StorageBackend) {
	interval := parseGovernanceDuration("PAD_RETENTION_PURGE_INTERVAL", cfg.Governance.RetentionPurgeInterval)
	// Cloud only: the filesystem backend's purge is a no-op, and with the
	// interval defaulting on, desktop would log a "purge complete" line of
	// zeros every day.
	if interval <= 0 || backend == nil || cfg.Mode != config.ModeCloud {
		return
	}
	// lifecycle.TickerLoop, like the scanner: its Stop cancels the in-flight
	// sweep AND waits (up to 5s) for it, so storage's OnStop — which runs after
	// this one — cannot close the pool under a sweep still deleting.
	var loop lifecycle.TickerLoop
	days := cfg.Governance.AuditRetentionDays
	sweep := func(ctx context.Context) { runRetentionSweep(ctx, backend, days) }
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			loop.Start(interval, true, sweep, func(r any) {
				logger.Error("retention purge loop stopped after a panic", "err", r)
			})
			return nil
		},
		OnStop: func(context.Context) error { loop.Stop(); return nil },
	})
}

// retentionLockKey namespaces the retention sweep's advisory lock: every replica
// must use the same key, and it must differ from the scanner's ("bakiscan") and
// the migration runner's ("bakimigr").
const retentionLockKey int64 = 0x62616b697265746e // "bakiretn"

// fieldEncryptionLockKey serializes the at-rest backfill of secret columns across replicas.
const fieldEncryptionLockKey int64 = 0x62616b6966656e63 // "bakifenc"

// fieldEncryptionBackfill runs the one-shot backfill off the boot path; shutdown drains it.
var fieldEncryptionBackfill = lifecycle.NewBounded("field_encryption_backfill", 1)

// initFieldEncryption encrypts secret columns at rest (org notification channel URLs and signing secrets) with the
// keystore's key and, once per boot across replicas, seals the rows written in plaintext before. It runs before
// the server starts, so no request of this process writes plaintext. Desktop mode has no server store.
func initFieldEncryption(lc fx.Lifecycle, cfg *config.Config, backend storageif.StorageBackend) error {
	fe, ok := backend.(storagedb.FieldEncryptor)
	if !ok {
		return nil
	}
	key := keystoreKey(cfg.Auth)
	if key == "" {
		logger.Warn("no PAD_ENCRYPTION_KEY or PAD_AUTH_SECRET: org notification channel URLs and secrets are stored unencrypted")
		return nil
	}
	if err := fe.EnableFieldEncryption([]byte(key)); err != nil {
		return fmt.Errorf("field encryption: %w", err)
	}
	lc.Append(fx.Hook{OnStart: func(context.Context) error {
		fieldEncryptionBackfill.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if locker, ok := backend.(api.GlobalLocker); ok {
				release, acquired, err := locker.TryGlobalLock(ctx, fieldEncryptionLockKey)
				if err != nil {
					logger.Warn("field encryption backfill skipped: lock unavailable", "error", err)
					return
				}
				if !acquired {
					return // another replica runs it
				}
				defer release()
			}
			n, err := fe.EncryptOrgChannelsAtRest(ctx)
			if err != nil {
				logger.Error("field encryption backfill failed; it resumes on the next boot", "sealed", n, "error", err)
				return
			}
			if n > 0 {
				logger.Info("field encryption backfill sealed plaintext org channels", "rows", n)
			}
		})
		return nil
	}})
	return nil
}

// retentionSweepTimeout bounds one whole sweep; each batched statement is
// still cut off by statement_timeout — this is only the ceiling on the sum.
const retentionSweepTimeout = 10 * time.Minute

// runRetentionSweep performs one retention purge. One sweep at a time ACROSS
// replicas (Postgres advisory lock; without it N replicas raced the same
// DELETEs; a backend without one sweeps as before). Recovers its own panics so
// a bad sweep is retried next tick, not the end of the loop.
func runRetentionSweep(ctx context.Context, backend storageif.StorageBackend, auditRetentionDays int) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("retention purge panicked", "err", r)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, retentionSweepTimeout)
	defer cancel()
	if locker, ok := backend.(api.GlobalLocker); ok {
		release, acquired, err := locker.TryGlobalLock(ctx, retentionLockKey)
		switch {
		case err != nil:
			logger.Warn("retention purge: advisory lock unavailable; sweeping without it", "err", err)
		case !acquired:
			logger.Debug("retention purge already running on another replica; skipping this tick")
			return
		default:
			defer release()
		}
	}
	res, err := backend.PurgeExpiredData(ctx, auditRetentionDays)
	if err != nil && res == nil {
		logger.Warn("retention purge failed", "err", err)
		return
	}
	if err != nil {
		// Partial: the purge continues past a table whose batches failed
		// (timed out) and reports what the other tables removed; the next
		// tick resumes. Log the counts, not just the failure.
		logger.Warn("retention purge incomplete", "err", err,
			"audit_events", res.AuditEvents, "flow_analysis_history", res.FlowAnalysisHistory,
			"usage_metrics", res.UsageMetrics, "token_blacklist", res.TokenBlacklist,
			"gov_alerts", res.GovAlerts, "idempotency_keys", res.IdempotencyKeys)
		padmetrics.RecordBackgroundLoopTick("retention_purge")
		return
	}
	logger.Info("retention purge complete",
		"refresh_tokens", res.RefreshTokens, "api_tokens", res.APITokens,
		"user_tokens", res.UserTokens, "org_invites", res.OrgInvites,
		"audit_events", res.AuditEvents,
		"flow_analysis_history", res.FlowAnalysisHistory,
		"usage_metrics", res.UsageMetrics, "token_blacklist", res.TokenBlacklist,
		"share_tokens", res.ShareTokens, "gov_alerts", res.GovAlerts,
		"idempotency_keys", res.IdempotencyKeys)
	padmetrics.RecordBackgroundLoopTick("retention_purge")
}

func provideAuthManager(lc fx.Lifecycle, cfg *config.Config, backend storageif.StorageBackend, redisClient *redis.Client) (*auth.Manager, error) {
	token := cfg.Auth.Secret
	if token == "" {
		// loadConfig resolves it (config.ResolveRuntimeSecrets); empty means a caller skipped that, so fail
		// closed rather than mint a secret nothing else knows about.
		return nil, fmt.Errorf("auth secret unresolved: run config.ResolveRuntimeSecrets before building the auth manager")
	}

	var blacklist auth.BlacklistStore
	if cfg.Auth.Enabled {
		if sqlBackend, ok := backend.(storagedb.SQLProvider); ok && sqlBackend != nil {
			// The Postgres table stays the shared source of truth; the cache
			// answers the per-request revocation check from a local copy
			// instead of a SELECT each. With the Redis backplane a revocation
			// fans out to the other replicas at once; the periodic reload is
			// the safety net. Until the first load, and whenever the
			// subscription is down, misses go to the table.
			opts := auth.CachedBlacklistOptions{}
			if redisClient != nil {
				ps := redisx.NewPubSub(redisClient)
				opts.Publisher, opts.Subscriber = ps, ps
			}
			cached := auth.NewCachedBlacklist(storagedb.NewPostgresBlacklist(sqlBackend.DB()), opts)
			blacklist = cached
			// Lifetime context for the cache's loops (fx's OnStart ctx only
			// bounds the start); OnStop cancels it then stops the cache, which
			// stops the Postgres store's own cleanup loop too.
			loopCtx, cancelLoops := context.WithCancel(context.Background())
			lc.Append(fx.Hook{
				OnStart: func(context.Context) error {
					cached.Start(loopCtx)
					return nil
				},
				OnStop: func(context.Context) error {
					cancelLoops()
					cached.Stop()
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

func provideOrgService(backend storageif.StorageBackend, bus *eventbus.Bus) *collaboration.OrgService {
	orgStore := collaboration.NewMemOrgStore()
	if s, ok := backend.(collaboration.OrgStore); ok {
		orgStore = s
	}
	return collaboration.NewOrgService(orgStore, collaboration.WithEventBus(bus))
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
	// funnel so they surface in the exception metrics / sink, not only the log.
	logger.SetPanicHook(func(operation string, r any, stack []byte) {
		errreport.CapturePanic(context.Background(), operation, r, stack, errreport.Attrs{"operation": operation})
	})
	// logger.FromContext lines on the request path carry the request_id, user_id and trace_id keys of the
	// request's access-log line.
	logger.SetContextAttrs(middleware.LogAttrs)
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

type StorageResult struct {
	fx.Out
	Backend storageif.StorageBackend
}

// vectorIndexBuildTimeout bounds the background HNSW build of the knowledge
// index (EnsureVectorIndex); a corpus that takes longer is left for the next
// boot to finish (the invalid leftover is dropped).
const vectorIndexBuildTimeout = 2 * time.Hour

func provideStorageBackend(lc fx.Lifecycle, cfg *config.Config) (StorageResult, error) {
	if cfg.Storage.Backend != config.StorageDatabase {
		return StorageResult{Backend: nil}, nil
	}

	// Build pool config, applying operator overrides from PAD_DB_* env vars.
	dbCfg := storagedb.DefaultConfig(cfg.Storage.DatabaseURL)
	// Resolve the TLS requirement: explicit PAD_DB_REQUIRE_SSL wins; otherwise
	// cloud mode requires TLS by default (credentials over plaintext is a
	// security bug), local leaves it optional for bundled/dev Postgres.
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
	if cfg.Storage.DBStatementTimeout != "" {
		d, err := time.ParseDuration(cfg.Storage.DBStatementTimeout)
		if err != nil {
			return StorageResult{}, fmt.Errorf("invalid PAD_DB_STATEMENT_TIMEOUT %q: %w", cfg.Storage.DBStatementTimeout, err)
		}
		dbCfg.StatementTimeout = d
	}

	// Apply Azure Blob Storage config: enabled when a container is set
	// together with an auth source — an account name (Managed Identity) or a
	// connection string (emulator / non-MI).
	if cfg.Storage.AzureStorageContainer != "" &&
		(cfg.Storage.AzureStorageAccount != "" || cfg.Storage.AzureBlobConnectionString != "") {
		dbCfg.AzureStorageAccount = cfg.Storage.AzureStorageAccount
		dbCfg.AzureStorageContainer = cfg.Storage.AzureStorageContainer
		dbCfg.AzureBlobConnectionString = cfg.Storage.AzureBlobConnectionString
	}
	// Knowledge-base embedding dimension; gates which chunks are
	// pgvector-searchable (default 1536 applied in the storage layer when 0).
	dbCfg.EmbeddingDim = cfg.Storage.EmbeddingDim

	pgBackend, err := storagedb.New(context.Background(), dbCfg)
	if err != nil {
		return StorageResult{}, fmt.Errorf("failed to connect to database: %w", err)
	}

	poolStop := make(chan struct{})
	// The knowledge HNSW index is built in the background after boot (CREATE
	// INDEX CONCURRENTLY cannot run inside the migration transaction, and a
	// large corpus takes minutes): detached from the start hook's context,
	// bounded by its own timeout, cancelled on stop. An interrupted build
	// leaves an invalid index the next boot drops and redoes.
	indexCtx, cancelIndexBuild := context.WithTimeout(context.Background(), vectorIndexBuildTimeout)
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
			go func() {
				defer cancelIndexBuild()
				if err := pgBackend.EnsureVectorIndex(indexCtx, pgBackend.EmbeddingDim()); err != nil {
					logger.Warn("knowledge vector index build did not finish", "error", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			cancelIndexBuild()
			close(poolStop)
			return pgBackend.Close()
		},
	})

	return StorageResult{Backend: pgBackend}, nil
}

// provideKeyStore builds the provider-API-key secret store: an encrypted,
// database-backed keystore in cloud+database mode (preferring a dedicated
// PAD_ENCRYPTION_KEY over PAD_AUTH_SECRET so rotating the JWT secret doesn't
// brick stored provider keys), falling back to the OS-keyring default
// everywhere else. A plain fx provider (not an Invoke mutating a global), so
// consumers get the same DAG-resolved value and failures surface as provider
// errors.
// keystoreKey picks the database keystore's encryption key: the dedicated PAD_ENCRYPTION_KEY, else an
// operator-provided auth secret (legacy fallback). A secret minted for this process never qualifies: provider
// keys encrypted with it would be unreadable after a restart.
func keystoreKey(a config.AuthConfig) string {
	if a.EncryptionKey != "" {
		return a.EncryptionKey
	}
	if a.SecretEphemeral {
		return ""
	}
	return a.Secret
}

func provideKeyStore(cfg *config.Config, backend storageif.StorageBackend) (service.KeyStore, error) {
	if cfg.Storage.Backend != config.StorageDatabase || backend == nil {
		return storage.NewKeyringSecretStore(), nil
	}
	ksBackend, ok := backend.(storagedb.KeyStoreProvider)
	if !ok {
		return storage.NewKeyringSecretStore(), nil
	}
	encKey := keystoreKey(cfg.Auth)
	switch {
	case encKey == "" && cfg.Auth.SecretEphemeral:
		logger.Warn("database storage without PAD_ENCRYPTION_KEY or PAD_AUTH_SECRET: provider keys stay in the OS keyring, " +
			"because a per-process secret would make them unreadable after a restart. Set PAD_ENCRYPTION_KEY to store them encrypted in the database.")
		return storage.NewKeyringSecretStore(), nil
	case encKey == "":
		return storage.NewKeyringSecretStore(), nil
	case cfg.Auth.EncryptionKey == "":
		logger.Warn("PAD_ENCRYPTION_KEY unset — falling back to PAD_AUTH_SECRET for keystore. " +
			"Set a dedicated PAD_ENCRYPTION_KEY so rotating the JWT secret doesn't brick stored provider keys.")
	}
	ks, err := ksBackend.NewEncryptedKeyStore([]byte(encKey))
	if err != nil {
		return nil, fmt.Errorf("failed to init encrypted keystore: %w", err)
	}
	logger.Info("encrypted database keystore enabled")
	return ks, nil
}

// initAuditTrail constructs the audit recorder early, so its pool starts before the hooks registered after it
// and drains after the HTTP server's stop hook (and the background work it drains), which still records.
func initAuditTrail(*audit.Recorder) {}

// startServer builds the full middleware stack (api.BuildHandler — see
// internal/api/middleware_chain.go for the layer order), binds a listener, and
// registers the fx lifecycle hooks that start/stop the HTTP server. Returns an
// error rather than calling os.Exit so fx surfaces boot failures.
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
		// explicitly configured port (PAD_PORT / config file) is honoured for
		// browser-based local development.
		listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Server.Port))
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}

		port := listener.Addr().(*net.TCPAddr).Port
		tokenPath, err := writeSessionSecret(cfg.Auth.Secret)
		if err != nil {
			// Writing the secret to a 0600 file is how the Tauri shell receives
			// it without the signing key landing in stdout/logs; if that fails
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
		// connections (SSE, WebSocket) use Hijack / Flusher and are unaffected;
		// only normal JSON endpoints benefit.
		ReadTimeout: 30 * time.Second,
		// WriteTimeout is deliberately 0 (unlimited) because SSE and
		// chat-stream responses are long-lived. SSE writes carry their own 30s
		// deadline (http.ResponseController, internal/api/events.go); WebSocket
		// writes set per-message deadlines on the hijacked connection.
		// Non-streaming routes are bounded by handler execution and Shutdown's
		// drain budget, not by this config.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		// MaxHeaderBytes caps total request header size to guard against
		// header-bomb DoS. Made explicit rather than relying on the 1 MB default.
		MaxHeaderBytes: 1 << 20,
	}

	warnIfMetricsExposed(cfg)

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
			// full shutdownCtx budget and drops in-flight collab state.
			// Budget 3s for the Close frame + pumps; the rest goes to non-WS.
			wsCtx, wsCancel := context.WithTimeout(ctx, 3*time.Second)
			if err := router.ShutdownWebSocket(wsCtx); err != nil {
				logger.Warn("websocket shutdown exceeded budget or was cancelled", "error", err)
			}
			wsCancel()
			// fx.StopTimeout (25s) bounds EVERY OnStop hook together, and this
			// hook runs first (reverse start order). 3s WS + 8s HTTP here
			// leaves 14s for everything else (background loops ≤5s each, audit
			// 5s, telemetry 5s, pool close), which covers the ordinary
			// slow-drain case; the absolute pathological stack still exceeds
			// ACA's 30s grace, so the platform SIGKILL remains the backstop. A
			// request still in flight after 8s is cut off — that beats losing
			// the audit tail and the clean pool close.
			shutdownCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			err := server.Shutdown(shutdownCtx)
			// Best-effort background work (webhooks, emails, alert fan-outs,
			// usage rows, admin jobs) started by the requests that just finished
			// gets the rest of the same 8s budget; any still running after it is
			// abandoned. Every lifecycle.Bounded runner is drained, not just the
			// ones a handler happens to expose.
			if derr := lifecycle.DrainAll(shutdownCtx); derr != nil {
				logger.Warn("background work still running at shutdown", "error", derr)
			}
			return err
		},
	})

	return nil
}

// warnIfMetricsExposed logs at boot when /metrics is reachable from the public
// internet. Behind a TLS-terminating reverse proxy (PAD_BEHIND_PROXY=true)
// RemoteAddr is the proxy's own private address for every request, so
// MetricsGuard's private-IP allowlist passes unconditionally, leaving
// PAD_METRICS_TOKEN as the only real gate. Warns rather than refusing: an
// in-cluster Prometheus scraper is indistinguishable by RemoteAddr from
// proxied public traffic, and /metrics carries operational data, not secrets.
// /debug/pprof/* is a different story and DOES fail closed; see
// config.validateProfilingConfig.
func warnIfMetricsExposed(cfg *config.Config) {
	if cfg.Server.BehindProxy && strings.TrimSpace(cfg.Server.MetricsToken) == "" {
		logger.Warn("/metrics is reachable through the reverse proxy: PAD_BEHIND_PROXY=true makes the private-IP allowlist pass for every request (RemoteAddr is the proxy). Set PAD_METRICS_TOKEN, and block /metrics at the ingress/NSG.")
	}
}

// writeSessionSecret persists the auto-generated JWT signing secret to a 0600
// file under the app config dir and returns its path. The Tauri shell reads
// the secret back from this path instead of the backend's stdout — emitting
// the signing key on stdout meant anyone with access to the process's logs
// could forge arbitrary JWTs. Local/desktop mode only.
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
// server's own /readyz, 0 on 200, 1 otherwise. Used by docker-compose and
// bare-docker operators because the distroless image has no shell/wget.
// Bounded by a short timeout so a hung server is reported unhealthy.
func runHealthcheck() int {
	port := os.Getenv("PAD_PORT")
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	// #nosec G704 -- not attacker-controlled: a fixed loopback host and a
	// constant path, with only the port coming from this process's own
	// PAD_PORT. This is the container healthcheck probing its own server.
	resp, err := client.Get("http://localhost:" + port + "/readyz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}
