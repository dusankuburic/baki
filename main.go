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
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"syscall"
	"time"

	"github.com/google/uuid"
)

func main() {
	cfg := loadConfig()

	// Initialize storage backend: PostgreSQL for cloud/database mode, nil for local.
	var backend storageif.StorageBackend
	if cfg.Storage.Backend == config.StorageDatabase {
		var err error
		backend, err = storagedb.New(storagedb.DefaultConfig(cfg.Storage.DatabaseURL))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to connect to database: %v\n", err)
			os.Exit(1)
		}
		logger.Info("postgres storage backend ready")
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
	router := api.NewRouter(app, token, cfg.Auth.Enabled, cfg.Server.AllowedOrigins)

	// Init the App with the router as notifier
	ctx := context.Background()
	app.Init(ctx, router)

	// Wrap router with middleware
	generalRl := middleware.NewRateLimiter(60, 20, cfg.Server.TrustedProxies)      // 60 req/s, burst of 20
	authRl := middleware.NewRateLimiter(5.0/60.0, 5, cfg.Server.TrustedProxies)    // 5 req/min, burst of 5

	routerWithLimits := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/auth/refresh" || r.URL.Path == "/api/auth/register" {
			authRl.Limit(router).ServeHTTP(w, r)
		} else {
			generalRl.Limit(router).ServeHTTP(w, r)
		}
	})

	handler := middleware.Recovery(routerWithLimits)

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

	server := &http.Server{Handler: handler}

	// Graceful shutdown on SIGINT / SIGTERM
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		logger.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if backend != nil {
			_ = backend.Close()
		}
	}()

	logger.Info("backend server starting", "addr", listener.Addr().String(), "mode", cfg.Mode)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		logger.Error("server failed", "error", err)
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
