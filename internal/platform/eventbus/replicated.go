package eventbus

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"pad-analyzer/internal/platform/txhook"
	"pad-core/logger"
)

// Locality is the choice every subscriber makes: whether the effect of its delivery must be reproduced on the
// other replicas of the deployment. The bus delivers in process, so a subscriber that drops a cache entry or
// closes a socket affects this replica only; on a second replica the same change leaves the entry or the socket
// until its TTL or its periodic pass. Nothing defaults: a subscriber registered with neither option is
// LocalityUnset, and internal/di's subscribers golden (and archtest) refuse it.
type Locality uint8

const (
	// LocalityUnset: the subscriber chose neither Local nor Replicated.
	LocalityUnset Locality = iota
	// LocalityLocal: the effect stays on this replica, for the stated reason (a side effect that must happen once,
	// or a cache that cannot go stale across replicas).
	LocalityLocal
	// LocalityReplicated: other replicas reproduce the effect, from an invalidation key the bus publishes.
	LocalityReplicated
)

func (l Locality) String() string {
	switch l {
	case LocalityLocal:
		return "local-only" // not "local": that is a provider id (TestArch_ProviderIDLiteralsOutsideSpecs)
	case LocalityReplicated:
		return "replicated"
	}
	return "unset"
}

// Scope narrows an invalidation to part of a cache: the tenant parts of the keys to drop, never free text or a
// secret ("org": an organisation id, "user", "flow", "scope": a credential key scope). An empty Scope means the
// whole cache. It travels between replicas, so a key may be present with an empty value (the deployment's
// credential scope is ""): presence, not emptiness, says a part was named. The vocabulary of each key is the one
// its subscriber's scope function writes and its applier (OnRemote) reads; docs/architecture/safety-nets.md lists them by key.
type Scope map[string]string

// NewScope returns the scope of the given key, value pairs.
func NewScope(pairs ...string) Scope {
	if len(pairs)%2 != 0 {
		panic("eventbus: NewScope takes key, value pairs")
	}
	s := make(Scope, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		s[pairs[i]] = pairs[i+1]
	}
	return s
}

// String renders the scope as sorted "k=v" pairs (for logs and the tests); "" for the whole cache.
func (s Scope) String() string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k + "=" + s[k])
	}
	return b.String()
}

// canonical is String with separators no id can contain, to dedupe invalidations exactly.
func (s Scope) canonical() string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "\x00" + s[k] + "\x01")
	}
	return b.String()
}

// Invalidation is one thing a replicated subscriber tells the other replicas to drop: an invalidation key and
// the part of it that changed.
type Invalidation struct {
	// Key names what to invalidate: a declared cache ("settings.deployment", a pad-core/cache.Declare name) or a
	// recheck ("recheck.ws-access"). Dot-separated lowercase segments, as a cache name.
	Key   string
	Scope Scope
}

// Replicas is the seam to the other replicas of the deployment, implemented by the Redis fan-out (Phase 5b wave 2,
// ADR 0032) and installed with SetReplicas. With none installed (a single replica, tests) nothing is published
// and the bus behaves exactly as without this file.
type Replicas interface {
	// PublishInvalidation tells the other replicas to drop part of the cache or recheck named key. The bus calls it
	// only once the write is durable: after the publisher's transaction committed, or at once where there is none,
	// never for a rolled-back write, and with PassOf(ctx) == Committed. It must return quickly (enqueue, do not
	// do the network call here), must not fail the publisher, and must not keep ctx, which is the commit hook's
	// and only valid until the call returns. Fan-out is best effort: the caches' TTLs and the receiver's resync
	// bound what a lost message costs.
	PublishInvalidation(ctx context.Context, key string, scope Scope)
}

// SetReplicas installs the cross-replica hook (nil removes it). Call it at boot, before the first Publish.
func (b *Bus) SetReplicas(r Replicas) {
	b.mu.Lock()
	b.replicas = r
	b.mu.Unlock()
}

