package invalidation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/testutil/redisreplica"
)

// The two-replica tests: each "replica" has its own bus, its own cache and its own fan-out on its own client, all
// on one Redis (internal/testutil/redisreplica). A write on one replica publishes the probe event the way a service
// does; what the other replica's cache holds afterwards is the observable.

type probeEvent struct{ ID string }

func (probeEvent) EventName() string { return "probe.changed" }

// probeCache is a replica's cache of the probe key: ids to values, dropped by id or whole.
type probeCache struct {
	mu      sync.Mutex
	m       map[string]string
	applied int // remote applications
}

func (c *probeCache) fill(ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		c.m[id] = "v"
	}
}

func (c *probeCache) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.m[id]
	return ok
}

func (c *probeCache) applications() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applied
}

func (c *probeCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func (c *probeCache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

type peer struct {
	*redisreplica.Replica
	bus   *eventbus.Bus
	cache *probeCache
	fan   *Fanout
	stop  func(context.Context)
}

// newPeer wires a replica as the app does: a bus with a replicated subscriber for the probe key and its applier.
// The fan-out is built, not started.
func newPeer(r *redisreplica.Replica, opts ...Option) *peer {
	p := &peer{Replica: r, bus: eventbus.New(nil, time.Second), cache: &probeCache{m: map[string]string{}}}
	eventbus.Subscribe(p.bus, eventbus.Sync, "probe-cache", func(_ context.Context, e probeEvent) { p.cache.drop(e.ID) },
		eventbus.Replicated(func(e probeEvent) []eventbus.Invalidation {
			return []eventbus.Invalidation{{Key: "cache.probe", Scope: eventbus.NewScope("id", e.ID)}}
		}, "cache.probe"))
	p.bus.OnRemote("cache.probe", func(s eventbus.Scope) {
		p.cache.mu.Lock()
		defer p.cache.mu.Unlock()
		p.cache.applied++
		if len(s) == 0 {
			clear(p.cache.m)
			return
		}
		delete(p.cache.m, s["id"])
	})
	// Short timings, so a test waits for milliseconds, not seconds.
	opts = append([]Option{WithRetry(5*time.Millisecond, 20*time.Millisecond), WithHeartbeat(time.Hour), WithResyncGap(time.Millisecond), WithResyncGapMax(20 * time.Millisecond)}, opts...)
	p.fan = New(r.Client, r.Health, p.bus, r.ID, opts...)
	return p
}

func startPeer(t *testing.T, r *redisreplica.Replica, opts ...Option) *peer {
	t.Helper()
	p := newPeer(r, opts...)
	p.stop = p.fan.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		p.stop(ctx)
	})
	return p
}

func cluster(t *testing.T, n int, opts ...Option) (*redisreplica.Cluster, []*peer) {
	t.Helper()
	c := redisreplica.New(t, n)
	before := counter(resyncsTotal, "subscribe")
	peers := make([]*peer, n)
	for i, r := range c.Replicas {
		peers[i] = startPeer(t, r, opts...)
	}
	subscribed(t, c, n)
	settled(t, n, before)
	return c, peers
}

// settled waits for the resync every subscription starts with (it drops the replicas' caches, so a test that fills
// one first would see its entries vanish): n of them past before, the subscribe counter when the test began.
func settled(t *testing.T, n int, before float64) {
	t.Helper()
	redisreplica.Eventually(t, 5*time.Second, "the resync that follows each subscription", func() bool {
		return counter(resyncsTotal, "subscribe")-before >= float64(n)
	})
	time.Sleep(20 * time.Millisecond) // the appliers run just after the counter moves
}

// subscribed waits until n fan-outs follow the channel: a message published earlier would be lost, as in production.
func subscribed(t *testing.T, c *redisreplica.Cluster, n int) {
	t.Helper()
	redisreplica.Eventually(t, 5*time.Second, "every replica subscribed", func() bool {
		return c.Redis.PubSubNumSub(channel.Key())[channel.Key()] == n
	})
}

func counter(v *prometheus.CounterVec, labels ...string) float64 {
	return testutil.ToFloat64(v.WithLabelValues(labels...))
}

