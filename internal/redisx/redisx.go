// Package redisx owns the optional Redis backplane client. The client is nil
// when PAD_REDIS_URL is unset, in which case every consumer falls back to its
// in-process implementation (single-replica); setting the URL switches the
// rate limiter, the WebSocket collaboration backplane and chat-stream resume
// to a shared store consistent across replicas. The scanner coordinates
// through a Postgres advisory lock instead, so it needs no Redis.
//
// Beside the client the package owns what every Redis user shares: the payload Envelope and its tolerant Decode,
// the Declare registry of every key and channel (golden: internal/di/testdata/redis_keys.golden), and Health, the
// degraded-not-unready signal (docs/adr/0029-redis-envelope-key-registry-and-health.md).
package redisx

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"
	"pad-core/logger"

	"pad-analyzer/internal/config"
)

// Provide returns a Redis client when cfg.Redis.URL is set, or nil when the
// backplane is disabled (the default). A nil client is the sentinel consumers
// check to pick their in-memory implementation. The returned client is pool-
// managed and closed on application shutdown.
//
// A URL that does not parse refuses the boot; a server that does not answer does not: the app starts degraded
// (a loud WARN, pad_redis_up 0, /readyz still 200 with "degraded":["redis"]) and the health loop notices the
// recovery. Every consumer has a declared on-down behaviour (Spec.OnDown), and a rolling deploy during a Redis
// maintenance window must not take the app down with it.
func Provide(lc fx.Lifecycle, cfg *config.Config) (*redis.Client, error) {
	client, _, err := provide(lc, cfg)
	return client, err
}

// bootPingTimeout bounds the first ping, so an unreachable server delays the boot by at most this long.
const bootPingTimeout = 3 * time.Second

// provide is Provide plus the Health that watches the client.
func provide(lc fx.Lifecycle, cfg *config.Config) (*redis.Client, *Health, error) {
	if !cfg.Redis.URL.IsSet() {
		return nil, NewHealth(nil), nil
	}
	opts, err := optionsFor(cfg)
	if err != nil {
		return nil, nil, err
	}
	warnTransport(cfg)
	client := redis.NewClient(opts)
	health := NewHealth(client)

	// Ping with a short deadline so a bad endpoint is reported at boot instead of surfacing as opaque timeouts on
	// the first limited request. Not fatal: see above.
	ctx, cancel := context.WithTimeout(context.Background(), bootPingTimeout)
	if err := health.Ping(ctx); err != nil {
		logger.Warn("redis: backplane unreachable at boot; starting degraded and retrying in the background", "class", errorClass(err))
	}
	cancel()

	loopCtx, stopLoop := context.WithCancel(context.Background())
	var loop sync.WaitGroup
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			loop.Add(1)
			go func() {
				defer loop.Done()
				health.Run(loopCtx)
			}()
			return nil
		},
		OnStop: func(context.Context) error {
			stopLoop()
			loop.Wait()
			return client.Close()
		},
	})
	return client, health, nil
}

// optionsFor builds the shared client's options: PAD_REDIS_URL plus the
// optional pool overrides (defaults: 10×GOMAXPROCS PoolSize, 0 MinIdleConns;
// tuned via PAD_REDIS_POOL_SIZE / PAD_REDIS_MIN_IDLE_CONNS).
// ContextTimeoutEnabled is forced on so go-redis applies the caller's context
// deadline inside its own command path — without it the rate limiter wraps
// every command in a goroutine to apply the deadline
// (internal/api/middleware/ratelimit.go, redisStore.run): one goroutine and
// buffered channel per request, twice for writes.
func optionsFor(cfg *config.Config) (*redis.Options, error) {
	opts, err := redis.ParseURL(cfg.Redis.URL.Reveal())
	if err != nil {
		return nil, errors.New("redis: parse PAD_REDIS_URL: " + parseFailure(err))
	}
	if cfg.Redis.PoolSize > 0 {
		opts.PoolSize = cfg.Redis.PoolSize
	}
	if cfg.Redis.MinIdleConns > 0 {
		opts.MinIdleConns = cfg.Redis.MinIdleConns
	}
	opts.ContextTimeoutEnabled = true
	return opts, nil
}

// parseFailure says what is wrong with PAD_REDIS_URL without quoting any of it (R9): url.Parse's error carries the
// whole URL and go-redis's the part it rejects (a port, a path, an option or its value), and a password with an
// unescaped reserved character ends up in one of those. go-redis's errors read "redis: <what>: <the text>"; only
// <what> is kept, and only when it is plain words, so a message shaped otherwise says nothing rather than too much.
// The original error is not wrapped: unwrapping it would quote the URL again.
func parseFailure(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return "malformed URL (percent-encode / ? # @ % in the password)"
	}
	what, _, _ := strings.Cut(strings.TrimPrefix(err.Error(), "redis: "), ": ")
	if what == "" || strings.TrimLeft(what, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_ ") != "" {
		return "invalid URL"
	}
	return what
}

// Module wires the optional Redis client and its Health into the fx dependency graph.
var Module = fx.Provide(provide)

// warnTransport says at boot what a link between several replicas and Redis exposes when it is not protected: event
// bodies cross Redis in plaintext (chat answers, flow names, finding comments for every tenant, user ids in key
// names), and what can publish on the command channels can stop streams and decide approvals. TLS (rediss://) and a
// password (or ACL user) are what stands in front of that; the commands are also signed (CommandBus) when the replicas
// share a secret. Said once per boot, never with the URL.
func warnTransport(cfg *config.Config) {
	if cfg.Storage.MaxReplicas <= 1 {
		return
	}
	opts, err := redis.ParseURL(cfg.Redis.URL.Reveal())
	if err != nil {
		return
	}
	var problems []string
	if opts.TLSConfig == nil && !strings.HasPrefix(cfg.Redis.URL.Reveal(), "unix://") {
		problems = append(problems, "the link is not TLS (use rediss://)")
	}
	if opts.Password == "" {
		problems = append(problems, "the URL carries no password")
	}
	if len(problems) == 0 {
		return
	}
	LimitedWarn("redis_transport", "redis: event bodies and stream commands cross Redis between the replicas, and "+strings.Join(problems, " and ")+
		"; keep Redis on a private network with a password and TLS (docs/adr/0030)", nil, "replicas", cfg.Storage.MaxReplicas)
}
