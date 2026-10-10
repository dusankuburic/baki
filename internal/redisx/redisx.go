// Package redisx owns the optional Redis backplane client. The client is nil
// when PAD_REDIS_URL is unset, in which case every consumer falls back to its
// in-process implementation (single-replica); setting the URL switches the
// rate limiter, the WebSocket collaboration backplane and chat-stream resume
// to a shared store consistent across replicas. The scanner coordinates
// through a Postgres advisory lock instead, so it needs no Redis.
package redisx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"pad-analyzer/internal/config"
)

// Provide returns a Redis client when cfg.Redis.URL is set, or nil when the
// backplane is disabled (the default). A nil client is the sentinel consumers
// check to pick their in-memory implementation. The returned client is pool-
// managed and closed on application shutdown.
func Provide(lc fx.Lifecycle, cfg *config.Config) (*redis.Client, error) {
	if !cfg.Redis.URL.IsSet() {
		return nil, nil
	}
	opts, err := optionsFor(cfg)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)

	// Ping with a short deadline so a bad URL/endpoint fails fast at boot
	// instead of surfacing as opaque timeouts on the first limited request.
	// The backplane is a hard dependency of multi-replica correctness, so we
	// refuse to start (not just warn) when it is configured but unreachable.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis: backplane unreachable: %w", err)
	}

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			return client.Close()
		},
	})
	return client, nil
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

// Module wires the optional Redis client into the fx dependency graph.
var Module = fx.Provide(Provide)
