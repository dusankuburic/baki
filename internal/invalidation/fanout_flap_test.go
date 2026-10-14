package invalidation

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/testutil/redisreplica"
)

// A Redis that drops connections every few seconds must not turn into a resync every few seconds: each resync empties
// every replicated cache and has every socket re-check its authorisation against the database (REVIEW_REDIS M4, L5).

// After a resubscribe the receiver's view of each peer's numbering starts over: the resync that follows the
// subscription already covers what was missed, so the first message of a peer afterwards is a baseline, not a gap
// against the numbers seen before the outage.
func TestFanout_AResubscribeStartsTheEpochTrackingOver(t *testing.T) {
	c, ps := cluster(t, 2)
	a, b := ps[0], ps[1]

	c.Kill()
	_ = a.Health.Ping(context.Background())
	if !a.Health.Degraded() {
		t.Fatal("premise: A's Redis is degraded")
	}
	// A's writes during the outage take their numbers but cannot be sent: B would see a jump.
	skipped := counter(dropped, "redis_down")
	for _, id := range []string{"1", "2", "3"} {
		a.bus.Publish(context.Background(), probeEvent{ID: id})
	}
	redisreplica.Eventually(t, 5*time.Second, "A's writes to be counted as dropped", func() bool { return counter(dropped, "redis_down")-skipped >= 3 })

	gaps, gapResyncs, subscribes := counter(gapsTotal), counter(resyncsTotal, "gap"), counter(resyncsTotal, "subscribe")
	c.Restore(t)
	_ = a.Health.Ping(context.Background())
	subscribed(t, c, 2)
	redisreplica.Eventually(t, 5*time.Second, "both replicas' resubscribe resyncs", func() bool { return counter(resyncsTotal, "subscribe")-subscribes >= 2 })

	b.cache.fill("x")
	a.bus.Publish(context.Background(), probeEvent{ID: "x"})
	redisreplica.Eventually(t, 5*time.Second, "B to receive A's first message after the outage", func() bool { return !b.cache.has("x") })
	time.Sleep(100 * time.Millisecond)
	if got := counter(gapsTotal) - gaps; got != 0 {
		t.Errorf("%v gap(s) counted for the first message after a resubscribe, want 0: the subscribe resync already covered what was missed", got)
	}
	if got := counter(resyncsTotal, "gap") - gapResyncs; got != 0 {
		t.Errorf("%v redundant gap resync(s) followed the resubscribe's", got)
	}
}

// Resyncs that keep being asked for back off: the gap between two doubles while requests keep arriving inside it, up
// to a ceiling, and returns to the base once a gap passes with none.
func TestFanout_ResyncsBackOffWhileTheyKeepBeingAsked(t *testing.T) {
	r := redisreplica.New(t, 1).Replicas[0]
	p := newPeer(r, WithResyncGap(10*time.Millisecond), WithResyncGapMax(80*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.fan.resyncLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	const reason = "flap-test"
	before := counter(resyncsTotal, reason)
	flapUntil := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(flapUntil) {
		p.fan.requestResync(reason)
		time.Sleep(2 * time.Millisecond)
	}
	got := counter(resyncsTotal, reason) - before
	// A fixed 10 ms gap would run about 60; the doubling gap (10, 20, 40, then 80) about a dozen.
	if got < 3 || got > 16 {
		t.Errorf("%v resyncs in 600 ms of constant requests, want a few (3 to 16): the gap must grow while they keep coming", got)
	}

	// Quiet for longer than the ceiling: the gap is back at its base.
	time.Sleep(250 * time.Millisecond)
	before = counter(resyncsTotal, reason)
	p.fan.requestResync(reason)
	redisreplica.Eventually(t, time.Second, "the first resync after quiet", func() bool { return counter(resyncsTotal, reason)-before >= 1 })
	p.fan.requestResync(reason)
	start := time.Now()
	redisreplica.Eventually(t, time.Second, "the second resync after quiet", func() bool { return counter(resyncsTotal, reason)-before >= 2 })
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("the second resync after a quiet spell took %v, want about the base gap of 10 ms: the backoff did not reset", d)
	}
}
