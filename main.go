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

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/fx"

	"pad-analyzer/internal/api"
	apimw "pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/connector/padcloud"
	"pad-analyzer/internal/di"
	"pad-analyzer/internal/errreport"
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
			func(m *api.EventManager) service.Notifier { return m },
			provideAuthManager,
			provideOrgService,
			provideNotifier,
			provideScanner,
			provideMigrationRunner,
			provideIngester,
			providePadCloudAuth,
		),
		di.ServiceModule,
		di.APIModule,
		redisx.Module,
		fx.Invoke(
			initLogger,
			initTelemetry,
			initStorageSecrets,
			initAuditPool,
			initScanner,
			initIngester,
			initRetentionPurge,
			startServer,
		),
	).Run()
}

func loadConfig() *config.Config {
	var cfg *config.Config

	if path := os.Getenv("PAD_CONFIG"); path != "" {
		// Raw load so Key Vault resolution can happen before validation.
		c, err := config.LoadRaw(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load config from %s: %v\n", path, err)
			os.Exit(1)
		}
		cfg = c
	} else if os.Getenv("PAD_MODE") != "" || os.Getenv("PAD_PORT") != "" {
		c, err := config.LoadFromEnvRaw()
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid env config: %v\n", err)
			os.Exit(1)
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
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(1)
	}

	return cfg
}

func provideShutdownCh() chan struct{} {
	return make(chan struct{})
}

// provideNotifier builds the governance alert dispatcher from config. With no
// channels configured it is a harmless no-op (Dispatcher.Enabled() == false).
// A non-HTTPS alert URL is a configuration error (governance payloads carry
// internal flow details and must not be sent in plaintext) — fail startup so
// the operator fixes the URL rather than silently leaking alerts.
func provideNotifier(cfg *config.Config) (*notify.Dispatcher, error) {
	d, err := notify.New(notify.Config{
		WebhookURL: cfg.Governance.NotifyWebhookURL,
		TeamsURL:   cfg.Governance.NotifyTeamsURL,
	})
	if err != nil {
		return nil, fmt.Errorf("notify: %w", err)
	}
	return d, nil
}

// provideScanner wires the periodic flow scanner. A zero/invalid interval or a
// missing backend/channel leaves it disabled, so it is opt-in and cloud-only.
func provideScanner(cfg *config.Config, backend storageif.StorageBackend, analysisSvc *service.AnalysisService, notifier *notify.Dispatcher) *scanner.Scanner {
	var analyze scanner.AnalyzeFunc
	if analysisSvc != nil {
		analyze = analysisSvc.AnalyzeFlow
	}
	return scanner.New(backend, analyze, notifier, scanInterval(cfg.Governance.ScanInterval))
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
	return padcloud.NewAuthenticator(pc.TenantID, pc.ClientID, pc.Scope, nil)
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
func initIngester(lc fx.Lifecycle, ing *padcloud.Ingester, cfg *config.Config) {
	if ing == nil {
		return
	}
	interval := parseGovernanceDuration("PAD_PP_INGEST_INTERVAL", cfg.PowerPlatform.IngestInterval)
	if interval <= 0 {
		return
	}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			logger.Info("starting Power Platform connector (experimental)", "interval", interval)
			ing.Start(interval)
			return nil
		},
		OnStop: func(context.Context) error { ing.Stop(); return nil },
	})
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

