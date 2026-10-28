package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// SizedCache is a Cache whose entries are charged a cost — bytes, by
// convention — against a budget: what bounds it is the memory it pins, not
// the entry count (a handful of large documents can push the process past
// its memory limit while the count says the cache is nearly empty).
type SizedCache interface {
	Cache
	// SetWithCost stores value under key, charged at cost against the budget.
	// An entry that cannot fit is not stored and the old value under key is
	// dropped, so a stale value never survives an oversize replacement.
	SetWithCost(ctx context.Context, key string, value any, cost int64, ttl time.Duration)
	// Len is the number of entries held.
	Len() int
	// Cost is the summed cost of the entries held.
	Cost() int64
}

// SizedLRU is an in-memory, concurrency-safe LRU bounded by summed cost
// (NewSizedLRU). It implements Cache: plain Set charges what the
// construction-time cost function says (WithCostFunc), or 1 without one — a
// Set-only user gets a count bound of `budget` entries. Entries evict
// least-recently-used first until within budget (and count cap); Get
// refreshes recency. An entry over the per-entry ceiling (WithMaxEntryCost,
// default: the budget) is never stored. TTLs expire on access; a store with
// ttl 0 gets the default TTL (WithTTL), none without one, and a negative ttl
// never expires.
type SizedLRU struct {
	mu         sync.Mutex
	budget     int64
	maxEntry   int64
	maxEntries int
	ttl        time.Duration
	costOf     func(value any) int64
	onEvict    func(EvictReason) // called under mu; Typed counts evictions on its spec
	total      int64
	ll         *list.List // front = most recently used
	items      map[string]*list.Element
}

type sizedEntry struct {
	key        string
	value      any
	cost       int64
	expiration int64
}

// SizedOption configures a SizedLRU at construction.
type SizedOption func(*SizedLRU)

// WithMaxEntryCost caps the cost of a single entry: anything larger is served
// but never cached (see SizedLRU). It is clamped to the budget.
func WithMaxEntryCost(maxCost int64) SizedOption {
	return func(c *SizedLRU) { c.maxEntry = maxCost }
}

// WithMaxEntries bounds the entry count as well as the cost; n <= 0 means
// unbounded (the default).
func WithMaxEntries(n int) SizedOption {
	return func(c *SizedLRU) { c.maxEntries = n }
}

// WithCostFunc sets the cost Set (the plain Cache interface) charges a value.
// SetWithCost callers supply the cost themselves and bypass it.
func WithCostFunc(fn func(value any) int64) SizedOption {
	return func(c *SizedLRU) { c.costOf = fn }
}

// WithTTL sets the expiry of entries stored with ttl 0; d <= 0 means none
// (the default).
func WithTTL(d time.Duration) SizedOption {
	return func(c *SizedLRU) { c.ttl = d }
}

// NewSizedLRU creates a cache whose entries' summed cost stays within budget.
// It is a building block, like NewLRUCache: production code declares a Spec
// and builds its cache with New.
func NewSizedLRU(budget int64, opts ...SizedOption) *SizedLRU {
	if budget < 0 {
		budget = 0
	}
	c := &SizedLRU{
		budget:   budget,
		maxEntry: budget,
		ll:       list.New(),
		items:    make(map[string]*list.Element),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.maxEntry > c.budget || c.maxEntry < 0 {
		c.maxEntry = c.budget
	}
	return c
}

func (c *SizedLRU) Get(ctx context.Context, key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*sizedEntry)
	if e.expiration > 0 && time.Now().UnixNano() > e.expiration {
		c.removeElement(el, EvictExpired)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return e.value, true
}

// Set stores value charged at the construction-time cost function's estimate
// (1 without one).
func (c *SizedLRU) Set(ctx context.Context, key string, value any, ttl time.Duration) {
	cost := int64(1)
	if c.costOf != nil {
		cost = c.costOf(value)
	}
	c.SetWithCost(ctx, key, value, cost, ttl)
}

func (c *SizedLRU) SetWithCost(ctx context.Context, key string, value any, cost int64, ttl time.Duration) {
	c.store(key, value, cost, ttl)
}

// store is SetWithCost reporting whether the value was stored (false: over the
// per-entry ceiling).
func (c *SizedLRU) store(key string, value any, cost int64, ttl time.Duration) bool {
	return c.storeIf(key, value, cost, ttl, nil)
}

// storeIf is store when allow (if not nil) agrees, asked under c.mu so that
// a Delete cannot run between the check and the store; when it refuses, the
// cache is left as it was. allow must not call back into c.
func (c *SizedLRU) storeIf(key string, value any, cost int64, ttl time.Duration, allow func() bool) bool {
	if cost < 0 {
		cost = 0
	}
	if ttl == 0 {
		ttl = c.ttl
	}
	var expiration int64
	if ttl > 0 {
		expiration = time.Now().Add(ttl).UnixNano()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if allow != nil && !allow() {
		return false
	}
	if el, ok := c.items[key]; ok {
		// A replacement never leaves the old value behind, whether or not
		// the new one fits.
		c.removeElement(el, EvictReplaced)
	}
	if cost > c.maxEntry {
		return false
	}
	el := c.ll.PushFront(&sizedEntry{key: key, value: value, cost: cost, expiration: expiration})
	c.items[key] = el
	c.total += cost
	// Evict from the LRU end until within budget and count. The new entry is
	// at the front and fits on its own, so the loop stops before reaching it.
	for c.ll.Len() > 1 && (c.total > c.budget || (c.maxEntries > 0 && c.ll.Len() > c.maxEntries)) {
		c.removeElement(c.ll.Back(), EvictCapacity)
	}
	return true
}

func (c *SizedLRU) Delete(ctx context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el, EvictDeleted)
	}
}

// Clear removes every entry, each counted as deleted.
func (c *SizedLRU) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.ll.Back(); el != nil; el = c.ll.Back() {
		c.removeElement(el, EvictDeleted)
	}
}

// Len is the number of entries held (expired ones included until touched).
func (c *SizedLRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Cost is the summed cost of the entries held.
func (c *SizedLRU) Cost() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// live copies the unexpired entries, most recently used first, without refreshing recency or removing the expired
// ones (that stays Get's job).
func (c *SizedLRU) live() []sizedEntry {
	now := time.Now().UnixNano()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]sizedEntry, 0, c.ll.Len())
	for el := c.ll.Front(); el != nil; el = el.Next() {
		e := el.Value.(*sizedEntry)
		if e.expiration > 0 && now > e.expiration {
			continue
		}
		out = append(out, *e)
	}
	return out
}

// Budget is the cost bound the cache was built with.
func (c *SizedLRU) Budget() int64 {
	return c.budget
}

// MaxEntryCost is the per-entry ceiling: larger entries are never stored.
func (c *SizedLRU) MaxEntryCost() int64 {
	return c.maxEntry
}

// removeElement unlinks el, settles the total and reports why. Caller holds c.mu.
func (c *SizedLRU) removeElement(el *list.Element, why EvictReason) {
	e := el.Value.(*sizedEntry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.total -= e.cost
	e.value = nil
	if c.onEvict != nil {
		c.onEvict(why)
	}
}
