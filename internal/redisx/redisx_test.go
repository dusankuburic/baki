package redisx

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/secret"
)

func TestProvide_DisabledWhenURLUnset(t *testing.T) {
	lc := fxtest.NewLifecycle(t)
	client, err := Provide(lc, &config.Config{}) // Redis.URL empty
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
	_, err := Provide(lc, &config.Config{Redis: config.RedisConfig{URL: secret.New("redis://[::1" /* malformed */)}})
	if err == nil {
		t.Fatal("expected an error for a malformed PAD_REDIS_URL, got nil")
	}
}

// TestProvide_UnreachableStartsDegraded: a configured Redis that does not answer at boot no longer refuses the boot (a
// rolling deploy during a Redis maintenance window must not take the app down). The client is returned, Health says
// degraded, and the ping loop notices the recovery. A URL that does not parse still refuses
// (TestProvide_BadURLIsRejected).
func TestProvide_UnreachableStartsDegraded(t *testing.T) {
	lc := fxtest.NewLifecycle(t)
	// :1 is reserved/unroutable in most CI → the ping fails fast.
	client, health, err := provide(lc, &config.Config{Redis: config.RedisConfig{URL: secret.New("redis://127.0.0.1:1/0")}})
	if err != nil {
		t.Fatalf("an unreachable Redis refused the boot: %v", err)
	}
	if client == nil || health == nil || !health.Configured() {
		t.Fatalf("client %v, health %v: want both, configured", client, health)
	}
	if !health.Degraded() {
		t.Error("Health is not degraded although the boot ping failed")
	}
	lc.RequireStart().RequireStop()
}

// A reachable Redis starts healthy, and the lifecycle runs the ping loop and closes the client.
func TestProvide_ReachableStartsHealthy(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	lc := fxtest.NewLifecycle(t)
	client, health, err := provide(lc, &config.Config{Redis: config.RedisConfig{URL: secret.New("redis://" + mr.Addr() + "/0")}})
	if err != nil {
		t.Fatal(err)
	}
	if health.Degraded() {
		t.Error("degraded against a reachable Redis")
	}
	lc.RequireStart().RequireStop()
	if err := client.Ping(context.Background()).Err(); err == nil {
		t.Error("the client is still open after the lifecycle stopped")
	}
}

// Without PAD_REDIS_URL there is no client and an unconfigured Health that is never degraded.
func TestProvide_DisabledHasAnUnconfiguredHealth(t *testing.T) {
	lc := fxtest.NewLifecycle(t)
	client, health, err := provide(lc, &config.Config{})
	if err != nil || client != nil {
		t.Fatalf("client %v, err %v; want neither", client, err)
	}
	if health == nil || health.Configured() || health.Degraded() {
		t.Errorf("health = %+v, want an unconfigured one", health)
	}
}

// With several replicas the Redis traffic carries event bodies (chat answers, flow names) and the commands that act as
// users, so a link that is neither TLS nor password-protected is called out at boot (REVIEW_REDIS M6). One replica is
// not warned: the deployment need not run Redis across a network.
func TestWarnTransport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		url      string
		replicas int
		want     []string // substrings of the line, none: no line
	}{
		{"plaintext with a password, several replicas", "redis://:s3cret@cache:6379/0", 3, []string{"rediss://"}},
		{"plaintext without a password, several replicas", "redis://cache:6379/0", 2, []string{"rediss://", "password"}},
		{"TLS with a password, several replicas", "rediss://:s3cret@cache:6380/0", 3, nil},
		{"TLS without a password, several replicas", "rediss://cache:6380/0", 3, []string{"password"}},
		{"plaintext, one replica", "redis://cache:6379/0", 1, nil},
		{"plaintext, replicas unset", "redis://cache:6379/0", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, restore := captureWarns(t)
			defer restore()
			cfg := &config.Config{Redis: config.RedisConfig{URL: secret.New(tc.url)}}
			cfg.Storage.MaxReplicas = tc.replicas
			warnTransport(cfg)
			got := lines()
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("warned %v, want nothing", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("%d lines %v, want one", len(got), got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got[0], w) {
					t.Errorf("line %q lacks %q", got[0], w)
				}
			}
			if strings.Contains(got[0], "s3cret") || strings.Contains(got[0], "cache:") {
				t.Errorf("line %q carries the URL or its secret", got[0])
			}
		})
	}
}
