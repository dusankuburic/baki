// Package invalidation carries cache invalidations and access rechecks between the replicas of a deployment over
// Redis pub/sub (Phase 5b; docs/adr/0032-cache-invalidation-fanout-and-state-registry.md).
//
// The domain event bus delivers in process. A replicated subscriber (eventbus.Replicated) hands the bus the
// invalidation keys its event concerns, and the bus gives each one to the Replicas hook once the write is durable:
// this package's Fanout is that hook. It queues the key, a goroutine publishes it on the channel baki:inval, and
// every other replica's Fanout hands what it receives to Bus.DeliverRemote, which runs the cache owner's applier
// (Bus.OnRemote). Only keys and their tenant scope travel, never an event or free text.
//
// Fan-out is best effort and never blocks a commit; staleness is bounded three ways, in order of strength:
//   - every replica numbers its messages, and a peer that sees a number skipped (or a heartbeat ahead of the last
//     message) drops every replicated cache and re-checks its sockets: a resync;
//   - a resync also follows every (re)subscription and every recovery of Redis, so whatever was published while a
//     replica was not listening is covered when it listens again;
//   - the caches' own TTLs, which is all there is while Redis is down, and all an older release that does not
//     subscribe has.
package invalidation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"
	"pad-core/logger"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/redisx"
)

// The payload kinds of the channel (redisx.Spec.Kinds) and the version this release writes and reads.
const (
	kindInval     = "inval"   // drop part of a cache
	kindRecheck   = "recheck" // re-check the sockets and streams an access change can reach
	kindHeartbeat = "hb"      // the sender's last epoch, so a lost last message shows
	kindResync    = "resync"  // drop everything: the sender's queue overflowed
	payloadV1     = 1
)

const (
	recheckPrefix = "recheck."  // invalidation keys that are rechecks, not caches
	maxMessage    = 4 << 10     // a larger message is not one of ours
	maxScopeParts = 8           // parts in a scope
	maxPartLen    = 512         // bytes in a scope key or value
	publishWait   = time.Second // one PUBLISH
)

// channel is the one pub/sub channel of the fan-out. It carries four kinds, each decoded and counted here
// (pad_redis_msgs_total{kind}). It is declared with OnDown open: while Redis is away the caches' TTLs are the bound.
var channel = redisx.Declare(redisx.Spec{
	Name:    "inval.fanout",
	Kind:    redisx.KindPubSub,
	Pattern: "baki:inval",
	Version: payloadV1,
	Kinds:   []string{kindInval, kindRecheck, kindHeartbeat, kindResync},
	OnDown:  redisx.OnDownOpen,
	Owner:   "internal/invalidation",
})

// message is the wire form: the envelope {"v":1,"kind":...} with the key, its scope, the sender's epoch and the
// sender's origin ("o": its instance id and a boot suffix). Fields are only ever added.
type message struct {
	redisx.Envelope
	Key    string            `json:"key,omitempty"`
	Scope  map[string]string `json:"scope,omitempty"`
	Epoch  uint64            `json:"epoch"`
	Origin string            `json:"o"`
}

// pending is a message waiting to be sent.
type pending struct {
	kind  string
	key   string
	scope eventbus.Scope
}

func (p pending) same(q pending) bool {
	return p.kind == q.kind && p.key == q.key && maps.Equal(p.scope, q.scope)
}

// Option tunes a Fanout (the defaults are the production values; tests shorten them).
type Option func(*Fanout)

// WithRetry sets the back-off between attempts to subscribe: it starts at min and doubles up to max.
func WithRetry(min, max time.Duration) Option {
	return func(f *Fanout) { f.retryMin, f.retryMax = min, max }
}

// WithHeartbeat sets how often a replica announces its epoch.
func WithHeartbeat(d time.Duration) Option { return func(f *Fanout) { f.hbEvery = d } }

// WithResyncGap sets the least time between two resyncs; requests that arrive meanwhile run as one afterwards.
func WithResyncGap(d time.Duration) Option { return func(f *Fanout) { f.resyncGap = d } }

// WithResyncGapMax sets the ceiling the gap between resyncs grows to while resyncs keep being asked for.
func WithResyncGapMax(d time.Duration) Option { return func(f *Fanout) { f.resyncGapMax = d } }

// WithQueueLimit sets how many messages may wait before a burst collapses into one resync.
func WithQueueLimit(n int) Option { return func(f *Fanout) { f.queueLimit = n } }