func provideAuthManager(lc fx.Lifecycle, cfg *config.Config, backend storageif.StorageBackend) *auth.Manager {
	token := cfg.Auth.Secret
	if token == "" {
		if cfg.Mode == config.ModeCloud && cfg.Auth.Enabled {
			fmt.Fprintln(os.Stderr, "fatal: PAD_AUTH_SECRET must be set in cloud mode (auto-generation disabled)")
			os.Exit(1)
		}
		token = uuid.NewString()
		cfg.Auth.Secret = token
	}

	var blacklist auth.BlacklistStore
	if cfg.Auth.Enabled {
		if pgBackend, ok := backend.(*storagedb.PostgresStorageBackend); ok && pgBackend != nil {
			pgBl := storagedb.NewPostgresBlacklist(pgBackend.DB())
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

	return auth.NewManager(token, blacklist)
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

func provideStorageBackend(lc fx.Lifecycle, cfg *config.Config) StorageResult {
	if cfg.Storage.Backend != config.StorageDatabase {
		return StorageResult{Backend: nil}
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
			fmt.Fprintf(os.Stderr, "invalid PAD_DB_CONN_MAX_LIFETIME %q: %v\n", cfg.Storage.DBConnMaxLifetime, err)
			os.Exit(1)
		}
		dbCfg.ConnMaxLifetime = d
	}
	if cfg.Storage.DBConnMaxIdleTime != "" {
		d, err := time.ParseDuration(cfg.Storage.DBConnMaxIdleTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid PAD_DB_CONN_MAX_IDLE_TIME %q: %v\n", cfg.Storage.DBConnMaxIdleTime, err)
			os.Exit(1)
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

	pgBackend, err := storagedb.New(context.Background(), dbCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to database: %v\n", err)
		os.Exit(1)
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

	return StorageResult{Backend: pgBackend}
}

func initStorageSecrets(cfg *config.Config, backend storageif.StorageBackend) {
	if cfg.Storage.Backend != config.StorageDatabase || backend == nil {
		return
	}
	pgBackend := backend.(*storagedb.PostgresStorageBackend)
	if cfg.Auth.Secret != "" {
		ks, err := pgBackend.NewEncryptedKeyStore([]byte(cfg.Auth.Secret))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to init encrypted keystore: %v\n", err)
			os.Exit(1)
		}
		storage.SetSecretStore(ks)
		logger.Info("encrypted database keystore enabled")
	}
}

func initAuditPool(lc fx.Lifecycle, backend storageif.StorageBackend) {
	api.InitAuditPool(backend)
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			api.ShutdownAuditPool()
			return nil
		},
	})
}

func startServer(lc fx.Lifecycle, cfg *config.Config, router *api.Router, chatSvc *service.ChatService, redisClient *redis.Client) {
	var routerWithLimits http.Handler = router
	var rateLimiters []*apimw.RateLimiter

	if cfg.Mode == config.ModeCloud {
		// When the Redis backplane is configured, build the limiters on the
		// shared store so the effective limit does not scale with replica count
		// (#1). Otherwise each limiter is in-process (correct for single-replica).
		newRL := func(rps, burst float64) *apimw.RateLimiter {
			return apimw.NewRateLimiterRedis(redisClient, rps, burst, cfg.Server.TrustedProxies)
		}
		generalRl := newRL(cfg.Runtime.RateLimitGeneralRPS, cfg.Runtime.RateLimitGeneralBurst).SetGroup("general")
		authRl := newRL(cfg.Runtime.RateLimitAuthRPS, cfg.Runtime.RateLimitAuthBurst).SetGroup("auth")
		analysisRl := newRL(cfg.Runtime.RateLimitExpensiveRPS, cfg.Runtime.RateLimitExpensiveBurst).SetGroup("analysis")
		chatRl := newRL(cfg.Runtime.RateLimitChatRPS, cfg.Runtime.RateLimitChatBurst).SetGroup("chat")
		uploadRl := newRL(cfg.Runtime.RateLimitUploadRPS, cfg.Runtime.RateLimitUploadBurst).SetGroup("upload")
		if redisClient != nil {
			logger.Info("rate limiting: using shared Redis backplane", "url_set", cfg.Redis.URL != "")
		}
		rateLimiters = append(rateLimiters, generalRl, authRl, analysisRl, chatRl, uploadRl)

		// rateLimitersByGroup maps the rateLimitGroup classifier to its limiter
		// so the per-request dispatch is a single lookup.
		rateLimitersByGroup := map[string]*apimw.RateLimiter{
			"general":  generalRl,
			"auth":     authRl,
			"analysis": analysisRl,
			"chat":     chatRl,
			"upload":   uploadRl,
		}

		routerWithLimits = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rl, ok := rateLimitersByGroup[rateLimitGroup(r.Method, r.URL.Path)]
			if !ok {
				rl = generalRl
			}
			rl.Limit(router).ServeHTTP(w, r)
		})
	}

	timeoutDur := 30 * time.Second
	if d, err := time.ParseDuration(cfg.Runtime.RequestTimeout); err == nil {
		timeoutDur = d
	}

	handler := otelhttp.NewHandler(
		apimw.Recovery(
			apimw.RequestTimeout(timeoutDur)(
				apimw.Compress(
					apimw.AccessLog(cfg.Server.TrustedProxies)(apimw.Metrics(routerWithLimits)),
				),
			),
		),
		"http.server",
	)

	var listener net.Listener
	var err error

	if cfg.Mode == config.ModeCloud {
		addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to listen on %s: %v\n", addr, err)
			os.Exit(1)
		}
	} else {
		// Local mode binds loopback only. The port is normally 0 (ephemeral,
		// reported to the Tauri shell via the CONFIG line below), but an
		// explicitly configured port (PAD_PORT / config file) is honoured so
		// browser-based local development can use a stable address.
		listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Server.Port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to listen: %v\n", err)
			os.Exit(1)
		}

		port := listener.Addr().(*net.TCPAddr).Port
		tokenPath, err := writeSessionSecret(cfg.Auth.Secret)
		if err != nil {
			// Writing the secret to a 0600 file is how the Tauri shell receives
			// it without the signing key landing in stdout/logs. If that fails
			// we cannot safely hand off the credential, so fail closed.
			fmt.Fprintf(os.Stderr, "failed to persist session secret: %v\n", err)
			os.Exit(1)
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
			// fx.StopTimeout (25s) bounds the parent context. Reserve a small
			// budget for the goroutines above to finish their teardown, then
			// give server.Shutdown the remainder. If ctx already has a short
			// deadline (rapid restart loop) we still respect it.
			shutdownCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			return server.Shutdown(shutdownCtx)
		},
	})
}

