package cache

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

var (
	_ Cache      = (*SizedLRU)(nil)
	_ SizedCache = (*SizedLRU)(nil)
)

func keysOf(c *SizedLRU) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var keys []string
	for el := c.ll.Front(); el != nil; el = el.Next() {
		keys = append(keys, el.Value.(*sizedEntry).key)
	}
	return keys
}

// TestSizedLRU_EvictsByCostInLRUOrder: the budget is a cost bound, not an
// entry bound — four 30-byte entries fit a 100-byte budget only three at a
// time, and the one that goes is the least recently used, Get counting as a
// use.
func TestSizedLRU_EvictsByCostInLRUOrder(t *testing.T) {
	ctx := context.Background()
	c := NewSizedLRU(100)
	c.SetWithCost(ctx, "a", 1, 30, 0)
	c.SetWithCost(ctx, "b", 2, 30, 0)
	c.SetWithCost(ctx, "c", 3, 30, 0)
	if c.Len() != 3 || c.Cost() != 90 {
		t.Fatalf("after three inserts: len=%d cost=%d, want 3/90", c.Len(), c.Cost())
	}
	// Touch "a" so "b" is the LRU entry.
	if _, ok := c.Get(ctx, "a"); !ok {
		t.Fatal("a missing")
	}
	c.SetWithCost(ctx, "d", 4, 30, 0)
	if _, ok := c.Get(ctx, "b"); ok {
		t.Error("b (least recently used) should have been evicted to fit d")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(ctx, k); !ok {
			t.Errorf("%s should still be cached", k)
		}
	}
	if c.Len() != 3 || c.Cost() != 90 {
		t.Errorf("after eviction: len=%d cost=%d, want 3/90", c.Len(), c.Cost())
	}
	// A single large entry evicts as many as it needs.
	c.SetWithCost(ctx, "e", 5, 70, 0)
	if c.Cost() > 100 {
		t.Errorf("cost %d exceeds the 100-byte budget", c.Cost())
	}
	if _, ok := c.Get(ctx, "e"); !ok {
		t.Error("e (fits the budget on its own) should be cached")
	}
	if got := c.Len(); got != 2 {
		t.Errorf("len=%d after a 70-byte insert into a 100-byte budget, want 2 (e + one 30-byte survivor)", got)
	}
}

// TestSizedLRU_OversizeEntryIsNeverStored: an entry above the per-entry
// ceiling is skipped, and a skipped replacement drops the stale value under
// its key rather than leaving it to be served.
func TestSizedLRU_OversizeEntryIsNeverStored(t *testing.T) {
	ctx := context.Background()
	c := NewSizedLRU(100, WithMaxEntryCost(40))
	if c.MaxEntryCost() != 40 {
		t.Fatalf("MaxEntryCost=%d, want 40", c.MaxEntryCost())
	}
	c.SetWithCost(ctx, "small", 1, 10, 0)
	c.SetWithCost(ctx, "big", 2, 41, 0)
	if _, ok := c.Get(ctx, "big"); ok {
		t.Error("an entry above the per-entry ceiling must not be cached")
	}
	if _, ok := c.Get(ctx, "small"); !ok {
		t.Error("a skipped oversize insert must not evict others")
	}
	if c.Len() != 1 || c.Cost() != 10 {
		t.Errorf("len=%d cost=%d, want 1/10", c.Len(), c.Cost())
	}
	// Stale value under the key of an oversize replacement is dropped.
	c.SetWithCost(ctx, "small", 1, 41, 0)
	if _, ok := c.Get(ctx, "small"); ok {
		t.Error("the old value survived an oversize replacement")
	}
	if c.Len() != 0 || c.Cost() != 0 {
		t.Errorf("len=%d cost=%d after the stale drop, want 0/0", c.Len(), c.Cost())
	}
	// The ceiling never exceeds the budget.
	wide := NewSizedLRU(50, WithMaxEntryCost(500))
	if wide.MaxEntryCost() != 50 {
		t.Errorf("MaxEntryCost=%d, want clamped to the 50-byte budget", wide.MaxEntryCost())
	}
	wide.SetWithCost(ctx, "x", 1, 51, 0)
	if wide.Len() != 0 {
		t.Error("an entry larger than the whole budget was stored")
	}
}

