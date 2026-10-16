package redisx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/metrics"
)

// A per-user cap across the replicas (chat streams, event streams, sockets, uploads). Each replica keeps its own cap
// as well, so a replica never holds more than its share of one user's resources; this adds the deployment-wide count
// that N replicas would otherwise multiply by N.
//
// One sorted set per user and cap (a family the cap's owner declares, KindZSet): a member per held resource, a random
// slot id, scored with the Unix ms at which its lease runs out. Leases are written and judged on the server's clock
// (TIME inside the scripts), so a replica whose clock is off cannot make its slots count longer, or another's shorter,
// than the lease. The holder rewrites the score every SlotRefreshEvery;
// a replica that dies stops rewriting, and its slots stop counting when their lease passes (SlotLease), with no one
// deleting anything: the next acquire for that user prunes them. The members carry no payload, so there is no
// envelope to version; the declared version 1 is the shape "member = slot id, score = expiry in Unix ms".
//
// The cap fails OPEN: an acquire that Redis does not answer (an error, the timeout, Health degraded) admits on the
// replica's own cap alone, and is counted (pad_redis_caps_total{outcome="open"}; the failed command itself in
// pad_redis_errors_total{mechanism}). A cap bounds what one account can take; it is not worth refusing a user for.

const (
	// SlotLease is how long a slot counts after its last refresh: the bound on how long a dead replica's slots keep
	// counting against their user.
	SlotLease = 30 * time.Second
	// SlotKeyTTL is the TTL of a cap's key, rewritten on every acquire and refresh: a set whose holders all died goes
	// away a little after their leases.
	SlotKeyTTL = 2 * SlotLease
	// slotOpTimeout bounds one acquire or release round trip: an event stream, a socket and a stream start wait for it.
	slotOpTimeout = 300 * time.Millisecond
)

// SlotRefreshEvery is how often a held slot's lease is rewritten: three refreshes per lease, so one lost write does not
// let a live slot lapse. A var for tests.
var SlotRefreshEvery = 10 * time.Second

var capsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pad_redis_caps_total",
	Help: "Per-user caps checked against the count across the replicas in Redis (deployments with PAD_MAX_REPLICAS > 1), by cap (the declared key family) and outcome: admitted, refused (the user holds the cap's limit across the replicas; answered as the replica's own cap answers) or open (Redis did not answer or is degraded, so the replica's own cap alone applied). A steady open rate means N replicas allow N times the cap.",
}, []string{"cap", "outcome"})

func init() { metrics.MustRegister(capsTotal) }

// acquireSlotScript prunes the lapsed members, refuses at the limit, else adds the new member and renews the key's
// TTL. KEYS[1] the set; ARGV: the lease (ms), the member, the limit, the key TTL (ms). Now is the server's.
var acquireSlotScript = redis.NewScript(`
local t = redis.call("time")
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call("zremrangebyscore", KEYS[1], "-inf", now)
if redis.call("zcard", KEYS[1]) >= tonumber(ARGV[3]) then return 0 end
redis.call("zadd", KEYS[1], now + tonumber(ARGV[1]), ARGV[2])
redis.call("pexpire", KEYS[1], ARGV[4])
return 1`)

// refreshSlotsScript rewrites the leases of the members still in the set (XX: a released one is not brought back) and
// renews the key's TTL. KEYS[1] the set; ARGV: the lease (ms), the key TTL (ms), the members.
var refreshSlotsScript = redis.NewScript(`
local t = redis.call("time")
local exp = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000) + tonumber(ARGV[1])
for i = 3, #ARGV do redis.call("zadd", KEYS[1], "XX", exp, ARGV[i]) end
redis.call("pexpire", KEYS[1], ARGV[2])
return 0`)

// SlotCap is one per-user cap shared by the replicas. A nil *SlotCap is a cap with no shared part (one replica, or no
// Redis): Acquire always admits, and the caller's own cap is the only one.
type SlotCap struct {
	client *redis.Client
	health *Health
	key    *Key
	limit  int
	every  time.Duration // the refresh interval, read once at construction

	mu      sync.Mutex
	held    map[string]string // slot member → the set it is in
	running bool              // the refresh loop is alive; it ends when nothing is held
}

// NewSlotCap is the cap of limit slots per holder on the sets of key (a KindZSet family). client nil is no shared cap
// (nil); health may be nil (never degraded).
func NewSlotCap(client *redis.Client, health *Health, key *Key, limit int) *SlotCap {
	if client == nil {
		return nil
	}
	return &SlotCap{client: client, health: health, key: key, limit: limit, every: SlotRefreshEvery, held: map[string]string{}}
}

