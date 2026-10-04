package middleware

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

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

// TestRedisStore_SharedAcrossInstances is the core guarantee: two limiter
// instances (one per replica) sharing one Redis client must enforce a single
// shared bucket, so the effective limit does not multiply by replica count.
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

// hangingRedis is a TCP endpoint that accepts connections and reads whatever
// is sent but never answers — a Redis brownout as the client sees it. It
// counts the connections it accepted.
func hangingRedis(t *testing.T) (addr string, accepted *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	accepted = new(atomic.Int64)
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			conns = append(conns, c)
			go func() { _, _ = c.Read(make([]byte, 4096)) }()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String(), accepted
}

// TestRedisStore_UnresponsiveRedisFallsBackFast: with a Redis that never
// answers, a limiter call ran under go-redis defaults (3 s read timeout, then
// retries), so every API request of a brownout blocked for seconds before it
// reached the in-memory fallback. One call must give up within
// redisCallTimeout, and the breaker must keep the following calls off Redis
// entirely, so they neither wait nor open connections. Both client kinds:
// the shared backplane client does not apply context deadlines to socket I/O
// (ContextTimeoutEnabled is off), one that does is called directly.
func TestRedisStore_UnresponsiveRedisFallsBackFast(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ctxTimeouts bool
	}{{"default client", false}, {"ContextTimeoutEnabled", true}} {
		t.Run(tc.name, func(t *testing.T) { testUnresponsiveRedisFallsBackFast(t, tc.ctxTimeouts) })
	}
}

func testUnresponsiveRedisFallsBackFast(t *testing.T, ctxTimeouts bool) {
	addr, accepted := hangingRedis(t)
	client := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: ctxTimeouts})
	t.Cleanup(func() { _ = client.Close() })
	s := newRedisStore(client)
	defer s.Stop()

	start := time.Now()
	if !s.Allow(context.Background(), "ratelimit:general:x", 0, 25) {
		t.Fatal("expected the fallback bucket to admit the first request")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("first call took %v against an unresponsive Redis; want about %v", d, redisCallTimeout)
	}
	connsAfterFirst := accepted.Load()

	start = time.Now()
	for i := 0; i < 20; i++ {
		if !s.Allow(context.Background(), "ratelimit:general:x", 0, 25) {
			t.Fatalf("call %d: expected the fallback bucket to admit", i+2)
		}
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("20 calls with the breaker open took %v; they must not wait on Redis", d)
	}
	time.Sleep(20 * time.Millisecond) // let any stray dial reach Accept
	if got := accepted.Load(); got > connsAfterFirst {
		t.Errorf("connections grew from %d to %d while the breaker was open", connsAfterFirst, got)
	}
}

// TestRedisStore_BreakerRetriesRedisAfterCooldown: after a failure the store
// stays on the fallback for redisBreakerCooldown even once Redis is back, then
// returns to Redis.
func TestRedisStore_BreakerRetriesRedisAfterCooldown(t *testing.T) {
	client, mr := newMiniRedis(t)
	s := newRedisStore(client)
	defer s.Stop()
	clock := time.Now()
	s.now = func() time.Time { return clock }
	ctx := context.Background()
	const key = "ratelimit:general:breaker"

	mr.SetError("LOADING Redis is loading the dataset in memory")
	if !s.Allow(ctx, key, 1, 5) {
		t.Fatal("expected the fallback to admit while Redis errors")
	}
	mr.SetError("")

	clock = clock.Add(redisBreakerCooldown - time.Millisecond)
	s.Allow(ctx, key, 1, 5)
	if mr.Exists(key) {
		t.Fatal("Redis was called while the breaker was open")
	}

	clock = clock.Add(2 * time.Millisecond)
	s.Allow(ctx, key, 1, 5)
	if !mr.Exists(key) {
		t.Fatal("Redis was not called again after the cooldown")
	}
}

// TestRedisStore_CancelledRequestDoesNotOpenBreaker: a call that fails because
// the request's own context ended says nothing about Redis, so it must not
// move every other request onto the per-replica fallback.
func TestRedisStore_CancelledRequestDoesNotOpenBreaker(t *testing.T) {
	client, mr := newMiniRedis(t)
	s := newRedisStore(client)
	defer s.Stop()
	const key = "ratelimit:general:cancel"

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	s.Allow(cancelled, key, 1, 5)

	s.Allow(context.Background(), key, 1, 5)
	if !mr.Exists(key) {
		t.Fatal("a cancelled request opened the breaker; the next request skipped Redis")
	}
}
