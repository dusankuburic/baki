package redisx

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
)

// newHealth is a Health over a client that fails fast and never retries, so a dead server is an error, not a stall.
func newHealth(t *testing.T) (*miniredis.Miniredis, *redis.Client, *Health) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(), ContextTimeoutEnabled: true,
		DialTimeout: 300 * time.Millisecond, ReadTimeout: 300 * time.Millisecond, WriteTimeout: 300 * time.Millisecond, MaxRetries: -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client, NewHealth(client)
}

func errorsFor(mechanism string) float64 {
	return testutil.ToFloat64(redisErrors.WithLabelValues(mechanism))
}

func TestHealth_NilAndUnconfiguredAreNeverDegraded(t *testing.T) {
	var nilHealth *Health
	for name, h := range map[string]*Health{"nil": nilHealth, "no client": NewHealth(nil)} {
		if h.Configured() || h.Degraded() {
			t.Errorf("%s: configured %v, degraded %v; want neither", name, h.Configured(), h.Degraded())
		}
		if err := h.Ping(context.Background()); err != nil {
			t.Errorf("%s: Ping = %v, want nil", name, err)
		}
		h.RecordError("ratelimit_bucket", errors.New("boom")) // must not panic or degrade
		h.OnChange(func(bool) {})
		if h.Degraded() || h.Status() != (Status{}) {
			t.Errorf("%s: state changed", name)
		}
	}
}

// One failed ping degrades at once; one good operation recovers.
func TestHealth_PingFailureDegradesAndSuccessRecovers(t *testing.T) {
	mr, client, h := newHealth(t)
	ctx := context.Background()
	if err := h.Ping(ctx); err != nil || h.Degraded() {
		t.Fatalf("healthy: err %v degraded %v", err, h.Degraded())
	}

	before := errorsFor("ping")
	mr.Close()
	if err := h.Ping(ctx); err == nil {
		t.Fatal("Ping of a dead server succeeded")
	}
	if !h.Degraded() {
		t.Fatal("one failed ping did not degrade")
	}
	if got := errorsFor("ping") - before; got != 1 {
		t.Errorf("pad_redis_errors_total{ping} grew by %v, want 1", got)
	}
	st := h.Status()
	if !st.Configured || !st.Degraded || st.LastError == "" || st.Since.IsZero() {
		t.Errorf("Status = %+v", st)
	}
	if got := testutil.ToFloat64(redisUp.WithLabelValues()); got != 0 {
		t.Errorf("pad_redis_up = %v while degraded, want 0", got)
	}

	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	// An ordinary operation that works proves Redis is back, without waiting for the next ping.
	if err := client.Set(ctx, "k", "v", time.Minute).Err(); err != nil {
		t.Fatalf("Set after restart: %v", err)
	}
	if h.Degraded() {
		t.Fatal("still degraded after a successful operation")
	}
	if got := testutil.ToFloat64(redisUp.WithLabelValues()); got != 1 {
		t.Errorf("pad_redis_up = %v after recovery, want 1", got)
	}
}