// TestSizedLRU_ReplaceAndDeleteSettleCost: the total tracks replacements and
// deletes exactly, so the budget stays exact over a long-lived cache.
func TestSizedLRU_ReplaceAndDeleteSettleCost(t *testing.T) {
	ctx := context.Background()
	c := NewSizedLRU(1000)
	c.SetWithCost(ctx, "k", "v1", 300, 0)
	c.SetWithCost(ctx, "k", "v2", 200, 0)
	if c.Len() != 1 || c.Cost() != 200 {
		t.Fatalf("after replace: len=%d cost=%d, want 1/200", c.Len(), c.Cost())
	}
	if v, _ := c.Get(ctx, "k"); v != "v2" {
		t.Errorf("Get after replace = %v, want v2", v)
	}
	c.Delete(ctx, "k")
	c.Delete(ctx, "absent")
	if c.Len() != 0 || c.Cost() != 0 {
		t.Errorf("after delete: len=%d cost=%d, want 0/0", c.Len(), c.Cost())
	}
	// Negative costs are clamped rather than crediting the budget.
	c.SetWithCost(ctx, "neg", 1, -50, 0)
	if c.Cost() != 0 {
		t.Errorf("negative cost credited the budget: cost=%d", c.Cost())
	}
}

// TestSizedLRU_TTLExpiryReleasesCost: an expired entry is dropped on access
// and its cost released.
func TestSizedLRU_TTLExpiryReleasesCost(t *testing.T) {
	ctx := context.Background()
	c := NewSizedLRU(1000)
	c.SetWithCost(ctx, "ttl", 1, 100, time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	if _, ok := c.Get(ctx, "ttl"); ok {
		t.Error("expired entry served")
	}
	if c.Len() != 0 || c.Cost() != 0 {
		t.Errorf("after expiry: len=%d cost=%d, want 0/0", c.Len(), c.Cost())
	}
	c.SetWithCost(ctx, "live", 1, 100, time.Hour)
	if _, ok := c.Get(ctx, "live"); !ok {
		t.Error("unexpired entry missing")
	}
}

// TestSizedLRU_MaxEntriesCapsCount: WithMaxEntries bounds the count as well,
// evicting LRU-first.
func TestSizedLRU_MaxEntriesCapsCount(t *testing.T) {
	ctx := context.Background()
	c := NewSizedLRU(1<<20, WithMaxEntries(2))
	c.SetWithCost(ctx, "a", 1, 1, 0)
	c.SetWithCost(ctx, "b", 2, 1, 0)
	c.SetWithCost(ctx, "c", 3, 1, 0)
	if got := keysOf(c); len(got) != 2 || got[0] != "c" || got[1] != "b" {
		t.Errorf("entries (MRU first) = %v, want [c b]", got)
	}
}

// TestSizedLRU_SetChargesTheCostFunc: through the plain Cache interface, Set
// charges what the cost function says (1 without one, making the budget an
// entry count).
func TestSizedLRU_SetChargesTheCostFunc(t *testing.T) {
	ctx := context.Background()
	weighed := NewSizedLRU(100, WithCostFunc(func(v any) int64 { return int64(len(v.(string))) }))
	var asCache Cache = weighed
	asCache.Set(ctx, "k", "twelve bytes", 0)
	if weighed.Cost() != 12 {
		t.Errorf("Set charged %d, want the cost function's 12", weighed.Cost())
	}
	asCache.Set(ctx, "k2", string(make([]byte, 101)), 0)
	if _, ok := asCache.Get(ctx, "k2"); ok {
		t.Error("Set stored a value the cost function sized above the budget")
	}

	counted := NewSizedLRU(2)
	counted.Set(ctx, "a", 1, 0)
	counted.Set(ctx, "b", 2, 0)
	counted.Set(ctx, "c", 3, 0)
	if counted.Len() != 2 || counted.Cost() != 2 {
		t.Errorf("without a cost func: len=%d cost=%d, want a 2-entry count bound", counted.Len(), counted.Cost())
	}
	if _, ok := counted.Get(ctx, "a"); ok {
		t.Error("count-bound cache kept the LRU entry past its budget")
	}
}

// TestSizedLRU_ConcurrentUse runs mixed operations from many goroutines (the
// race detector is the assertion) and checks the accounting stays exact.
func TestSizedLRU_ConcurrentUse(t *testing.T) {
	ctx := context.Background()
	c := NewSizedLRU(5000, WithMaxEntries(50))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				key := fmt.Sprintf("k%d", (g*31+i)%80)
				switch i % 4 {
				case 0, 1:
					c.SetWithCost(ctx, key, i, int64(50+i%100), 0)
				case 2:
					c.Get(ctx, key)
				case 3:
					c.Delete(ctx, key)
				}
			}
		}(g)
	}
	wg.Wait()
	c.mu.Lock()
	var sum int64
	n := 0
	for el := c.ll.Front(); el != nil; el = el.Next() {
		sum += el.Value.(*sizedEntry).cost
		n++
	}
	c.mu.Unlock()
	if sum != c.Cost() || n != c.Len() || n != len(c.items) {
		t.Errorf("accounting drifted: list cost=%d total=%d, list len=%d Len=%d items=%d", sum, c.Cost(), n, c.Len(), len(c.items))
	}
	if c.Cost() > 5000 || c.Len() > 50 {
		t.Errorf("bounds violated: cost=%d len=%d", c.Cost(), c.Len())
	}
}