// Fanout is one replica's end of the invalidation channel: it publishes the invalidations the bus hands it
// (eventbus.Replicas) and applies the ones the other replicas publish.
type Fanout struct {
	client *redis.Client
	health *redisx.Health
	bus    *eventbus.Bus
	// origin names this process on the wire: the instance id and a random suffix, so two replicas configured with
	// the same instance id (a copied PAD_INSTANCE_ID) still tell their own messages from each other's, and a
	// restarted replica is a new origin with its numbering from one.
	origin string

	retryMin, retryMax time.Duration
	hbEvery            time.Duration
	resyncGap          time.Duration
	resyncGapMax       time.Duration
	queueLimit         int

	// The publish side. queue is guarded by mu; epoch is the publishing goroutine's alone.
	mu    sync.Mutex
	queue []pending
	wake  chan struct{}
	epoch uint64

	// The receive side: resyncReq coalesces requests for a resync; track is the subscription goroutine's alone.
	resyncReq chan string
	track     *tracker

	stopCh   chan struct{}
	flushCtx context.Context
	warnedAt time.Time
	warnMu   sync.Mutex
}

// New builds a fan-out for the replica instance. client may be nil in a test that only exercises the queue; a real
// one is started with Start.
func New(client *redis.Client, health *redisx.Health, bus *eventbus.Bus, instance string, opts ...Option) *Fanout {
	f := &Fanout{
		client: client, health: health, bus: bus, origin: instance + "#" + bootNonce(),
		retryMin: 500 * time.Millisecond, retryMax: 10 * time.Second,
		hbEvery: 10 * time.Second, resyncGap: 5 * time.Second, resyncGapMax: time.Minute, queueLimit: 512,
		wake: make(chan struct{}, 1), resyncReq: make(chan string, 1), track: newTracker(), stopCh: make(chan struct{}),
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// bootNonce is a random suffix for this process's origin.
func bootNonce() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0"
	}
	return hex.EncodeToString(b[:])
}

// PublishInvalidation satisfies eventbus.Replicas: queue the invalidation for the publishing goroutine. It never
// waits and never fails the writer; a recheck key goes out as kind recheck, a cache key as inval. An identical
// message already waiting absorbs it. At the queue limit the burst collapses into one resync, which reaches the
// peers after every write it replaces committed.
func (f *Fanout) PublishInvalidation(_ context.Context, key string, scope eventbus.Scope) {
	kind := kindInval
	if strings.HasPrefix(key, recheckPrefix) {
		kind = kindRecheck
	}
	p := pending{kind: kind, key: key}
	if len(scope) > 0 {
		p.scope = eventbus.Scope(maps.Clone(scope))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(f.queue) > 0 && f.queue[0].kind == kindResync:
		// A resync waits at the head and covers every write before it is sent.
		dropped.WithLabelValues("queue_full").Inc()
		return
	case len(f.queue) >= f.queueLimit:
		dropped.WithLabelValues("queue_full").Add(float64(len(f.queue) + 1))
		f.queue = append(f.queue[:0], pending{kind: kindResync})
	default:
		for _, q := range f.queue {
			if q.same(p) {
				return
			}
		}
		f.queue = append(f.queue, p)
	}
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// pop removes and returns the oldest waiting message.
func (f *Fanout) pop() (pending, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) == 0 {
		return pending{}, false
	}
	p := f.queue[0]
	f.queue = f.queue[1:]
	return p, true
}

// Start installs the fan-out as the bus's Replicas hook and runs it: the publishing goroutine, the subscription,
// and the resync worker. Call it once, at boot, before the first Publish that must reach the peers. The returned
// stop function sends what is still queued (bounded by its context: a replica that is draining has just accepted
// writes its peers most need to hear of), then ends the goroutines.
func (f *Fanout) Start(ctx context.Context) (stop func(context.Context)) {
	f.bus.SetReplicas(f)
	f.health.OnChange(func(up bool) {
		if up {
			f.requestResync("health")
		}
	})
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){f.publishLoop, f.subscribeLoop, f.resyncLoop} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					logger.Error("invalidation fan-out goroutine panicked", "panic", r)
				}
			}()
			loop(runCtx)
		}()
	}
	var once sync.Once
	return func(stopCtx context.Context) {
		once.Do(func() {
			f.mu.Lock()
			f.flushCtx = stopCtx
			f.mu.Unlock()
			close(f.stopCh)
			cancel()
			wg.Wait()
		})
	}
}