func noRelease() {}

// Acquire takes one of holder's slots. ok is false when holder already has the limit across the replicas; then nothing
// is held. Otherwise release gives the slot back (idempotent). An empty holder, a nil cap and a Redis that does not
// answer admit (release is a no-op then).
func (c *SlotCap) Acquire(ctx context.Context, holder string) (release func(), ok bool) {
	return c.acquire(ctx, holder, true)
}

// AcquireWait is Acquire that, while ctx lives, asks again every poll until a slot is free (a slot another replica
// holds can only be asked for). The wait is one refusal in pad_redis_caps_total, not one per poll.
func (c *SlotCap) AcquireWait(ctx context.Context, holder string, poll time.Duration) (release func(), ok bool) {
	for counted := false; ; counted = true {
		if release, ok := c.acquire(ctx, holder, !counted); ok {
			return release, true
		}
		select {
		case <-ctx.Done():
			return noRelease, false
		case <-time.After(poll):
		}
	}
}

// acquire is Acquire; countRefusal false leaves a refusal out of pad_redis_caps_total (a wait's later polls).
func (c *SlotCap) acquire(ctx context.Context, holder string, countRefusal bool) (release func(), ok bool) {
	if c == nil || holder == "" {
		return noRelease, true
	}
	if c.health.Degraded() {
		capsTotal.WithLabelValues(c.key.Name(), "open").Inc()
		return noRelease, true
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	member := hex.EncodeToString(b[:])
	key := c.key.Key(holder)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slotOpTimeout)
	defer cancel()
	n, err := acquireSlotScript.Run(ctx, c.client, []string{key},
		SlotLease.Milliseconds(), member, c.limit, SlotKeyTTL.Milliseconds()).Int()
	if err != nil {
		capsTotal.WithLabelValues(c.key.Name(), "open").Inc()
		LimitedWarn("slot_cap:"+c.key.Name(), "redis: a per-user cap could not be checked across the replicas; the replica's own cap applies", err)
		return noRelease, true
	}
	if n == 0 {
		if countRefusal {
			capsTotal.WithLabelValues(c.key.Name(), "refused").Inc()
		}
		return noRelease, false
	}
	capsTotal.WithLabelValues(c.key.Name(), "admitted").Inc()
	c.mu.Lock()
	c.held[member] = key
	if !c.running {
		c.running = true
		go c.refreshLoop()
	}
	c.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { c.release(member) }) }, true
}

// release drops the slot here and in Redis. A failed delete is left to the lease.
func (c *SlotCap) release(member string) {
	c.mu.Lock()
	key, ok := c.held[member]
	delete(c.held, member)
	c.mu.Unlock()
	if !ok || c.health.Degraded() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), slotOpTimeout)
	defer cancel()
	_ = c.client.ZRem(ctx, key, member).Err()
}

// Held is how many slots this replica holds in c (tests and the state registry's proof).
func (c *SlotCap) Held() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.held)
}

// refreshLoop rewrites the lease of every held slot each tick and ends when none is held (the next acquire starts it
// again). The rewrite only updates members that are still there (ZADD XX): a slot released while the write was in
// flight is not brought back to count for a whole lease. A slot that was pruned meanwhile (this replica stalled past
// the lease) or evicted is then not counted until it is released, so the user can briefly hold more than the cap across
// the replicas; never less than the local cap allows.
func (c *SlotCap) refreshLoop() {
	t := time.NewTicker(c.every)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		if len(c.held) == 0 {
			c.running = false
			c.mu.Unlock()
			return
		}
		byKey := map[string][]any{}
		for member, key := range c.held {
			byKey[key] = append(byKey[key], member)
		}
		c.mu.Unlock()
		if c.health.Degraded() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), slotOpTimeout)
		pipe := c.client.Pipeline()
		for key, members := range byKey {
			// Eval, not Run: a pipeline cannot fall back from EVALSHA when the server has not cached the script.
			refreshSlotsScript.Eval(ctx, pipe, []string{key}, append([]any{SlotLease.Milliseconds(), SlotKeyTTL.Milliseconds()}, members...)...)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			LimitedWarn("slot_cap_refresh:"+c.key.Name(), "redis: the leases of this replica's per-user cap slots could not be refreshed; they lapse after "+strconv.Itoa(int(SlotLease/time.Second))+" s", err)
		}
		cancel()
	}
}