// tapChannel follows the channel as a third party would, returning the payloads in order.
func tapChannel(t *testing.T, client *redis.Client) <-chan string {
	t.Helper()
	sub := client.Subscribe(context.Background(), channel.Key())
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 64)
	go func() {
		for msg := range sub.Channel() {
			out <- msg.Payload
		}
	}()
	return out
}

func next(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a message on the channel")
		return ""
	}
}

// A committed write on A drops B's entry (and only that entry); A's own entry went with the local subscriber, and
// the message A published is not applied to A again.
func TestFanout_AWriteOnAInvalidatesBsCache(t *testing.T) {
	_, ps := cluster(t, 2)
	a, b := ps[0], ps[1]
	a.cache.fill("x", "y")
	b.cache.fill("x", "y")
	appliedBefore := a.cache.applications()

	a.bus.Publish(context.Background(), probeEvent{ID: "x"})

	redisreplica.Eventually(t, 5*time.Second, "B to drop x", func() bool { return !b.cache.has("x") })
	if !b.cache.has("y") {
		t.Error("B dropped y too: the scope named x only")
	}
	if a.cache.has("x") {
		t.Error("A's own entry survived its own write")
	}
	if n := a.cache.applications() - appliedBefore; n != 0 {
		t.Errorf("A applied %d remote invalidations of its own write, want 0 (it drops its origin's messages)", n)
	}
}

// Inside a request's transaction the message waits for the commit and never goes out for a rollback.
func TestFanout_PublishesOnlyWhatCommitted(t *testing.T) {
	_, ps := cluster(t, 2)
	a, b := ps[0], ps[1]
	b.cache.fill("x", "z")
	before := counter(published, "inval")

	ctx, reg := txhook.With(context.Background())
	a.bus.Publish(ctx, probeEvent{ID: "x"})
	reg.Abort()
	time.Sleep(150 * time.Millisecond)
	if !b.cache.has("x") {
		t.Error("B dropped an entry for a rolled-back write")
	}
	if d := counter(published, "inval") - before; d != 0 {
		t.Errorf("published %v messages for a rolled-back write", d)
	}

	ctx, reg = txhook.With(context.Background())
	a.bus.Publish(ctx, probeEvent{ID: "z"})
	time.Sleep(150 * time.Millisecond)
	if !b.cache.has("z") {
		t.Fatal("B dropped an entry before the write committed")
	}
	reg.Commit(nil)
	redisreplica.Eventually(t, 5*time.Second, "B to drop z after the commit", func() bool { return !b.cache.has("z") })
}

// What is on the wire: the envelope of ADR 0029 with the key, the scope, the origin and the sender's epoch, and
// rechecks as their own kind on the same channel, numbered by the same epoch.
func TestFanout_WireFormat(t *testing.T) {
	c, ps := cluster(t, 1)
	a := ps[0]
	eventbus.Subscribe(a.bus, eventbus.Async, "probe-recheck", func(context.Context, probeEvent) {},
		eventbus.Replicated(func(e probeEvent) []eventbus.Invalidation {
			return []eventbus.Invalidation{{Key: "recheck.probe", Scope: eventbus.NewScope("what", "user", "id", e.ID)}}
		}, "recheck.probe"))
	tap := tapChannel(t, c.Replicas[0].Client)

	a.bus.Publish(context.Background(), probeEvent{ID: "x"})

	// The replicated subscribers run in registration order, so the cache key is first.
	for i, want := range []string{
		`{"v":1,"kind":"inval","key":"cache.probe","scope":{"id":"x"},"epoch":1,"o":"replica-1"}`,
		`{"v":1,"kind":"recheck","key":"recheck.probe","scope":{"id":"x","what":"user"},"epoch":2,"o":"replica-1"}`,
	} {
		got := next(t, tap)
		// The origin is the instance id and a random suffix per boot.
		got = regexp.MustCompile(`"o":"replica-1#[0-9a-f]{8}"`).ReplaceAllString(got, `"o":"replica-1"`)
		if !sameJSON(t, got, want) {
			t.Errorf("message %d = %s, want %s", i+1, got, want)
		}
	}
}

func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatalf("not JSON: %s", a)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return reflect.DeepEqual(x, y)
}