// Install builds a fan-out on client and starts it; with no client (no Redis configured) it installs nothing, and
// the bus behaves exactly as on one replica. The returned stop function is safe to call either way.
func Install(client *redis.Client, health *redisx.Health, bus *eventbus.Bus, instance string, opts ...Option) (stop func(context.Context)) {
	if client == nil {
		return func(context.Context) {}
	}
	return New(client, health, bus, instance, opts...).Start(context.Background())
}

// publishLoop sends what is queued, in order, and a heartbeat every hbEvery. It numbers the messages: a message it
// does not send (Redis degraded, the publish failed) still takes its number, so the peers see the gap.
func (f *Fanout) publishLoop(ctx context.Context) {
	tick := time.NewTicker(f.hbEvery)
	defer tick.Stop()
	f.heartbeat(context.Background()) // announce this origin at epoch 0, so a peer's first sight of it is a baseline it can trust
	for {
		select {
		case <-f.stopCh:
			f.flush()
			return
		case <-ctx.Done():
			f.flush()
			return
		case <-f.wake:
			f.drain(ctx, context.Background())
		case <-tick.C:
			f.heartbeat(context.Background())
		}
	}
}

// flushContext bounds the final send: the context stop was given (set before the goroutines are cancelled), else a
// short deadline.
func (f *Fanout) flushContext() (context.Context, context.CancelFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flushCtx != nil {
		return f.flushCtx, func() {}
	}
	return context.WithTimeout(context.Background(), 2*time.Second)
}

// flush sends what is queued, within the final deadline.
func (f *Fanout) flush() {
	ctx, cancel := f.flushContext()
	defer cancel()
	f.drain(ctx, ctx)
}

// drain sends the queue, oldest first, until it is empty or until ends. A send is not tied to until: the message
// being sent when a replica stops is sent, and the ones behind it stay queued for the final flush.
func (f *Fanout) drain(until, sendCtx context.Context) {
	for until.Err() == nil {
		p, ok := f.pop()
		if !ok {
			return
		}
		f.epoch++
		f.send(sendCtx, p.kind, p.key, p.scope)
	}
}

func (f *Fanout) heartbeat(ctx context.Context) { f.send(ctx, kindHeartbeat, "", nil) }

// send publishes one message numbered with the current epoch (a heartbeat names the last epoch used).
func (f *Fanout) send(ctx context.Context, kind, key string, scope eventbus.Scope) {
	if f.health.Degraded() {
		if kind != kindHeartbeat {
			dropped.WithLabelValues("redis_down").Inc()
		}
		return
	}
	payload, err := json.Marshal(message{Envelope: redisx.Envelope{V: payloadV1, Kind: kind}, Key: key, Scope: scope, Epoch: f.epoch, Origin: f.origin})
	if err != nil {
		return // cannot happen: strings and numbers
	}
	pctx, cancel := context.WithTimeout(ctx, publishWait)
	defer cancel()
	if err := f.client.Publish(pctx, channel.Key(), payload).Err(); err != nil {
		if kind != kindHeartbeat {
			dropped.WithLabelValues("publish_error").Inc()
		}
		f.warn("invalidation publish failed; the peers keep their caches until the next resync or their TTL", err)
		return
	}
	published.WithLabelValues(kind).Inc()
}

// warn logs at most once every 30 s, with an error class and never the error text (it can carry the Redis address).
func (f *Fanout) warn(msg string, err error) {
	f.warnMu.Lock()
	due := time.Since(f.warnedAt) > 30*time.Second
	if due {
		f.warnedAt = time.Now()
	}
	f.warnMu.Unlock()
	if !due {
		return
	}
	class := "error"
	if errors.Is(err, context.DeadlineExceeded) {
		class = "timeout"
	}
	logger.Warn(msg, "class", class)
}

// subscribeLoop follows the channel for as long as ctx lives, resubscribing with a back-off when the subscription
// breaks. Each time the server confirms a subscription it asks for a resync: whatever was published while this
// replica was not listening is gone, and the caches cannot tell which entries it concerned.
func (f *Fanout) subscribeLoop(ctx context.Context) {
	ps := redisx.NewPubSub(f.client)
	backoff := f.retryMin
	for ctx.Err() == nil {
		err := ps.Subscribe(ctx, channel.Key(), func() {
			backoff = f.retryMin
			// The numbers seen before the outage say nothing about the messages after it: the resync below covers
			// whatever was lost, so each peer's next message is a baseline, not a gap against the old numbering. (This
			// runs on the subscription's goroutine, the only one that touches the tracker.)
			f.track = newTracker()
			f.requestResync("subscribe")
		}, f.onMessage)
		if ctx.Err() != nil {
			return
		}
		f.warn("invalidation subscription lost; resubscribing", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, f.retryMax)
	}
}