// OnRemote registers the function that applies an invalidation received from another replica for key: the cache
// owner's own invalidate (a generation bump, so a fill in flight is refused, and the delete). apply gets the Scope
// the origin's scope function wrote, and must drop the whole cache for an empty one (the receiver calls it that way
// to resync after a gap). It must be idempotent, quick, and must not publish again: DeliverRemote does not
// republish, so an invalidation cannot loop between replicas. One applier per key; registering a key twice panics.
func (b *Bus) OnRemote(key string, apply func(Scope)) {
	if !validKey(key) {
		panic(fmt.Sprintf("eventbus: OnRemote key %q must be dot-separated lowercase segments", key))
	}
	if apply == nil {
		panic(fmt.Sprintf("eventbus: OnRemote %q needs an applier", key))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, dup := b.appliers[key]; dup {
		panic(fmt.Sprintf("eventbus: OnRemote %q registered twice", key))
	}
	b.appliers[key] = apply
}

// DeliverRemote is the entry point for an invalidation received from another replica: it runs the applier of key
// with scope. It reports whether an applier exists; a key nobody here registered (a newer release's cache, a
// cache this deployment did not build) is ignored, which the receiver counts. It never publishes. A panicking
// applier is recovered and logged. The caller drops messages of its own origin.
func (b *Bus) DeliverRemote(key string, scope Scope) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	apply := b.appliers[key]
	b.mu.RUnlock()
	if apply == nil {
		return false
	}
	var perr error
	func() {
		defer logger.Guard("remote invalidation "+key, &perr)
		apply(scope)
	}()
	return true
}