// Rate-limit group labels returned by rateLimitGroup. Kept as unexported consts
// (not string-typed enums) because they double as the limiter's group name in
// metrics/logs.
const (
	rlGroupAuth     = "auth"
	rlGroupAnalysis = "analysis"
	rlGroupChat     = "chat"
	rlGroupUpload   = "upload"
	rlGroupGeneral  = "general"
)

// authRateLimitPaths is the set of auth-shaped endpoints that share the tighter
// auth rate-limit bucket. It deliberately includes the password-recovery
// endpoints (forgot-password / reset-password): those send email and run bcrypt
// on the reset path, so leaving them on the looser "general" bucket enabled
// email-flooding / SMTP cost amplification by attackers rotating source IPs.
var authRateLimitPaths = map[string]struct{}{
	"/api/auth/login":           {},
	"/api/auth/refresh":         {},
	"/api/auth/register":        {},
	"/api/auth/change-password": {},
	"/api/auth/forgot-password": {},
	"/api/auth/reset-password":  {},
	// verify-email and sso/exchange are unauthenticated, token-consuming
	// credential endpoints; keep them on the tighter auth bucket rather than the
	// looser general one so they can't be flooded by rotating source IPs.
	"/api/auth/verify-email": {},
	"/api/auth/sso/exchange": {},
}

// rateLimitGroup classifies a request into its rate-limit group. It is a pure
// function (no I/O) so the routing policy can be unit-tested independently of
// the fx wiring. Order matters only in that the explicit checks take precedence
// over the general fallback.
func rateLimitGroup(method, path string) string {
	if _, ok := authRateLimitPaths[path]; ok {
		return rlGroupAuth
	}
	if method == "POST" {
		if strings.HasPrefix(path, "/api/analysis/") {
			return rlGroupAnalysis
		}
		if path == "/api/chat/stream" {
			return rlGroupChat
		}
		if path == "/api/flow/upload" {
			return rlGroupUpload
		}
	}
	return rlGroupGeneral
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