// An idle replica says where its numbering stands, so a peer that lost the last message of a burst notices.
func TestFanout_HeartbeatsCarryTheSendersEpoch(t *testing.T) {
	c := redisreplica.New(t, 1)
	a := startPeer(t, c.Replicas[0], WithHeartbeat(20*time.Millisecond))
	subscribed(t, c, 1)
	tap := tapChannel(t, c.Replicas[0].Client)

	a.bus.Publish(context.Background(), probeEvent{ID: "x"})

	for {
		var m struct {
			Kind  string `json:"kind"`
			Epoch uint64 `json:"epoch"`
			O     string `json:"o"`
		}
		if err := json.Unmarshal([]byte(next(t, tap)), &m); err != nil {
			t.Fatal(err)
		}
		if m.Kind == "inval" {
			continue
		}
		if m.Kind != "hb" || !strings.HasPrefix(m.O, "replica-1#") {
			t.Fatalf("unexpected message %+v", m)
		}
		if m.Epoch == 1 {
			return // the heartbeat after the write names the write's epoch
		}
		if m.Epoch != 0 {
			t.Fatalf("heartbeat epoch = %d, want 0 (before the write) or 1 (after)", m.Epoch)
		}
	}
}

// publishRaw puts a message on the channel as some other replica (or release) would.
func publishRaw(t *testing.T, r *redisreplica.Replica, payload string) {
	t.Helper()
	if err := r.Client.Publish(context.Background(), channel.Key(), payload).Err(); err != nil {
		t.Fatal(err)
	}
}

// A message that never arrived is noticed by the next one from the same origin (its epoch jumps) and by the
// origin's heartbeat (its epoch is ahead of the last message seen), and either way the receiver drops every
// replicated cache: it cannot know which entries the lost message concerned.
func TestFanout_AMissedMessageIsHealedByTheEpochGapCheck(t *testing.T) {
	_, ps := cluster(t, 2)
	b := ps[1]
	ghost := func(kind string, epoch int, key string) string {
		return fmt.Sprintf(`{"v":1,"kind":%q,"key":%q,"scope":{"id":"seen"},"epoch":%d,"o":"ghost"}`, kind, key, epoch)
	}
	gaps := counter(gapsTotal)
	resyncs := counter(resyncsTotal, "gap")

	b.cache.fill("seen", "other")
	publishRaw(t, ps[0].Replica, ghost("hb", 0, ""))
	publishRaw(t, ps[0].Replica, ghost("inval", 1, "cache.probe"))
	redisreplica.Eventually(t, 5*time.Second, "the in-sequence message", func() bool { return !b.cache.has("seen") })
	if !b.cache.has("other") {
		t.Fatal("an in-sequence message dropped the whole cache")
	}
	if counter(gapsTotal) != gaps {
		t.Fatal("a gap was counted for messages in sequence")
	}

	// Epoch 2 is lost; 3 arrives.
	publishRaw(t, ps[0].Replica, ghost("inval", 3, "cache.probe"))
	redisreplica.Eventually(t, 5*time.Second, "the resync after a skipped epoch", func() bool { return !b.cache.has("other") })
	// Both replicas follow the channel, so each notices the skipped epoch.
	if counter(gapsTotal)-gaps != 2 || counter(resyncsTotal, "gap")-resyncs != 2 {
		t.Errorf("gap +%v resync +%v, want one per replica", counter(gapsTotal)-gaps, counter(resyncsTotal, "gap")-resyncs)
	}

	// The last message of a burst is lost: only the heartbeat, which carries the sender's epoch, shows it.
	b.cache.fill("other")
	publishRaw(t, ps[0].Replica, ghost("hb", 3, ""))
	time.Sleep(100 * time.Millisecond)
	if !b.cache.has("other") {
		t.Fatal("a heartbeat at the epoch already seen caused a resync")
	}
	publishRaw(t, ps[0].Replica, ghost("hb", 5, ""))
	redisreplica.Eventually(t, 5*time.Second, "the resync after a heartbeat ahead of the last message", func() bool { return !b.cache.has("other") })
}

// A peer that asks for a resync (its own queue overflowed) makes this replica drop everything.
func TestFanout_APeersResyncRequestIsHonoured(t *testing.T) {
	_, ps := cluster(t, 2)
	b := ps[1]
	b.cache.fill("x")
	publishRaw(t, ps[0].Replica, `{"v":1,"kind":"resync","epoch":1,"o":"overflowed"}`)
	redisreplica.Eventually(t, 5*time.Second, "B to drop everything", func() bool { return b.cache.size() == 0 })
}

