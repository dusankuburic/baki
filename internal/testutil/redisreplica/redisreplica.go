// Package redisreplica is the harness for tests of several replicas that share one Redis: N "replicas", each with
// its own instance ID, its own *redis.Client (its own connection pool, as separate processes have) and its own
// redisx.Health, all talking to one in-process miniredis. A test builds whatever it needs on each replica (a Hub, a
// ChatService, a limiter, a blacklist) from that replica's client and ID, drives a scenario across them, and calls
// Kill and Restore to play a Redis outage. Later phases of the multi-replica work add their mechanism's scenario
// beside the two-replica tests that use this.
package redisreplica

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/redisx"
)

// Replica is one simulated app instance's view of Redis.
type Replica struct {
	// ID is the replica's instance ID (what PAD_INSTANCE_ID is in a real deployment): "replica-1", "replica-2", ...
	ID string
	// Client is this replica's own client, with the Health hook installed.
	Client *redis.Client
	// Health watches Client. Its ping loop is not started; call Health.Ping or Health.Run when a test needs it.
	Health *redisx.Health
}

// Cluster is N replicas on one miniredis.
type Cluster struct {
	// Redis is the shared server, for assertions on its contents (HGet, Get, TTL, PubSubNumSub) and for
	// fault injection beyond Kill (SetError, FastForward).
	Redis    *miniredis.Miniredis
	Replicas []*Replica
}

// New starts a miniredis and n replicas on it. Everything is closed when the test ends. The clients fail fast and
// never retry, so a Kill shows up in the test as it would in the app (an error, not a long stall).
func New(t testing.TB, n int) *Cluster {
	t.Helper()
	if n < 1 {
		t.Fatalf("redisreplica.New(%d): need at least one replica", n)
	}
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	c := &Cluster{Redis: mr}
	for i := 1; i <= n; i++ {
		client := redis.NewClient(&redis.Options{
			Addr:                  mr.Addr(),
			ContextTimeoutEnabled: true, // as redisx.optionsFor sets it
			DialTimeout:           500 * time.Millisecond,
			ReadTimeout:           500 * time.Millisecond,
			WriteTimeout:          500 * time.Millisecond,
			MaxRetries:            -1,
		})
		t.Cleanup(func() { _ = client.Close() })
		c.Replicas = append(c.Replicas, &Replica{ID: fmt.Sprintf("replica-%d", i), Client: client, Health: redisx.NewHealth(client)})
	}
	return c
}

// Kill takes Redis down: connections are cut and new ones refused, as when the server dies. Its data survives for
// Restore, like a restart from a snapshot.
func (c *Cluster) Kill() { c.Redis.Close() }

// Restore brings the server back on the same address with the data it had.
func (c *Cluster) Restore(t testing.TB) {
	t.Helper()
	if err := c.Redis.Restart(); err != nil {
		t.Fatalf("restart miniredis: %v", err)
	}
}

// Eventually polls cond until it holds or d passes, for the asynchronous parts of a scenario (a subscription
// registering, a message crossing).
func Eventually(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out after %v waiting for %s", d, what)
	}
}

// PrevFixture returns a payload captured from a previous release (internal/redisx/testdata/redis/prev/<release>/).
// A test decodes it with the current code: whatever an older replica left in Redis must still read.
func PrevFixture(t testing.TB, release, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(FixtureRoot(), release, name)) //nolint:gosec // G304: a test helper reading a checked-in fixture by name
	if err != nil {
		t.Fatalf("previous-release fixture %s/%s: %v", release, name, err)
	}
	return []byte(strings.TrimSpace(string(b)))
}

// FixtureRoot is the directory holding one subdirectory per previous release.
func FixtureRoot() string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..", "redisx", "testdata", "redis", "prev")
}
