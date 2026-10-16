package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/testutil/redisreplica"
)

// A missing liveness key must mean a gone replica (ADR 0033, addendum (close-out)): the key can go while its replica
// lives (Redis restarted from a snapshot older than the key's TTL, the replica cut off from Redis, an eviction). The
// three parts that keep "key gone" equal to "streams gone": the owner stops what it published once it cannot keep its
// key, it writes the key again as soon as it can, and the origin trusts a missing key only after its own view of Redis
// has been steady for a whole key TTL.

// fastLiveness makes the liveness writer tick every 20 ms, so a test sees the owner's reaction to an outage in time.
func fastLiveness(t *testing.T) {
	prev := redisx.LivenessEvery
	redisx.LivenessEvery = 20 * time.Millisecond
	t.Cleanup(func() { redisx.LivenessEvery = prev })
}

// An owner that cannot write its liveness key stops the streams it published before the key can lapse: another
// replica that then finds the key gone (and answers a stop ok) is right. A stream it started during the outage was
// never published, nobody can find it to call it gone, and it runs on (Redis down costs a single stream nothing).
func TestCommands_AnOwnerCutOffFromRedisStopsItsPublishedStreams(t *testing.T) {
	fastLiveness(t)
	c := redisreplica.New(t, 2)
	a := newSharedReplica(t, c.Replicas[0])
	published := startBlockedStream(t, a, "u1")
	c.Kill()
	redisreplica.Eventually(t, 3*time.Second, "the published stream stopped by its own replica", func() bool { return !streamRunning(a, published) })
	if ctl := waitStreamFinished(t, a, published); ctl.errCode != ChatErrInterrupted {
		t.Errorf("the fenced stream ended with %q, want %q", ctl.errCode, ChatErrInterrupted)
	}
	unpublished := uuid.NewString()
	if err := startStreamOn(t, a, "u2", unpublished); err != nil {
		t.Fatalf("a stream started with Redis down: %v (the start fails open)", err)
	}
	time.Sleep(10 * redisx.LivenessEvery)
	if !streamRunning(a, unpublished) {
		t.Error("a stream nobody else can know of was stopped by the fence")
	}
}

// When Redis comes back the owner writes its key at once (its command subscription is back), not on its next tick.
func TestLiveness_TheOwnerWritesItsKeyAgainWhenRedisComesBack(t *testing.T) {
	slowLiveness(t)
	c := redisreplica.New(t, 2)
	a := newSharedReplica(t, c.Replicas[0])
	key := redisx.ReplicaLiveKey.Key(a.cluster.instance)
	c.Kill()
	c.Redis.FastForward(redisx.LivenessTTL + time.Second) // Redis comes back from a snapshot older than the key's TTL
	c.Restore(t)
	if c.Redis.Exists(key) {
		t.Fatal("premise: the key lapsed during the outage")
	}
	redisreplica.Eventually(t, 8*time.Second, "the owner's key written again after the outage", func() bool { return c.Redis.Exists(key) })
}

// A stop that lands on another replica right after Redis came back, while the owner has neither resubscribed nor
// written its key again, is a retryable 503: the owner is alive and its stream runs. Before, it was answered ok and
// the stream's snapshot deleted, while the stream ran on and billed for up to ten minutes.
func TestCommands_AStopRightAfterRedisCameBackIsNotADeadOwner(t *testing.T) {
	slowLiveness(t)
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	c.Kill()
	c.Redis.FastForward(redisx.LivenessTTL + time.Second) // the keys lapsed; the 12 minute snapshot did not
	a.cluster.bus.Stop()                                  // A's subscription is still on its reconnect backoff
	c.Restore(t)
	redisreplica.Eventually(t, 8*time.Second, "B subscribed again", b.cluster.bus.Ready)
	freshConns(t, c.Replicas[1])
	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("a stop right after the outage = %v, want ErrReplicaUnreachable (the owner is alive)", err)
	}
	if !streamRunning(a, id) {
		t.Fatal("premise: the stream still runs on A")
	}
	if !c.Redis.Exists(resumeKey.Key(id)) {
		t.Error("the snapshot of a running stream was deleted as a dead owner's")
	}
}

// freshConns retires the pooled connections of rep's client that the outage broke (each fails its next command once),
// so the scenario's commands meet Redis and not a dead socket.
func freshConns(t *testing.T, rep *redisreplica.Replica) {
	t.Helper()
	ok := 0
	for i := 0; i < 100 && ok < 10; i++ {
		if rep.Client.Ping(context.Background()).Err() == nil {
			ok++
		}
	}
	if ok < 10 {
		t.Fatal("the client did not get through to the restored Redis")
	}
}

// The origin believes a missing key only on a steady view of Redis: within a TTL of its own subscriptions coming up,
// or of its Redis recovering from degraded, a dead-looking owner is a 503 and its snapshot stays; once steady, the
// owner whose key lapsed is gone.
func TestCommands_AMissingKeyIsBelievedOnlyOnASteadyView(t *testing.T) {
	slowLiveness(t)
	c := redisreplica.New(t, 2)
	a, b := newSharedReplica(t, c.Replicas[0]), newSharedReplica(t, c.Replicas[1])
	id := startBlockedStream(t, a, "u1")
	killReplica(a)
	c.Redis.FastForward(redisx.LivenessTTL + time.Second)

	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("a stop within a TTL of B's connecting = %v, want ErrReplicaUnreachable", err)
	}
	// B's subscriptions have been up a TTL, but its Redis was degraded until a moment ago (the stop's own reads are
	// what recover it).
	subscribed, _ := b.cluster.bus.UpSince()
	time.Sleep(time.Second)
	for i := 0; i < 3; i++ {
		c.Replicas[1].Health.RecordError("test", errors.New("unreachable"))
	}
	b.cluster.now = func() time.Time { return subscribed.Add(redisx.LivenessTTL + 500*time.Millisecond) }
	if err := b.CancelStreamAs(context.Background(), "u1", id); !errors.Is(err, ErrReplicaUnreachable) {
		t.Fatalf("a stop within a TTL of B's Redis recovering = %v, want ErrReplicaUnreachable", err)
	}
	if !c.Redis.Exists(resumeKey.Key(id)) {
		t.Fatal("the snapshot went without a steady view")
	}
	steadyView(b)
	if err := b.CancelStreamAs(context.Background(), "u1", id); err != nil {
		t.Fatalf("a stop on a steady view once the owner's key lapsed = %v, want nil", err)
	}
}
