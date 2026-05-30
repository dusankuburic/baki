package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"pad-analyzer/internal/api"
	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/manager"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/telemetry"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var (
	Version   = "0.1.0"
	BuildDate = ""
	GitCommit = ""
)

// @title Pad Analyzer API
// @version 1.0
// @description This is the API for the Pad Analyzer project.
// @host localhost:8080
// @BasePath /
func main() {
	cfg := loadConfig()

	// Initialize Logger
	logger.InitWith(logger.Options{
		Level:      cfg.Log.Level,
		StdoutOnly: cfg.Mode == config.ModeCloud,
	})

	// Resolve sensitive configuration from Azure Key Vault if configured.
	// This happens before storage/auth init so we have the resolved secrets.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := config.ResolveAzureSecrets(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to resolve azure secrets: %v\n", err)
		os.Exit(1)
	}

	// Initialize OpenTelemetry
	otelShutdown, otelErr := telemetry.Init(ctx, "baki-backend", Version)
	if otelErr != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize telemetry: %v\n", otelErr)
	} else {
		defer otelShutdown()
	}

	// Re-validate config after potential Key Vault resolution.
	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize storage backend: PostgreSQL for cloud/database mode, nil for local.
	var backend storageif.StorageBackend
	var pgBackend *storagedb.PostgresStorageBackend
	if cfg.Storage.Backend == config.StorageDatabase {
		var err error
		pgBackend, err = storagedb.New(storagedb.DefaultConfig(cfg.Storage.DatabaseURL))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to connect to database: %v\n", err)
			os.Exit(1)
		}
		backend = pgBackend
		logger.Info("postgres storage backend ready")

		// Cloud mode has no OS keychain, so provider API keys are persisted in
		// Postgres, encrypted at rest with a key derived from the auth secret.
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

	// Initialize App Manager
	app := manager.NewApp(backend)

	// In local/Tauri mode use a random per-session token.
	// In cloud mode require an explicit secret so JWT signing is stable across restarts.
	token := cfg.Auth.Secret
	if token == "" {
		token = uuid.NewString()
	}

	// Initialize the Router (which also acts as a Notifier)
	router := api.NewRouter(app, token, cfg.Auth.Enabled, cfg.Server.AllowedOrigins, cfg.Server.StaticDir)

	// Init the App with the router as notifier
	app.Init(ctx, router, cfg.Log, string(cfg.Mode))

	// Background metrics collection (Prometheus)
	stopMetrics := make(chan struct{})
	if pgBackend != nil {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stopMetrics:
					return
				case <-t.C:
					metrics.ObservePostgresPool(pgBackend.DB())
				}
			}
		}()
	}

	var routerWithLimits http.Handler = router
	if cfg.Mode == config.ModeCloud {
		generalRl := middleware.NewRateLimiter(60, 20, cfg.Server.TrustedProxies).SetGroup("general")   // 60 req/s, burst of 20
		authRl := middleware.NewRateLimiter(5.0/60.0, 5, cfg.Server.TrustedProxies).SetGroup("auth") // 5 req/min, burst of 5

		routerWithLimits = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/auth/refresh" || r.URL.Path == "/api/auth/register" {
				authRl.Limit(router).ServeHTTP(w, r)
			} else {
				generalRl.Limit(router).ServeHTTP(w, r)
			}
		})
	}

	// Middleware stack (outermost first): OTel → Recovery → AccessLog → Metrics → rate-limit → router.
	handler := otelhttp.NewHandler(
		middleware.Recovery(middleware.AccessLog(middleware.Metrics(routerWithLimits))),
		"http.server",
	)

	// Bind the listener
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
		// Tauri sidecar: ephemeral port on loopback
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to listen: %v\n", err)
			os.Exit(1)
		}
		// Output port and token for the Tauri host to read
		port := listener.Addr().(*net.TCPAddr).Port
		startupInfo := map[string]any{
			"port":  port,
			"token": token,
		}
		infoJSON, _ := json.Marshal(startupInfo)
		fmt.Println(string(infoJSON))
	}

	// HTTP server hardening:
	//   * ReadHeaderTimeout bounds slowloris attacks (clients drip-feeding
	//     request headers to exhaust connections).
	//   * IdleTimeout allows the OS to reap dead keep-alive connections.
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		close(stopMetrics)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		if backend != nil {
			_ = backend.Close()
		}
	}()

	// Pick the listen mode: HTTPS directly (TLSCert/TLSKey set) or plain
	// HTTP via the existing listener. Validation already refused the unsafe
	// combination of cloud + auth + no TLS + no BehindProxy.
	useTLS := cfg.Server.TLSCert != "" && cfg.Server.TLSKey != ""
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	logger.Info("backend server starting",
		"addr", listener.Addr().String(),
		"mode", cfg.Mode,
		"scheme", scheme,
		"behind_proxy", cfg.Server.BehindProxy,
	)
	var serveErr error
	if useTLS {
		serveErr = server.ServeTLS(listener, cfg.Server.TLSCert, cfg.Server.TLSKey)
	} else {
		serveErr = server.Serve(listener)
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		logger.Error("server failed", "error", serveErr)
		os.Exit(1)
	}
}

// loadConfig resolves configuration in priority order:
//  1. PAD_CONFIG env var pointing to a JSON file
//  2. PAD_* env vars
//  3. Default() (Tauri local mode)
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