// Redis away, then back: the writes made meanwhile reach nobody, the writer does not wait, and the peers drop every
// cache when their subscription is re-established, which is the TTL's backstop made prompt.
func TestFanout_ResubscribeHealsWhatWasMissedWhileRedisWasDown(t *testing.T) {
	c, ps := cluster(t, 2)
	a, b := ps[0], ps[1]
	b.cache.fill("x", "y")

	c.Kill()
	start := time.Now()
	a.bus.Publish(context.Background(), probeEvent{ID: "x"})
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("the writer waited %v on a dead Redis", d)
	}
	time.Sleep(100 * time.Millisecond)
	if !b.cache.has("x") {
		t.Fatal("B dropped x with Redis down: nothing could have told it")
	}

	resyncs := counter(resyncsTotal, "subscribe")
	c.Restore(t)
	redisreplica.Eventually(t, 5*time.Second, "B to resubscribe and drop everything", func() bool { return b.cache.size() == 0 })
	if counter(resyncsTotal, "subscribe")-resyncs < 1 {
		t.Error("the resync was not counted as a resubscribe's")
	}
}

// The health monitor's recovery resyncs too (a subscription that never noticed the outage), and an outage costs a
// writer nothing: the messages are counted as dropped, not retried.
func TestFanout_RedisDownIsTTLOnlyAndRecoveryResyncs(t *testing.T) {
	c, ps := cluster(t, 2)
	a, b := ps[0], ps[1]
	b.cache.fill("x")

	c.Kill()
	_ = a.Health.Ping(context.Background()) // the health loop's verdict: degraded
	if !a.Health.Degraded() {
		t.Fatal("premise: A's Redis is degraded")
	}
	before := counter(dropped, "redis_down")
	a.bus.Publish(context.Background(), probeEvent{ID: "x"})
	redisreplica.Eventually(t, 5*time.Second, "the message to be counted as dropped", func() bool {
		return counter(dropped, "redis_down")-before >= 1
	})
	if !b.cache.has("x") {
		t.Error("B dropped x: the outage should leave it to the TTL")
	}

	resyncs := counter(resyncsTotal, "health")
	c.Restore(t)
	_ = a.Health.Ping(context.Background())
	redisreplica.Eventually(t, 5*time.Second, "A's health recovery to request a resync", func() bool {
		return counter(resyncsTotal, "health")-resyncs >= 1
	})
}

// Unknown versions and kinds, junk, and keys this release has no applier for are counted and ignored; the receiver
// keeps working and nothing is deleted.
func TestFanout_IgnoresWhatItDoesNotKnow(t *testing.T) {
	_, ps := cluster(t, 2)
	b := ps[1]
	b.cache.fill("x")
	unknownKey := counter(ignored, "unknown_key")
	unknownMsg := redisMsgCount("inval", "unknown")
	bad := redisMsgCount("inval", "bad")
	for _, payload := range []string{
		`{"v":2,"kind":"inval","key":"cache.probe","scope":{"id":"x"},"epoch":1,"o":"newer"}`,  // a newer version
		`{"v":1,"kind":"galaxy","key":"cache.probe","scope":{"id":"x"},"epoch":1,"o":"newer"}`, // a newer kind
		`not json`,
		`{"v":1,"kind":"inval","key":"cache.nobody-has-this","epoch":1,"o":"newer"}`,             // a cache this release lacks
		`{"v":1,"kind":"inval","key":"cache.probe","scope":{"id":["x"]},"epoch":1,"o":"newer"}`,  // wrong field type
		`{"v":1,"kind":"inval","key":"cache.probe","scope":{"id":"x"},"epoch":1}`,                // no origin
		`{"v":1,"kind":"inval","key":"` + strings.Repeat("k", 5000) + `","epoch":1,"o":"newer"}`, // oversized
	} {
		publishRaw(t, ps[0].Replica, payload)
	}
	publishRaw(t, ps[0].Replica, `{"v":1,"kind":"inval","key":"cache.probe","scope":{"id":"x"},"epoch":1,"o":"fine"}`)
	redisreplica.Eventually(t, 5*time.Second, "the good message after the junk", func() bool { return !b.cache.has("x") })
	// Both replicas follow the channel, so each counts the cache this release lacks.
	if counter(ignored, "unknown_key")-unknownKey != 2 {
		t.Errorf("unknown_key ignored +%v, want 2 (one per replica)", counter(ignored, "unknown_key")-unknownKey)
	}
	if redisMsgCount("inval", "unknown")-unknownMsg < 2 {
		t.Errorf("unknown payloads counted +%v, want at least 2", redisMsgCount("inval", "unknown")-unknownMsg)
	}
	if redisMsgCount("inval", "bad")-bad < 2 {
		t.Errorf("bad payloads counted +%v, want at least 2", redisMsgCount("inval", "bad")-bad)
	}
}