// Operations alone degrade only after a streak: one slow call (the limiter's 75 ms bound) is not an outage.
func TestHealth_OperationFailuresNeedAStreak(t *testing.T) {
	mr, client, h := newHealth(t)
	ctx := context.Background()
	if err := client.Set(ctx, "k", "v", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	mr.Close()

	before := errorsFor("other")
	for i := 1; i < opFailuresToDegrade; i++ {
		if err := client.Get(ctx, "k").Err(); err == nil {
			t.Fatal("Get against a dead server succeeded")
		}
		if h.Degraded() {
			t.Fatalf("degraded after %d failed operations, want %d", i, opFailuresToDegrade)
		}
	}
	_ = client.Get(ctx, "k").Err()
	if !h.Degraded() {
		t.Fatalf("not degraded after %d failed operations in a row", opFailuresToDegrade)
	}
	if got := errorsFor("other") - before; got != float64(opFailuresToDegrade) {
		t.Errorf("pad_redis_errors_total{other} grew by %v, want %d", got, opFailuresToDegrade)
	}
}

// A success between failures resets the streak; a miss (redis.Nil) is an answer, not a failure.
func TestHealth_SuccessAndMissesResetTheStreak(t *testing.T) {
	mr, client, h := newHealth(t)
	ctx := context.Background()
	h.failures.Store(opFailuresToDegrade - 1)
	if err := client.Get(ctx, "absent").Err(); !errors.Is(err, redis.Nil) {
		t.Fatalf("Get of a missing key = %v, want redis.Nil", err)
	}
	if got := h.failures.Load(); got != 0 {
		t.Errorf("failure streak after a miss = %d, want 0", got)
	}
	mr.Close()
	_ = client.Get(ctx, "k").Err()
	if h.Degraded() {
		t.Error("one failure degraded")
	}
}

// A caller that gave up says nothing about Redis.
func TestHealth_CancelledCallerIsNotAFailure(t *testing.T) {
	_, client, h := newHealth(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = client.Get(ctx, "k").Err()
	_ = client.Get(ctx, "k").Err()
	_ = client.Get(ctx, "k").Err()
	if h.Degraded() || h.failures.Load() != 0 {
		t.Errorf("cancelled calls counted: degraded %v, streak %d", h.Degraded(), h.failures.Load())
	}
}

// The hook names the mechanism by the key an operation touched: a plain command, a pipeline and an EVAL.
func TestHealth_ErrorsAreLabelledByMechanism(t *testing.T) {
	mr, client, _ := newHealth(t)
	ctx := context.Background()
	mr.Close()

	before := errorsFor("auth_revoked")
	_ = client.Publish(ctx, RevocationChannel.Key(), "x").Err()
	if got := errorsFor("auth_revoked") - before; got != 1 {
		t.Errorf("publish: pad_redis_errors_total{auth_revoked} grew by %v, want 1", got)
	}

	before = errorsFor("auth_revoked")
	_, _ = client.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, RevocationChannel.Key(), "x", time.Minute)
		return nil
	})
	if got := errorsFor("auth_revoked") - before; got != 1 {
		t.Errorf("pipeline: pad_redis_errors_total{auth_revoked} grew by %v, want 1 (one per pipeline)", got)
	}

	before = errorsFor("auth_revoked")
	_ = redis.NewScript(`return 1`).Run(ctx, client, []string{RevocationChannel.Key()}).Err()
	if got := errorsFor("auth_revoked") - before; got != 1 {
		t.Errorf("eval: pad_redis_errors_total{auth_revoked} grew by %v, want 1", got)
	}
}

// A script run starts with EVALSHA, which a server that has not cached the script refuses (NOSCRIPT) before the
// client falls back to EVAL: that is a healthy server, not an error to count.
func TestHealth_ScriptFallbackIsNotAnError(t *testing.T) {
	_, client, h := newHealth(t)
	before := errorsFor(MechanismOf(RevocationChannel.Key()))
	h.failures.Store(opFailuresToDegrade - 1)
	if err := redis.NewScript(`return 1`).Run(context.Background(), client, []string{RevocationChannel.Key()}).Err(); err != nil {
		t.Fatal(err)
	}
	if got := errorsFor(MechanismOf(RevocationChannel.Key())) - before; got != 0 {
		t.Errorf("pad_redis_errors_total grew by %v for a NOSCRIPT fallback, want 0", got)
	}
	if h.failures.Load() != 0 {
		t.Errorf("failure streak = %d after a working script run, want 0", h.failures.Load())
	}
}

func TestHealth_OnChangeFiresOncePerFlip(t *testing.T) {
	mr, _, h := newHealth(t)
	events := make(chan bool, 8)
	h.OnChange(func(up bool) { events <- up })
	ctx := context.Background()

	mr.Close()
	_ = h.Ping(ctx)
	_ = h.Ping(ctx) // still down: no second event
	if up := waitFor(t, "the down event", events); up {
		t.Fatal("first event reported up")
	}
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	if err := h.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if up := waitFor(t, "the up event", events); !up {
		t.Fatal("second event reported down")
	}
	select {
	case extra := <-events:
		t.Fatalf("unexpected extra event up=%v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// The ping loop notices an outage and a recovery by itself.
func TestHealth_RunPingsOnAnInterval(t *testing.T) {
	mr, _, h := newHealth(t)
	h.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var stopped atomic.Bool
	go func() { h.Run(ctx); stopped.Store(true) }()

	mr.Close()
	waitUntil(t, "degraded by the loop", h.Degraded)
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "recovered by the loop", func() bool { return !h.Degraded() })
	cancel()
	waitUntil(t, "the loop to stop", stopped.Load)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestErrorClass(t *testing.T) {
	for msg, want := range map[string]string{
		"dial tcp 10.0.0.1:6379: i/o timeout":                             "timeout",
		"context deadline exceeded":                                       "timeout",
		"dial tcp 127.0.0.1:1: connect: connection refused":               "connection refused",
		"failed to dial after 5 attempts: actively refused it":            "connection refused",
		"read tcp 10.0.0.2:5000->10.0.0.1:6379: connection reset by peer": "connection reset",
		"WRONGPASS invalid username-password pair":                        "auth failed",
		"something else": "unavailable",
	} {
		if got := errorClass(errors.New(msg)); got != want {
			t.Errorf("errorClass(%q) = %q, want %q", msg, got, want)
		}
	}
}
