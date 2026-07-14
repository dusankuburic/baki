package middleware

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newMiniRedis stands up an in-process Redis (no external dependency) shared by
// every store in a test, so the suite can exercise the multi-replica behaviour
// deterministically in CI.
func newMiniRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client, mr
}

// TestRedisStore_SharedAcrossInstances is the core #1 guarantee: two limiter
// instances (one per replica) sharing one Redis client must enforce a single
// shared bucket, so the effective limit no longer multiplies by replica count.
func TestRedisStore_SharedAcrossInstances(t *testing.T) {
	client, _ := newMiniRedis(t)
	const rate, burst = 0.0, 3.0 // no refill during the test (rate 0)
	replicaA := newRedisStore(client)
	replicaB := newRedisStore(client)
	ctx := context.Background()
	key := "ratelimit:general:10.0.0.1"

	// Burn through the burst from replica A.
	admitted := 0
	for i := 0; i < 5; i++ {
		if replicaA.Allow(ctx, key, rate, burst) {
			admitted++
		}
	}
	if admitted != 3 {
		t.Fatalf("replica A: expected burst of 3 admits, got %d", admitted)
	}

	// Replica B must see an empty bucket — the limit is shared, not per-process.
	if replicaB.Allow(ctx, key, rate, burst) {
		t.Fatal("replica B admitted past the shared burst; bucket is not shared across instances")
	}
}

// TestRedisStore_GroupsAreIndependent verifies the group namespace in the key
// (auth vs general) so two limiters sharing one Redis don't collide.
func TestRedisStore_GroupsAreIndependent(t *testing.T) {
	client, _ := newMiniRedis(t)
	s := newRedisStore(client)
	ctx := context.Background()

	// Exhaust the "auth" bucket for an IP.
	authKey := "ratelimit:auth:1.2.3.4"
	for i := 0; i < 3; i++ {
		s.Allow(ctx, authKey, 0, 3)
	}
	if s.Allow(ctx, authKey, 0, 3) {
		t.Fatal("auth bucket should be exhausted")
	}
	// Same IP under a different group still has its full burst.
	generalKey := "ratelimit:general:1.2.3.4"
	if !s.Allow(ctx, generalKey, 0, 3) {
		t.Fatal("general bucket should be independent of auth")
	}
}

// TestRedisStore_RefillsOverTime verifies the Lua script refills the bucket as
// time passes (the elapsed-since-last-fill term), matching the in-memory
// semantics. Calls the script directly with a controlled `now` so the test
// doesn't depend on real time.
func TestRedisStore_RefillsOverTime(t *testing.T) {
	client, _ := newMiniRedis(t)
	ctx := context.Background()
	key := "ratelimit:general:refill"

	// rate=1 token/sec, burst=2. Exhaust it at t0.
	admit := func(nowMs int64) bool {
		v, err := tokenBucketScript.Run(ctx, client, []string{key}, 1.0, 2.0, nowMs, int64(300_000)).Int()
		if err != nil {
			t.Fatalf("script run: %v", err)
		}
		return v == 1
	}
	// t0: burst of 2.
	if !admit(1_000) {
		t.Fatal("expected first admit")
	}
	if !admit(1_000) {
		t.Fatal("expected second admit (burst)")
	}
	if admit(1_000) {
		t.Fatal("expected deny at t0 (burst exhausted)")
	}
	// +1s: 1 token refilled → one admit.
	if !admit(2_000) {
		t.Fatal("expected admit after 1 token refilled")
	}
	if admit(2_000) {
		t.Fatal("expected deny again before more refill")
	}
	// +3s: capacity (2) refilled → two admits.
	if !admit(5_000) {
		t.Fatal("expected admit after refill to capacity")
	}
	if !admit(5_000) {
		t.Fatal("expected second admit at capacity")
	}
	if admit(5_000) {
		t.Fatal("expected deny once capacity exhausted again")
	}
}

// TestRedisStore_FallbackOnRedisError asserts the store degrades to a
// per-replica in-memory bucket when Redis is unreachable, rather than fully
// uncapping (the previous behaviour let an attacker who DoS'd Redis disable all
// rate limiting). The first `capacity` requests are admitted; the next is
// rejected — the limit is enforced, just locally instead of across replicas.
func TestRedisStore_FallbackOnRedisError(t *testing.T) {
	client, _ := newMiniRedis(t)
	s := newRedisStore(client)
	// Close the underlying server so the next command errors.
	client.Close()

	ctx := context.Background()
	// capacity 3 with rate 0 (no refill) — exactly 3 admitted, the 4th rejected.
	if !s.Allow(ctx, "ratelimit:general:x", 0, 3) {
		t.Fatal("expected first fallback request to be admitted")
	}
	if !s.Allow(ctx, "ratelimit:general:x", 0, 3) {
		t.Fatal("expected second fallback request to be admitted")
	}
	if !s.Allow(ctx, "ratelimit:general:x", 0, 3) {
		t.Fatal("expected third fallback request to be admitted")
	}
	if s.Allow(ctx, "ratelimit:general:x", 0, 3) {
		t.Fatal("expected 4th request to be rejected by the fallback bucket, not fully uncapped")
	}
}