// Applying a peer's message republishes nothing: an invalidation cannot loop between replicas.
func TestFanout_ApplyingARemoteMessageNeverPublishes(t *testing.T) {
	_, ps := cluster(t, 2)
	a, b := ps[0], ps[1]
	b.cache.fill("x")
	before := counter(published, "inval")
	a.bus.Publish(context.Background(), probeEvent{ID: "x"})
	redisreplica.Eventually(t, 5*time.Second, "B to apply", func() bool { return !b.cache.has("x") })
	time.Sleep(100 * time.Millisecond)
	if d := counter(published, "inval") - before; d != 1 {
		t.Errorf("%v messages published for one write, want 1", d)
	}
}

// A replica that is stopping still sends what it accepted: the writes of the last seconds of a draining replica
// are the ones its peers most need to hear of.
func TestFanout_StopFlushesTheQueue(t *testing.T) {
	c := redisreplica.New(t, 2)
	before := counter(resyncsTotal, "subscribe")
	b := startPeer(t, c.Replicas[1])
	subscribed(t, c, 1)
	settled(t, 1, before)
	b.cache.fill("x")

	a := newPeer(c.Replicas[0])
	stop := a.fan.Start(context.Background())
	subscribed(t, c, 2)
	a.bus.Publish(context.Background(), probeEvent{ID: "x"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stop(ctx)
	redisreplica.Eventually(t, 5*time.Second, "B to hear the write A made just before stopping", func() bool { return !b.cache.has("x") })
}

// Without a Redis client there is no fan-out: nothing is installed, and a bus publishes nothing across replicas.
func TestInstall_WithoutRedisInstallsNothing(t *testing.T) {
	bus := eventbus.New(nil, time.Second)
	stop := Install(nil, redisx.NewHealth(nil), bus, "replica-1")
	rec := &eventbus.ReplicasRecorder{}
	bus.SetReplicas(rec) // nothing was installed that this would replace
	stop(context.Background())
}

// redisMsgCount reads pad_redis_msgs_total{kind,result}, which internal/redisx owns.
func redisMsgCount(kind, result string) float64 {
	families, err := metrics.Gather()
	if err != nil {
		return -1
	}
	for _, f := range families {
		if f.GetName() != "pad_redis_msgs_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["kind"] == kind && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// Two replicas configured with the same instance id (a copied PAD_INSTANCE_ID) must not take each other's messages
// for their own echo: the origin carries a boot suffix.
func TestFanout_TheSameInstanceIDOnTwoReplicasStillFansOut(t *testing.T) {
	c := redisreplica.New(t, 2)
	before := counter(resyncsTotal, "subscribe")
	mk := func(r *redisreplica.Replica) *peer {
		p := newPeer(r)
		p.fan = New(r.Client, r.Health, p.bus, "same-name", WithRetry(5*time.Millisecond, 20*time.Millisecond), WithHeartbeat(time.Hour), WithResyncGap(time.Millisecond))
		p.stop = p.fan.Start(context.Background())
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			p.stop(ctx)
		})
		return p
	}
	a, b := mk(c.Replicas[0]), mk(c.Replicas[1])
	subscribed(t, c, 2)
	settled(t, 2, before)
	b.cache.fill("x")
	a.bus.Publish(context.Background(), probeEvent{ID: "x"})
	redisreplica.Eventually(t, 5*time.Second, "B to drop x", func() bool { return !b.cache.has("x") })
}