// onMessage handles one payload from the channel. Anything it cannot use is counted and ignored; it never panics
// and never deletes.
func (f *Fanout) onMessage(payload []byte) {
	if len(payload) > maxMessage {
		ignored.WithLabelValues("oversized").Inc()
		return
	}
	var head redisx.Envelope
	_ = json.Unmarshal(payload, &head)
	kind := head.Kind
	switch kind {
	case kindInval, kindRecheck, kindHeartbeat, kindResync:
	default:
		kind = kindInval // Decode counts a kind we do not know as unknown, under the channel's main kind
	}
	var m message
	if redisx.Decode(kind, payloadV1, payload, &m) != redisx.ResultOK {
		return
	}
	if m.Kind == "" || m.Origin == "" {
		ignored.WithLabelValues("no_origin").Inc()
		return
	}
	if m.Origin == f.origin {
		return // this replica's own message, echoed back
	}
	if len(m.Scope) > maxScopeParts || oversizedPart(m) {
		ignored.WithLabelValues("bad_scope").Inc()
		return
	}
	if f.track.observe(m.Origin, m.Kind, m.Epoch) {
		gapsTotal.WithLabelValues().Inc()
		f.requestResync("gap")
	}
	if len(f.track.last) > 64 {
		f.track.prune()
	}
	received.WithLabelValues(m.Kind).Inc()
	switch m.Kind {
	case kindInval, kindRecheck:
		if !f.bus.DeliverRemote(m.Key, eventbus.Scope(m.Scope)) {
			ignored.WithLabelValues("unknown_key").Inc()
		}
	case kindResync:
		f.requestResync("peer")
	}
}

func oversizedPart(m message) bool {
	if len(m.Key) > maxPartLen {
		return true
	}
	for k, v := range m.Scope {
		if len(k) > maxPartLen || len(v) > maxPartLen {
			return true
		}
	}
	return false
}

// requestResync asks the resync worker to run; requests that arrive while one is waiting coalesce.
func (f *Fanout) requestResync(reason string) {
	select {
	case f.resyncReq <- reason:
	default:
	}
}

// resyncLoop runs the resyncs, at most one per gap, so a flapping Redis cannot turn into a storm of socket rechecks and
// cold caches. A request that arrives during the gap runs after it, and doubles the gap (up to resyncGapMax): resyncs
// that keep being asked for are a symptom (a Redis that drops its connections every few seconds), and each one costs
// every replicated cache and a database check per open socket. A gap that passes with no request resets it.
func (f *Fanout) resyncLoop(ctx context.Context) {
	gap := f.resyncGap
	var reason string
	for have := false; ; {
		if !have {
			select {
			case <-ctx.Done():
				return
			case reason = <-f.resyncReq:
			}
		}
		f.resync(reason)
		select {
		case <-ctx.Done():
			return
		case <-time.After(gap):
		}
		select {
		case reason = <-f.resyncReq:
			have = true
			gap = min(gap*2, max(f.resyncGapMax, f.resyncGap))
		default:
			have = false
			gap = f.resyncGap
		}
	}
}

// resync drops every replicated cache and has the sockets re-check, through the appliers with an empty scope.
func (f *Fanout) resync(reason string) {
	resyncsTotal.WithLabelValues(reason).Inc()
	for _, key := range f.bus.ReplicatedKeys() {
		f.bus.DeliverRemote(key, nil)
	}
}

// Module installs the fan-out when a Redis client is configured (PAD_REDIS_URL). It is invoked after the services
// that register the appliers (the services and API modules), and its stop hook runs after the HTTP server's, so the
// writes a draining replica accepted are still sent.
var Module = fx.Invoke(register)

type params struct {
	fx.In

	Lifecycle fx.Lifecycle
	Bus       *eventbus.Bus
	Client    *redis.Client
	Health    *redisx.Health
	Config    *config.Config
}

func register(p params) {
	if p.Client == nil {
		return
	}
	var stop func(context.Context)
	p.Lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			stop = Install(p.Client, p.Health, p.Bus, p.Config.Runtime.InstanceID)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if stop != nil {
				stop(ctx)
			}
			return nil
		},
	})
}
