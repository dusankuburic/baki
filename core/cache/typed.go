package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"
)

// FillGuard fences a fill against the invalidations that ran while it read: Load captures the fence's generation
// before the fill reads, FillOK says whether what it read may still be stored. eventbus.Gen[string] has this method
// set, so a service passes its *Gen without core importing it. Typed asks FillOK under its lock, in one step with the
// store, so an invalidator that bumps the fence before it deletes the key never leaves a stale fill behind (deleting
// first leaves a gap). FillOK must not call into the cache.
type FillGuard interface {
	Load(key string) uint64
	FillOK(ctx context.Context, key string, captured uint64) bool
}

// Typed is the default storage of a declared cache: values of one type under the spec's keys, in an LRU bounded by
// summed cost (SizedLRU), with fills deduplicated per key. Its hits, misses, stores, evictions and size count
// toward the spec's statistics.
type Typed[V any] struct {
	spec    *Spec
	lru     *SizedLRU
	flights singleflight.Group
}

// New builds a cache of spec whose entries' summed cost stays within budget: bytes by convention, or an entry
// count when every entry is stored at cost 1. opts are the SizedLRU options (WithMaxEntryCost, WithMaxEntries,
// WithTTL). It panics for a Shared spec other than MechanismInvalidation: Typed is replica-local storage, and only
// the invalidation fan-out shares what such storage holds.
func New[V any](spec *Spec, budget int64, opts ...SizedOption) *Typed[V] {
	if spec == nil {
		panic("cache: New with a nil spec")
	}
	if spec.locality != LocalityReplica && spec.mechanism != MechanismInvalidation {
		panic(fmt.Sprintf("cache %q: declared shared by %q, but New builds in-process storage (only %q shares that)", spec.name, spec.mechanism, MechanismInvalidation))
	}
	lru := NewSizedLRU(budget, opts...)
	lru.onEvict = spec.RecordEviction
	Track(spec, lru)
	return &Typed[V]{spec: spec, lru: lru}
}

// Spec is the spec c was built for.
func (c *Typed[V]) Spec() *Spec { return c.spec }

// usable reports whether k may be used with c. The zero Key (counted when Key refused its parts) and a key of
// another spec (counted here, and a panic in a test binary) skip the cache.
func (c *Typed[V]) usable(k Key) bool {
	if k.spec == c.spec {
		return true
	}
	if k.spec != nil {
		_ = c.spec.keyError(&KeyError{Spec: c.spec.name, Problem: "foreign"})
	}
	return false
}

// Get returns the value under k and refreshes its recency.
func (c *Typed[V]) Get(k Key) (V, bool) {
	var zero V
	if !c.usable(k) {
		return zero, false
	}
	v, ok := c.lru.Get(context.Background(), k.enc)
	if !ok {
		c.spec.stats.misses.Add(1)
		return zero, false
	}
	c.spec.stats.hits.Add(1)
	tv, _ := v.(V)
	return tv, true
}

// Set stores v under k at cost. ttl 0 is the cache's default TTL (WithTTL), negative never expires. A value over
// the per-entry ceiling is not stored, and drops the old value under k.
func (c *Typed[V]) Set(k Key, v V, cost int64, ttl time.Duration) {
	if !c.usable(k) {
		return
	}
	if c.lru.store(k.enc, v, cost, ttl) {
		c.spec.stats.stores.Add(1)
	}
}

// Delete removes the value under k.
func (c *Typed[V]) Delete(k Key) {
	if c.usable(k) {
		c.lru.Delete(context.Background(), k.enc)
	}
}

// Clear drops every entry, counted as deletions: what an invalidation fan-out resync does to a cache that cannot
// tell which of its entries a lost message concerned. A fill in flight is fenced by its owner's guard, as for Delete.
func (c *Typed[V]) Clear() { c.lru.Clear() }

// Len is the number of entries held (expired ones included until touched).
func (c *Typed[V]) Len() int { return c.lru.Len() }

// Cost is the summed cost of the entries held.
func (c *Typed[V]) Cost() int64 { return c.lru.Cost() }

// Budget is the cost bound the cache was built with.
func (c *Typed[V]) Budget() int64 { return c.lru.Budget() }

// MaxEntryCost is the per-entry ceiling: larger values are never stored.
func (c *Typed[V]) MaxEntryCost() int64 { return c.lru.MaxEntryCost() }

// maxFlightAttempts bounds how often a caller joins a flight whose leader left before it finished.
const maxFlightAttempts = 3

// leaderGone fails a flight whose leader's context ended before the fill returned: its result is neither stored
// nor handed to the callers that joined, who fill again under their own context.
type leaderGone struct{ err error }

func (e *leaderGone) Error() string { return "cache: fill abandoned: " + e.err.Error() }

// GetOrFill returns the value under k, or runs fill once for all concurrent callers of k (the flight is keyed by
// the full key, so callers the key separates never share a fill) and stores its result at the returned cost with
// the default TTL, if guard (when not nil) still agrees for fence: guard.Load(fence) is captured before fill runs,
// and FillOK is asked atomically with the store. Invalidators Bump the fence, then Delete the key (FillGuard).
// fill runs under the context of the caller that started the flight; a fill whose context ended before it returned
// is neither stored nor handed out, and a joined caller whose own context is live fills again. A fill error is
// returned and nothing is stored. With an unusable key (the zero Key) fill runs uncached.
func (c *Typed[V]) GetOrFill(ctx context.Context, k Key, fill func(context.Context) (V, int64, error), guard FillGuard, fence string) (V, error) {
	var zero V
	if !c.usable(k) {
		v, _, err := fill(ctx)
		return v, err
	}
	if v, ok := c.Get(k); ok {
		return v, nil
	}
	for range maxFlightAttempts {
		res, err, _ := c.flights.Do(k.enc, func() (any, error) {
			// A flight that finished between our miss and this one has stored the value.
			if v, ok := c.lru.Get(ctx, k.enc); ok {
				return v, nil
			}
			return c.fillAndStore(ctx, k, fill, guard, fence)
		})
		if err == nil {
			v, _ := res.(V)
			return v, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return zero, cerr
		}
		var gone *leaderGone
		if !errors.As(err, &gone) {
			return zero, err
		}
	}
	// Joined leaders that kept leaving: fill under our own context, outside the flight.
	res, err := c.fillAndStore(ctx, k, fill, guard, fence)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return zero, cerr
		}
		return zero, err
	}
	v, _ := res.(V)
	return v, nil
}

func (c *Typed[V]) fillAndStore(ctx context.Context, k Key, fill func(context.Context) (V, int64, error), guard FillGuard, fence string) (any, error) {
	var captured uint64
	if guard != nil {
		captured = guard.Load(fence)
	}
	v, cost, err := fill(ctx)
	if cerr := ctx.Err(); cerr != nil {
		return nil, &leaderGone{err: cerr}
	}
	if err != nil {
		return nil, err
	}
	var allow func() bool
	if guard != nil {
		allow = func() bool { return guard.FillOK(ctx, fence, captured) }
	}
	if c.lru.storeIf(k.enc, v, cost, 0, allow) {
		c.spec.stats.stores.Add(1)
	}
	return v, nil
}
