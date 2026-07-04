// Package redisx owns the optional Redis backplane client. The client is nil
// when PAD_REDIS_URL is unset, in which case every consumer falls back to its
// in-process implementation (single-replica). Setting the URL switches the
// rate limiter (#1), and will switch WebSocket presence (#2) and chat-stream
// resume (#3), to a shared store so those subsystems are consistent across
// replicas (PRODUCTION_READINESS.md #1-#3).
package redisx

import (
	"context"
	"fmt"
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
	if cfg.Redis.URL == "" {
		return nil, nil
	}
	opts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return nil, fmt.Errorf("redis: parse PAD_REDIS_URL: %w", err)
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

// Module wires the optional Redis client into the fx dependency graph.
var Module = fx.Provide(Provide)
