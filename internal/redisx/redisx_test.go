package redisx_test

import (
	"testing"

	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/redisx"
)

func TestProvide_DisabledWhenURLUnset(t *testing.T) {
	lc := fxtest.NewLifecycle(t)
	client, err := redisx.Provide(lc, &config.Config{}) // Redis.URL empty
	if err != nil {
		t.Fatalf("expected no error when backplane disabled, got %v", err)
	}
	if client != nil {
		t.Fatalf("expected nil client when backplane disabled, got %v", client)
	}
	lc.RequireStart().RequireStop()
}

func TestProvide_BadURLIsRejected(t *testing.T) {
	lc := fxtest.NewLifecycle(t)
	_, err := redisx.Provide(lc, &config.Config{Redis: config.RedisConfig{URL: "redis://[::1" /* malformed */}})
	if err == nil {
		t.Fatal("expected an error for a malformed PAD_REDIS_URL, got nil")
	}
}

// TestProvide_UnreachableIsRejected asserts the backplane fails fast at boot
// when configured but down — a silent broken backplane is worse than no boot,
// because every multi-replica subsystem would then run unsynchronized.
func TestProvide_UnreachableIsRejected(t *testing.T) {
	lc := fxtest.NewLifecycle(t)
	// :1 is reserved/unroutable in most CI → Ping times out fast.
	_, err := redisx.Provide(lc, &config.Config{Redis: config.RedisConfig{URL: "redis://127.0.0.1:1/0"}})
	if err == nil {
		t.Fatal("expected an error when the configured Redis is unreachable, got nil")
	}
}
