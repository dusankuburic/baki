// Package eventbus carries domain events (internal/events) from the service that changed something to whoever
// keeps something derived from it, in process. It depends on no domain package: a publisher and its subscribers
// know only the event types. docs/adr/0009-domain-events.md has the rules.
//
// Two tiers, chosen per subscriber:
//
//   - Sync: cache invalidation. The subscriber runs inside Publish, and, when Publish ran inside an open
//     transaction, again once that transaction has committed, or rolled back. A cache filled from the
//     transaction's uncommitted writes is so dropped whichever way it ends (and Gen keeps a concurrent fill from
//     re-adding a stale value). Sync subscribers must be quick, must not block and must not fail the publisher.
//   - Async: side effects (email, notifications, realtime). The subscriber runs only after the commit (never for a
//     rolled-back write), on the bus's bounded background runner, each delivery with its own timeout and panic
//     recovery, on a context without the request's cancellation or row-level-security scope.
//
// Every subscriber also states its locality (replicated.go): Local, its effect stays on this replica, or Replicated,
// the bus tells the other replicas what to invalidate once the write is durable, through the Replicas hook.
//
// Delivery is in process and at most once: a crash between the commit and an async delivery loses it (no
// outbox).
package eventbus

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/reqinfo"
	"pad-analyzer/internal/platform/txhook"
	"pad-core/logger"
)

// Event is a domain event; every type in internal/events is one.
type Event interface {
	// EventName is the event's name in metrics and logs: a code literal, never request data.
	EventName() string
}

// Tier is how a subscriber is delivered to: Sync or Async (see the package doc).
type Tier uint8

const (
	Sync Tier = iota
	Async
)

func (t Tier) String() string {
	if t == Async {
		return "async"
	}
	return "sync"
}

// Pass says which delivery of a Sync subscriber is running.
type Pass uint8

const (
	// Immediate: inside Publish, before the publisher's transaction (if any) has ended.
	Immediate Pass = iota
	// Committed: after the publisher's transaction committed. Fan-out to other replicas belongs here only (the bus
	// does it for Replicated subscribers, with this pass in the context).
	Committed
	// RolledBack: after the publisher's transaction rolled back.
	RolledBack
)

type passKey struct{}

// PassOf returns which delivery a Sync subscriber is in (Immediate outside the bus).
func PassOf(ctx context.Context) Pass {
	p, _ := ctx.Value(passKey{}).(Pass)
	return p
}

// Runner runs async deliveries: *lifecycle.Bounded, which caps them and is drained at shutdown. Go reports whether
// it started fn (false: saturated or draining, and the delivery is dropped).
type Runner interface {
	Go(fn func()) bool
}

type subscriber struct {
	name   string
	tier   Tier
	fn     func(context.Context, Event)
	inline bool

	// event and evType name the event type subscribed to; the rest is the locality choice (replicated.go).
	event       string
	evType      reflect.Type
	locality    Locality
	reason      string
	keys        []string
	invalidates func(Event) []Invalidation
}

// SubscribeOption tunes one subscription.
type SubscribeOption func(*subscriber)

// InlineWhenSaturated makes an async subscriber run inline, in the committing goroutine's post-commit hook, when the
// bus's runner refuses its delivery (saturated, or draining at shutdown), instead of dropping it. For a subscriber
// whose work must not be lost and that only hands it to a runner of its own, so running it inline is quick.
func InlineWhenSaturated() SubscribeOption {
	return func(s *subscriber) { s.inline = true }
}

// Bus delivers published events to the subscribers of their type. The zero value is not usable; a nil *Bus
// publishes nothing (a service built without one in a test).
type Bus struct {
	mu   sync.RWMutex
	subs map[reflect.Type][]*subscriber
	taps []func(Event)

	// replicas and appliers are the cross-replica seam (replicated.go); both are empty on a single replica.
	replicas Replicas
	appliers map[string]func(Scope)

	runner  Runner
	timeout time.Duration
}

// New returns a bus whose async deliveries run on runner, each bounded by timeout. A nil runner runs them inline,
// in the post-commit hook (tests that want deterministic delivery).
func New(runner Runner, timeout time.Duration) *Bus {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Bus{subs: map[reflect.Type][]*subscriber{}, appliers: map[string]func(Scope){}, runner: runner, timeout: timeout}
}

