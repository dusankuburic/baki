package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/fx"

	"pad-analyzer/internal/api"
	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/di"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/metrics"
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
			startServer,
		),
	).Run()
}

func loadConfig() *config.Config {
	if path := os.Getenv("PAD_CONFIG"); path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load config from %s: %v\n", path, err)
			os.Exit(1)
		}
		return cfg
	}

	if os.Getenv("PAD_MODE") != "" || os.Getenv("PAD_PORT") != "" {
		cfg, err := config.LoadFromEnv()
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid environment config: %v\n", err)
			os.Exit(1)
		}
		return cfg
	}

	return config.Default()
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

func provideAuthManager(cfg *config.Config) *auth.Manager {
	token := cfg.Auth.Secret
	if token == "" {
		token = uuid.NewString()
		cfg.Auth.Secret = token // Ensure it's available for stdout later
	}
	return auth.NewManager(token)
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

func initTelemetry(lc fx.Lifecycle) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			otelShutdown, otelErr := telemetry.Init(ctx, "baki-backend", Version)
			if otelErr != nil {
				fmt.Fprintf(os.Stderr, "failed to initialize telemetry: %v\n", otelErr)
			} else {
				_ = otelShutdown
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

	pgBackend, err := storagedb.New(context.Background(), storagedb.DefaultConfig(cfg.Storage.DatabaseURL))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to database: %v\n", err)
		os.Exit(1)
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			go func() {
				t := time.NewTicker(15 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						metrics.ObservePostgresPool(pgBackend.DB())
					}
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
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

func startServer(lc fx.Lifecycle, cfg *config.Config, router *api.Router, chatSvc *service.ChatService) {
	var routerWithLimits http.Handler = router
	var rateLimiters []*middleware.RateLimiter

	if cfg.Mode == config.ModeCloud {
		generalRl := middleware.NewRateLimiter(60, 20, cfg.Server.TrustedProxies).SetGroup("general")
		authRl := middleware.NewRateLimiter(5.0/60.0, 5, cfg.Server.TrustedProxies).SetGroup("auth")
		rateLimiters = append(rateLimiters, generalRl, authRl)

		routerWithLimits = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/auth/refresh" || r.URL.Path == "/api/auth/register" {
				authRl.Limit(router).ServeHTTP(w, r)
			} else {
				generalRl.Limit(router).ServeHTTP(w, r)
			}
		})
	}

	handler := otelhttp.NewHandler(
		middleware.Recovery(middleware.AccessLog(middleware.Metrics(routerWithLimits))),
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
		fmt.Println(string(infoJSON))
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
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
			return server.Shutdown(ctx)
		},
	})
}