// TestSizedLRU_EvictionHookReportsWhy: every entry that leaves is reported once, with its reason; a skipped oversize
// insert removes nothing and reports nothing.
func TestSizedLRU_EvictionHookReportsWhy(t *testing.T) {
	ctx := context.Background()
	c := NewSizedLRU(100, WithMaxEntryCost(60))
	var got []EvictReason
	c.onEvict = func(r EvictReason) { got = append(got, r) }
	c.SetWithCost(ctx, "a", 1, 50, 0)
	c.SetWithCost(ctx, "b", 2, 50, 0)
	c.SetWithCost(ctx, "c", 3, 50, 0) // a: capacity
	c.SetWithCost(ctx, "b", 4, 10, 0) // b: replaced
	c.SetWithCost(ctx, "c", 5, 61, 0) // c: replaced by an oversize value, which is not stored
	c.SetWithCost(ctx, "d", 6, 61, 0) // nothing to drop
	c.Delete(ctx, "b")                // b: deleted
	c.Delete(ctx, "b")                // already gone
	c.SetWithCost(ctx, "e", 7, 1, time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	c.Get(ctx, "e") // e: expired
	want := []EvictReason{EvictCapacity, EvictReplaced, EvictReplaced, EvictDeleted, EvictExpired}
	if !slices.Equal(got, want) {
		t.Errorf("reported %v, want %v", got, want)
	}
	if c.Len() != 0 || c.Cost() != 0 {
		t.Errorf("len=%d cost=%d, want 0/0", c.Len(), c.Cost())
	}
}

// TestSizedLRU_DefaultTTL: WithTTL applies to stores with ttl 0; an explicit ttl wins and a negative one never
// expires.
func TestSizedLRU_DefaultTTL(t *testing.T) {
	ctx := context.Background()
	c := NewSizedLRU(10, WithTTL(time.Nanosecond))
	c.Set(ctx, "default", 1, 0)
	c.SetWithCost(ctx, "explicit", 1, 1, time.Hour)
	c.SetWithCost(ctx, "never", 1, 1, -1)
	time.Sleep(2 * time.Millisecond)
	if _, ok := c.Get(ctx, "default"); ok {
		t.Error("the default TTL did not apply")
	}
	for _, k := range []string{"explicit", "never"} {
		if _, ok := c.Get(ctx, k); !ok {
			t.Errorf("%s expired", k)
		}
	}
	plain := NewSizedLRU(10)
	plain.Set(ctx, "k", 1, 0)
	time.Sleep(2 * time.Millisecond)
	if _, ok := plain.Get(ctx, "k"); !ok {
		t.Error("without WithTTL, ttl 0 must never expire")
	}
}

// TestSizedLRU_StoreIfRefusedLeavesCache: a refused conditional store changes nothing, the old value included.
func TestSizedLRU_StoreIfRefusedLeavesCache(t *testing.T) {
	ctx := context.Background()
	c := NewSizedLRU(10)
	c.SetWithCost(ctx, "k", "old", 3, 0)
	if c.storeIf("k", "new", 4, 0, func() bool { return false }) {
		t.Error("refused store reported stored")
	}
	if v, ok := c.Get(ctx, "k"); !ok || v != "old" || c.Cost() != 3 {
		t.Errorf("after a refused store: %v, %v, cost %d; want old, true, 3", v, ok, c.Cost())
	}
	if !c.storeIf("k", "new", 4, 0, func() bool { return true }) {
		t.Error("allowed store refused")
	}
	if v, _ := c.Get(ctx, "k"); v != "new" || c.Cost() != 4 {
		t.Errorf("after an allowed store: %v, cost %d", v, c.Cost())
	}
}