// Subscribe registers fn for events of type E on tier. name labels the subscriber in metrics and logs and must be
// a code literal. Subscribe at boot (an fx.Invoke), before the first Publish. Every subscriber states whether
// other replicas must reproduce its effect, with Local or Replicated (replicated.go); a registration that states
// neither is listed as unclassified and fails internal/di's subscribers golden.
func Subscribe[E Event](b *Bus, tier Tier, name string, fn func(ctx context.Context, e E), opts ...SubscribeOption) {
	t := reflect.TypeFor[E]()
	var zero E
	s := &subscriber{name: name, tier: tier, fn: func(ctx context.Context, e Event) { fn(ctx, e.(E)) }, event: zero.EventName(), evType: t}
	for _, o := range opts {
		o(s)
	}
	b.mu.Lock()
	b.subs[t] = append(b.subs[t], s)
	b.mu.Unlock()
}

// Publish delivers e to its subscribers (see the package doc for when). ctx is the publisher's: inside a
// transaction, Publish ties the later deliveries to its outcome.
func (b *Bus) Publish(ctx context.Context, e Event) {
	if b == nil {
		return
	}
	recordEventPublished(e.EventName())
	txhook.MarkPublished(ctx)

	b.mu.RLock()
	all := b.subs[reflect.TypeOf(e)]
	taps := b.taps
	b.mu.RUnlock()
	for _, tap := range taps {
		tap(e)
	}

	var syncs, asyncs, reps []*subscriber
	for _, s := range all {
		if s.locality == LocalityReplicated {
			reps = append(reps, s)
		}
		if s.tier == Async {
			asyncs = append(asyncs, s)
		} else {
			syncs = append(syncs, s)
		}
	}
	if len(syncs) > 0 {
		b.deliverSync(ctx, Immediate, syncs, e)
		if txhook.Pending(ctx) {
			txhook.Register(ctx, func(hctx context.Context) { b.deliverSync(hctx, Committed, syncs, e) })
			txhook.OnRollback(ctx, func(hctx context.Context) { b.deliverSync(hctx, RolledBack, syncs, e) })
		}
	}
	if len(asyncs) > 0 {
		base := detach(ctx)
		txhook.Register(ctx, func(context.Context) {
			for _, s := range asyncs {
				b.deliverAsync(base, s, e)
			}
		})
	}
	// Last, so this replica's own caches are already dropped when the others are told.
	if len(reps) > 0 {
		b.replicate(ctx, reps, e)
	}
}

func (b *Bus) deliverSync(ctx context.Context, pass Pass, subs []*subscriber, e Event) {
	ctx = context.WithValue(ctx, passKey{}, pass)
	for _, s := range subs {
		run(ctx, s, e, "sync")
	}
}

func (b *Bus) deliverAsync(base context.Context, s *subscriber, e Event) {
	deliver := func() {
		ctx, cancel := context.WithTimeout(base, b.timeout)
		defer cancel()
		run(ctx, s, e, "async")
	}
	if b.runner == nil {
		deliver()
		return
	}
	if !b.runner.Go(deliver) {
		if s.inline {
			deliver()
			return
		}
		recordEventDelivery(e.EventName(), s.name, "async", 0, "dropped")
		logger.Warn("event delivery dropped", "event", e.EventName(), "subscriber", s.name)
	}
}

// run delivers e to s, recording its duration and its failure (a panic, which is recovered and reported, or an
// async delivery that ran past its deadline).
func run(ctx context.Context, s *subscriber, e Event, tier string) {
	start := time.Now()
	var perr error
	defer func() {
		failure := ""
		switch {
		case perr != nil:
			failure = "panic"
		case tier == "async" && errors.Is(ctx.Err(), context.DeadlineExceeded):
			failure = "timeout"
		}
		recordEventDelivery(e.EventName(), s.name, tier, time.Since(start), failure)
	}()
	defer logger.Guard("event subscriber "+s.name, &perr)
	s.fn(ctx, e)
}

// detach returns the context an async delivery runs on: none of the publisher's cancellation, deadline or
// transaction (its row-level-security scope ends with the request), but its actor and request info, so the
// subscriber's logs and audit rows still name who caused the event.
func detach(ctx context.Context) context.Context {
	out := context.Background()
	if a := auth.ActorFromContext(ctx); a != nil {
		out = auth.WithActor(out, a)
	}
	if c := auth.ClaimsFromContext(ctx); c != nil {
		out = auth.WithClaims(out, c)
	}
	if info := reqinfo.From(ctx); info != nil {
		out = reqinfo.With(out, info)
	}
	return out
}
