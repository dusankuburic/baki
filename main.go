package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/fx"

	"pad-analyzer/internal/api"
	apimw "pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/di"
	"pad-analyzer/internal/logger"
	padmetrics "pad-analyzer/internal/metrics"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/telemetry"
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
		),
		di.ServiceModule,
		di.APIModule,
		fx.Invoke(
			initLogger,
			initTelemetry,
			initStorageSecrets,
			initAuditPool,
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
		cfg = config.LoadFromEnvRaw()
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

func provideShutdownCh(lc fx.Lifecycle) chan struct{} {
	ch := make(chan struct{})
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			close(ch)
			return nil
		},
	})
	return ch
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
	logger.InitWith(logger.Options{
		Level:      cfg.Log.Level,
		StdoutOnly: cfg.Mode == config.ModeCloud,
	})
	if cfg.Mode != config.ModeCloud {
		configDir, _ := storage.ConfigDir()
		logger.Init(configDir, false)
	}
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

	// Apply Azure Blob Storage config if available
	if cfg.Storage.AzureStorageAccount != "" && cfg.Storage.AzureStorageContainer != "" {
		dbCfg.AzureStorageAccount = cfg.Storage.AzureStorageAccount
		dbCfg.AzureStorageContainer = cfg.Storage.AzureStorageContainer
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

func startServer(lc fx.Lifecycle, cfg *config.Config, router *api.Router, chatSvc *service.ChatService) {
	var routerWithLimits http.Handler = router
	var rateLimiters []*apimw.RateLimiter

	if cfg.Mode == config.ModeCloud {
		generalRl := apimw.NewRateLimiter(cfg.Runtime.RateLimitGeneralRPS, cfg.Runtime.RateLimitGeneralBurst, cfg.Server.TrustedProxies).SetGroup("general")
		authRl := apimw.NewRateLimiter(cfg.Runtime.RateLimitAuthRPS, cfg.Runtime.RateLimitAuthBurst, cfg.Server.TrustedProxies).SetGroup("auth")
		analysisRl := apimw.NewRateLimiter(cfg.Runtime.RateLimitExpensiveRPS, cfg.Runtime.RateLimitExpensiveBurst, cfg.Server.TrustedProxies).SetGroup("analysis")
		chatRl := apimw.NewRateLimiter(cfg.Runtime.RateLimitChatRPS, cfg.Runtime.RateLimitChatBurst, cfg.Server.TrustedProxies).SetGroup("chat")
		uploadRl := apimw.NewRateLimiter(cfg.Runtime.RateLimitUploadRPS, cfg.Runtime.RateLimitUploadBurst, cfg.Server.TrustedProxies).SetGroup("upload")
		rateLimiters = append(rateLimiters, generalRl, authRl, analysisRl, chatRl, uploadRl)

		routerWithLimits = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if path == "/api/auth/login" || path == "/api/auth/refresh" || path == "/api/auth/register" || path == "/api/auth/change-password" {
				authRl.Limit(router).ServeHTTP(w, r)
			} else if r.Method == "POST" && strings.HasPrefix(path, "/api/analysis/") {
				analysisRl.Limit(router).ServeHTTP(w, r)
			} else if r.Method == "POST" && path == "/api/chat/stream" {
				chatRl.Limit(router).ServeHTTP(w, r)
			} else if r.Method == "POST" && path == "/api/flow/upload" {
				uploadRl.Limit(router).ServeHTTP(w, r)
			} else {
				generalRl.Limit(router).ServeHTTP(w, r)
			}
		})
	}

	timeoutDur := 30 * time.Second
	if d, err := time.ParseDuration(cfg.Runtime.RequestTimeout); err == nil {
		timeoutDur = d
	}

	handler := otelhttp.NewHandler(
		apimw.Recovery(
			apimw.RequestTimeout(timeoutDur)(
				middleware.Compress(5)(
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
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to listen: %v\n", err)
			os.Exit(1)
		}
		
		port := listener.Addr().(*net.TCPAddr).Port
		startupInfo := map[string]any{
			"port":  port,
			"token": cfg.Auth.Secret,
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