// ReplicatedKeys lists the invalidation keys the replicated subscribers can publish, sorted: what a receiver
// resyncs (DeliverRemote with an empty scope) after a gap in the stream.
func (b *Bus) ReplicatedKeys() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, subs := range b.subs {
		for _, s := range subs {
			for _, k := range s.keys {
				seen[k] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MissingAppliers lists the replicated keys without an OnRemote applier: a message for one reaches DeliverRemote
// and is ignored, so its replicas keep the stale entry until the TTL.
func (b *Bus) MissingAppliers() []string {
	keys := b.ReplicatedKeys()
	b.mu.RLock()
	defer b.mu.RUnlock()
	return slices.DeleteFunc(keys, func(k string) bool { _, ok := b.appliers[k]; return ok })
}

// Local marks a subscriber whose effect must stay on the replica that delivers it, and says why in a sentence
// (it is in the golden, so write what a reviewer must know): it has a side effect that has to happen once (an
// email, a database write), or it keeps something no other replica can see stale.
func Local(reason string) SubscribeOption {
	return func(s *subscriber) {
		s.claim(LocalityLocal)
		if strings.TrimSpace(reason) == "" {
			panic(fmt.Sprintf("eventbus: subscriber %q: Local needs a reason", s.name))
		}
		s.reason = reason
	}
}

// Replicated marks a subscriber whose effect the other replicas must reproduce, by the invalidation keys it can
// publish (keys: every key invalidations may return, declared so the golden lists them). invalidations maps the
// event to what changed: nil or empty for an event that changes nothing cached. After the publisher's write is
// durable, the bus hands each result to the installed Replicas, once per distinct (key, scope) of the event; the
// receiving replica runs the key's applier (OnRemote), not this subscriber. Replicated suits a subscriber that
// only drops what it holds derived from the event, idempotently, from fields that are not secret; a subscriber
// with any other side effect is Local. It works on either tier.
func Replicated[E Event](invalidations func(E) []Invalidation, keys ...string) SubscribeOption {
	return func(s *subscriber) {
		s.claim(LocalityReplicated)
		if invalidations == nil {
			panic(fmt.Sprintf("eventbus: subscriber %q: Replicated needs a scope function", s.name))
		}
		if len(keys) == 0 {
			panic(fmt.Sprintf("eventbus: subscriber %q: Replicated needs at least one key", s.name))
		}
		if s.evType != reflect.TypeFor[E]() {
			panic(fmt.Sprintf("eventbus: subscriber %q: Replicated's scope function takes %v, the subscription is for %v",
				s.name, reflect.TypeFor[E](), s.evType))
		}
		for _, k := range keys {
			if !validKey(k) {
				panic(fmt.Sprintf("eventbus: subscriber %q: invalidation key %q must be dot-separated lowercase segments", s.name, k))
			}
		}
		s.keys = slices.Compact(slices.Sorted(slices.Values(keys)))
		s.invalidates = func(e Event) []Invalidation { return invalidations(e.(E)) }
	}
}

func (s *subscriber) claim(l Locality) {
	if s.locality != LocalityUnset {
		panic(fmt.Sprintf("eventbus: subscriber %q chose its locality twice (%s, then %s)", s.name, s.locality, l))
	}
	s.locality = l
}

// keyShape: the shape of a declared cache name, which also names the recheck keys.
var keyShape = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)+$`)

func validKey(k string) bool { return len(k) <= 64 && keyShape.MatchString(k) }

// replicate hands the invalidations of e to the Replicas hook once e's write is durable. Called by Publish after
// the local deliveries are registered, so within a transaction it runs after the Committed pass of the sync
// subscribers (the local caches are already dropped).
func (b *Bus) replicate(ctx context.Context, reps []*subscriber, e Event) {
	b.mu.RLock()
	hook := b.replicas
	b.mu.RUnlock()
	if hook == nil {
		return
	}
	txhook.Register(ctx, func(hctx context.Context) {
		hctx = context.WithValue(hctx, passKey{}, Committed)
		seen := map[string]struct{}{}
		for _, s := range reps {
			for _, inv := range invalidationsOf(s, e) {
				if !slices.Contains(s.keys, inv.Key) {
					logger.Warn("event subscriber returned an undeclared invalidation key", "subscriber", s.name, "key", inv.Key)
					continue
				}
				id := inv.Key + "\x02" + inv.Scope.canonical()
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				publishGuarded(hctx, hook, s.name, inv)
			}
		}
	})
}

// Invalidate tells the other replicas to drop part of the cache or recheck named key for a change that is not a
// domain event: something a service does as a side effect (an analysis recorded, a credential disabled), where an
// event would only exist to carry the key. The caller has dropped its own replica's entry; like a replicated
// subscriber's, the message goes out once the write is durable (after the commit of ctx's transaction, at once
// where there is none, never for a rollback). key must be one a replicated subscriber declares, so the subscribers
// golden stays the complete list of what crosses replicas; any other key is refused with a WARN. A nil bus, or one
// with no Replicas installed, does nothing.
func (b *Bus) Invalidate(ctx context.Context, key string, scope Scope) {
	if b == nil {
		return
	}
	b.mu.RLock()
	hook := b.replicas
	b.mu.RUnlock()
	if hook == nil {
		return
	}
	if !slices.Contains(b.ReplicatedKeys(), key) {
		logger.Warn("invalidation for a key no replicated subscriber declares", "key", key)
		return
	}
	txhook.Register(ctx, func(hctx context.Context) {
		publishGuarded(context.WithValue(hctx, passKey{}, Committed), hook, "Invalidate", Invalidation{Key: key, Scope: scope})
	})
}

// invalidationsOf runs s's scope function, recovering a panic: it must not fail the publisher.
func invalidationsOf(s *subscriber, e Event) (out []Invalidation) {
	var perr error
	defer logger.Guard("event subscriber "+s.name+" scope", &perr)
	return s.invalidates(e)
}

func publishGuarded(ctx context.Context, hook Replicas, name string, inv Invalidation) {
	var perr error
	defer logger.Guard("replicas hook for "+name, &perr)
	hook.PublishInvalidation(ctx, inv.Key, inv.Scope)
}

// SubscriberInfo describes one registered subscriber, for the subscribers golden and the tests.
type SubscriberInfo struct {
	Event    string // the event's EventName
	Tier     Tier
	Name     string
	Locality Locality
	Reason   string   // why a Local subscriber stays local
	Keys     []string // the invalidation keys of a Replicated one, sorted
}

// Passes says in which deliveries the subscriber runs: a Sync one in Publish and again after the commit or the
// rollback of its transaction, an Async one after the commit only.
func (i SubscriberInfo) Passes() string {
	if i.Tier == Async {
		return "committed"
	}
	return "immediate+committed+rolled-back"
}

// String is the subscriber's line in the golden: event | mode | passes | name | locality.
func (i SubscriberInfo) String() string {
	var loc string
	switch i.Locality {
	case LocalityLocal:
		loc = "local: " + i.Reason
	case LocalityReplicated:
		loc = "replicated: " + strings.Join(i.Keys, ", ")
	default:
		loc = "UNCLASSIFIED"
	}
	return fmt.Sprintf("%s | %s | %s | %s | %s", i.Event, i.Tier, i.Passes(), i.Name, loc)
}

// Subscribers lists every registration, by event name and, within an event, in registration order (the order a
// publish delivers the sync tier in).
func (b *Bus) Subscribers() []SubscriberInfo {
	b.mu.RLock()
	var out []SubscriberInfo
	for _, subs := range b.subs {
		for _, s := range subs {
			out = append(out, SubscriberInfo{Event: s.event, Tier: s.tier, Name: s.name, Locality: s.locality, Reason: s.reason, Keys: slices.Clone(s.keys)})
		}
	}
	b.mu.RUnlock()
	// A stable sort over the per-type slices, which map order shuffles only between types, never within one.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Event < out[j].Event })
	return out
}

// Unclassified lists the registrations that chose neither Local nor Replicated.
func (b *Bus) Unclassified() []SubscriberInfo {
	return slices.DeleteFunc(b.Subscribers(), func(i SubscriberInfo) bool { return i.Locality != LocalityUnset })
}
