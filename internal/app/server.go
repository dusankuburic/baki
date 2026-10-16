package app

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

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"pad-analyzer/internal/api"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/drain"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	"pad-core/logger"
)

func provideShutdownCh() chan struct{} {
	return make(chan struct{})
}

// startServer builds the full middleware stack (api.BuildHandler — see
// internal/api/middleware_chain.go for the layer order), binds a listener, and
// registers the fx lifecycle hooks that start/stop the HTTP server. Returns an
// error rather than calling os.Exit so fx surfaces boot failures.
func startServer(lc fx.Lifecycle, cfg *config.Config, router *api.Router, redisClient *redis.Client) error {
	handler, rateLimiters := api.BuildHandler(router, cfg, redisClient)

	var listener net.Listener
	var err error

	if capability.Of(cfg).Profile() == capability.Cloud {
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
		tokenPath, err := writeSessionSecret(cfg.Auth.Secret.Reveal())
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
			// The drain hook ran first (registerDrain): the chat streams have ended and their terminal events are
			// queued on the event streams, which this closes; each handler writes what is queued, then returns.
			router.Shutdown()
			for _, rl := range rateLimiters {
				rl.Stop()
			}
			// The budgets below (the WebSocket clients', then the HTTP server's and the background runners') are rows
			// of ADR 0024's stop budget table, which keeps the whole stop, the audit flush and the pool close last,
			// inside fx.StopTimeout: change them together.
			//
			// Gracefully drain WebSocket clients FIRST. http.Server.Shutdown
			// does not close hijacked (WebSocket) sockets, so without this
			// every rolling restart with even one live WS client takes the
			// full shutdownCtx budget and drops in-flight collab state.
			wsCtx, wsCancel := context.WithTimeout(ctx, 3*time.Second)
			if err := router.ShutdownWebSocket(wsCtx); err != nil {
				logger.Warn("websocket shutdown exceeded budget or was cancelled", "error", err)
			}
			wsCancel()
			// A request still in flight at the end of its budget is cut off:
			// that beats losing the audit tail and the clean pool close.
			shutdownCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			err := server.Shutdown(shutdownCtx)
			// Best-effort background work (webhooks, emails, alert fan-outs,
			// usage rows) started by the requests that just finished
			// gets the rest of the same budget; any still running after it is
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

// registerDrain registers the first stop hook to run (it is the last Invoke; ADR 0024): readiness 503, and new chat
// streams, event streams and sockets refused, at once; after the drain delay the chat streams end with
// SERVER_RESTARTING over the event streams that are still open, which startServer's hook closes next.
func registerDrain(lc fx.Lifecycle, cfg *config.Config, chatSvc *service.ChatService) {
	lc.Append(fx.Hook{OnStop: func(ctx context.Context) error {
		if err := drain.Process().Drain(ctx, cfg.Runtime.ShutdownDrainDelay.Std(), chatSvc.Shutdown); err != nil {
			logger.Warn("drain incomplete", "error", err)
		}
		return nil // the rest of the stop must still run
	}})
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
	if cfg.Server.BehindProxy && strings.TrimSpace(cfg.Server.MetricsToken.Reveal()) == "" {
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
