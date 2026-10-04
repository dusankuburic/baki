package redisx

import (
	"strings"
	"testing"

	"pad-analyzer/internal/config"
)

// The shared client must enable go-redis's in-band context deadlines: without
// ContextTimeoutEnabled the rate limiter wraps every command in a goroutine to
// apply the deadline (internal/api/middleware/ratelimit.go, redisStore.run) —
// one goroutine and buffered channel per request.
func TestOptionsFor_ContextTimeoutEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Redis.URL = "redis://localhost:6379/0"
	cfg.Redis.PoolSize = 7
	cfg.Redis.MinIdleConns = 2

	opts, err := optionsFor(cfg)
	if err != nil {
		t.Fatalf("optionsFor: %v", err)
	}
	if !opts.ContextTimeoutEnabled {
		t.Error("ContextTimeoutEnabled is false: the rate limiter will spawn a goroutine per request")
	}
	if opts.PoolSize != 7 {
		t.Errorf("PoolSize = %d, want the configured 7", opts.PoolSize)
	}
	if opts.MinIdleConns != 2 {
		t.Errorf("MinIdleConns = %d, want the configured 2", opts.MinIdleConns)
	}
}

func TestOptionsFor_ParseError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Redis.URL = "not-a-redis-url"
	if _, err := optionsFor(cfg); err == nil || !strings.Contains(err.Error(), "PAD_REDIS_URL") {
		t.Errorf("err = %v, want a PAD_REDIS_URL parse error", err)
	}
}
