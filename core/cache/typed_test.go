package cache

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func typedSpec(t *testing.T) *Spec {
	return testSpec(t, "x."+strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-")),
		Keys(KindTenant, KindContent), ReplicaLocal("test"))
}

func mustKey(t *testing.T, s *Spec, parts ...Part) Key {
	t.Helper()
	k, err := s.Key(parts...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestTyped_GetSetDeleteCountStats: the spec counts every hit, miss, store and eviction, and reports the entries and
// cost its caches hold.
func TestTyped_GetSetDeleteCountStats(t *testing.T) {
	s := typedSpec(t)
	c := New[string](s, 100)
	a, b := mustKey(t, s, User("u"), Content("a")), mustKey(t, s, User("u"), Content("b"))
	if _, ok := c.Get(a); ok {
		t.Fatal("hit on an empty cache")
	}
	c.Set(a, "va", 30, 0)
	c.Set(b, "vb", 20, 0)
	if v, ok := c.Get(a); !ok || v != "va" {
		t.Fatalf("Get(a) = %q, %v", v, ok)
	}
	c.Set(a, "va2", 40, 0)  // replaced
	c.Set(b, "big", 101, 0) // oversize: not stored, drops the old value (replaced)
	c.Delete(a)             // deleted
	st := s.Info().Stats
	if st.Hits != 1 || st.Misses != 1 || st.Stores != 3 || st.Rejected != 0 {
		t.Errorf("hits/misses/stores/rejected = %d/%d/%d/%d, want 1/1/3/0", st.Hits, st.Misses, st.Stores, st.Rejected)
	}
	if st.Evictions[EvictReplaced] != 2 || st.Evictions[EvictDeleted] != 1 || st.Evictions[EvictCapacity] != 0 {
		t.Errorf("evictions = %v, want 2 replaced, 1 deleted", st.Evictions)
	}
	if st.Entries != 0 || st.Cost != 0 {
		t.Errorf("entries/cost = %d/%d, want 0/0", st.Entries, st.Cost)
	}
	c.Set(a, "va", 60, 0)
	c.Set(b, "vb", 60, 0) // evicts a (capacity)
	c2 := New[string](s, 100)
	c2.Set(a, "other instance", 7, 0)
	st = s.Info().Stats
	if st.Evictions[EvictCapacity] != 1 || st.Entries != 2 || st.Cost != 67 {
		t.Errorf("capacity evictions %d, entries %d, cost %d; want 1, 2 (summed over both caches), 67",
			st.Evictions[EvictCapacity], st.Entries, st.Cost)
	}
	if c.Len() != 1 || c.Cost() != 60 || c.Budget() != 100 || c.MaxEntryCost() != 100 || c.Spec() != s {
		t.Errorf("accessors: len %d cost %d budget %d max %d", c.Len(), c.Cost(), c.Budget(), c.MaxEntryCost())
	}
}

// TestTyped_TTL: WithTTL is the default expiry, an explicit ttl overrides it, a negative one never expires, and an
// expired entry counts as an expiry and a miss.
func TestTyped_TTL(t *testing.T) {
	s := typedSpec(t)
	c := New[int](s, 100, WithTTL(time.Nanosecond))
	short, long, never := mustKey(t, s, Local(), Content("s")), mustKey(t, s, Local(), Content("l")), mustKey(t, s, Local(), Content("n"))
	c.Set(short, 1, 1, 0)
	c.Set(long, 2, 1, time.Hour)
	c.Set(never, 3, 1, -1)
	time.Sleep(2 * time.Millisecond)
	if _, ok := c.Get(short); ok {
		t.Error("the default TTL did not expire the entry")
	}
	if _, ok := c.Get(long); !ok {
		t.Error("an explicit TTL was overridden by the default")
	}
	if _, ok := c.Get(never); !ok {
		t.Error("a negative TTL expired")
	}
	if st := s.Info().Stats; st.Evictions[EvictExpired] != 1 || st.Misses != 1 {
		t.Errorf("expired %d, misses %d; want 1, 1", st.Evictions[EvictExpired], st.Misses)
	}
}

// TestTyped_GetOrFillOncePerKey: concurrent callers of one key share one fill; another key (another tenant here)
// gets its own fill, never the first one's result.
func TestTyped_GetOrFillOncePerKey(t *testing.T) {
	s := typedSpec(t)
	c := New[string](s, 1<<20)
	ctx := context.Background()
	var fills atomic.Int32
	release := make(chan struct{})
	fill := func(tenant string) func(context.Context) (string, int64, error) {
		return func(context.Context) (string, int64, error) {
			fills.Add(1)
			<-release
			return "report of " + tenant, 10, nil
		}
	}
	kA, kB := mustKey(t, s, Org("a"), Content("same")), mustKey(t, s, Org("b"), Content("same"))
	var wg sync.WaitGroup
	results := make([]string, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k, tenant := kA, "a"
			if i%2 == 1 {
				k, tenant = kB, "b"
			}
			v, err := c.GetOrFill(ctx, k, fill(tenant), nil, "")
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		}(i)
	}
	waitFor(t, func() bool { return fills.Load() == 2 })
	close(release)
	wg.Wait()
	for i, v := range results {
		want := "report of a"
		if i%2 == 1 {
			want = "report of b"
		}
		if v != want {
			t.Errorf("caller %d got %q, want %q", i, v, want)
		}
	}
	if n := fills.Load(); n != 2 {
		t.Errorf("fills = %d, want 2 (one per key)", n)
	}
	if v, err := c.GetOrFill(ctx, kA, fill("again"), nil, ""); err != nil || v != "report of a" {
		t.Errorf("after the flight: %q, %v; want the stored value", v, err)
	}
	if st := s.Info().Stats; st.Stores != 2 || st.Hits+st.Misses != 21 {
		t.Errorf("stores %d, lookups %d; want 2 stores and one lookup per call (21)", st.Stores, st.Hits+st.Misses)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

// fakeGuard has eventbus.Gen[string]'s method set: a bump between Load and FillOK refuses the fill.
type fakeGuard struct {
	mu  sync.Mutex
	gen map[string]uint64
}

func (g *fakeGuard) Load(k string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gen[k]
}

func (g *fakeGuard) Bump(k string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gen == nil {
		g.gen = map[string]uint64{}
	}
	g.gen[k]++
}

func (g *fakeGuard) FillOK(_ context.Context, k string, captured uint64) bool {
	return g.Load(k) == captured
}

// TestTyped_GetOrFillFenced: a fill during which its fence was invalidated is returned but not stored; with the
// fence quiet it is stored.
func TestTyped_GetOrFillFenced(t *testing.T) {
	s := typedSpec(t)
	c := New[int](s, 100)
	g := &fakeGuard{}
	k := mustKey(t, s, Org("o"), Content("c"))
	v, err := c.GetOrFill(context.Background(), k, func(context.Context) (int, int64, error) {
		g.Bump("o") // an invalidation commits while the fill reads
		return 1, 1, nil
	}, g, "o")
	if err != nil || v != 1 {
		t.Fatalf("fenced fill = %d, %v", v, err)
	}
	if _, ok := c.Get(k); ok {
		t.Error("a fill fenced by an invalidation was stored")
	}
	if v, _ := c.GetOrFill(context.Background(), k, func(context.Context) (int, int64, error) { return 2, 1, nil }, g, "o"); v != 2 {
		t.Errorf("second fill = %d", v)
	}
	if v, ok := c.Get(k); !ok || v != 2 {
		t.Errorf("unfenced fill not stored: %d, %v", v, ok)
	}
}

// invalidateAfterCheck is a guard whose check passes, after which an invalidator bumps the fence and deletes the key
// (the documented order). It returns once the delete finished or is blocked, so the store follows the invalidation.
type invalidateAfterCheck struct {
	*fakeGuard
	c    *Typed[string]
	k    Key
	once sync.Once
	wg   sync.WaitGroup
}

func (g *invalidateAfterCheck) FillOK(ctx context.Context, fence string, captured uint64) bool {
	ok := g.fakeGuard.FillOK(ctx, fence, captured)
	g.once.Do(func() {
		deleted := make(chan struct{})
		g.wg.Go(func() {
			g.Bump(fence)
			g.c.Delete(g.k)
			close(deleted)
		})
		select {
		case <-deleted:
		case <-time.After(50 * time.Millisecond): // blocked behind the store
		}
	})
	return ok
}

// TestTyped_GetOrFillInvalidatedAfterCheck: an invalidation (Bump, then Delete) that lands just after the guard
// agreed never leaves the value read before it cached: the check and the store are one step under the cache's lock.
func TestTyped_GetOrFillInvalidatedAfterCheck(t *testing.T) {
	s := typedSpec(t)
	c := New[string](s, 100)
	k := mustKey(t, s, Org("o"), Content("c"))
	g := &invalidateAfterCheck{fakeGuard: &fakeGuard{}, c: c, k: k}
	v, err := c.GetOrFill(context.Background(), k, func(context.Context) (string, int64, error) { return "stale", 1, nil }, g, "o")
	if err != nil || v != "stale" {
		t.Fatalf("fill = %q, %v", v, err)
	}
	g.wg.Wait()
	if v, ok := c.Get(k); ok {
		t.Errorf("value read before the invalidation is cached after it: %q", v)
	}
}

// TestTyped_GetOrFillErrorNotStored: a fill error goes to the caller after one fill, and nothing is stored.
func TestTyped_GetOrFillErrorNotStored(t *testing.T) {
	s := typedSpec(t)
	c := New[*int](s, 100)
	k := mustKey(t, s, User("u"), Content("c"))
	boom := errors.New("boom")
	fills := 0
	if _, err := c.GetOrFill(context.Background(), k, func(context.Context) (*int, int64, error) { fills++; return nil, 0, boom }, nil, ""); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	if fills != 1 {
		t.Errorf("fills = %d, want 1: a fill error is not retried", fills)
	}
	if c.Len() != 0 {
		t.Error("a failed fill was stored")
	}
	// A nil pointer is a value like any other.
	if v, err := c.GetOrFill(context.Background(), k, func(context.Context) (*int, int64, error) { return nil, 1, nil }, nil, ""); err != nil || v != nil {
		t.Errorf("nil value fill = %v, %v", v, err)
	}
	if v, ok := c.Get(k); !ok || v != nil {
		t.Errorf("nil value not served: %v, %v", v, ok)
	}
}

// inFlight counts the goroutines inside a singleflight Do (the leader, and callers that joined or are joining).
func inFlight() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "singleflight.(*Group).Do(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// TestTyped_GetOrFillErrorSharedOnce: callers that joined a failing flight all get its error, from a single fill.
func TestTyped_GetOrFillErrorSharedOnce(t *testing.T) {
	s := typedSpec(t)
	c := New[int](s, 100)
	k := mustKey(t, s, User("u"), Content("c"))
	boom := errors.New("boom")
	release := make(chan struct{})
	var fills atomic.Int32
	fill := func(context.Context) (int, int64, error) {
		fills.Add(1)
		<-release
		return 0, 0, boom
	}
	const callers = 8
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, err := c.GetOrFill(context.Background(), k, fill, nil, "")
			errs <- err
		}()
	}
	// Once the leader's fill runs, its call is registered until the fill returns, so every caller inside Do joins it:
	// none can start a second flight.
	waitFor(t, func() bool { return fills.Load() == 1 && inFlight() == callers })
	close(release)
	for range callers {
		if err := <-errs; !errors.Is(err, boom) {
			t.Errorf("err = %v, want boom", err)
		}
	}
	if n := fills.Load(); n != 1 {
		t.Errorf("fills = %d, want 1 for %d callers of one failing flight", n, callers)
	}
}

// TestTyped_GetOrFillLeaderCancelled: when the caller that started a flight leaves, its fill is neither stored nor
// handed to a joined caller, which fills again under its own live context.
func TestTyped_GetOrFillLeaderCancelled(t *testing.T) {
	s := typedSpec(t)
	c := New[string](s, 100)
	k := mustKey(t, s, User("u"), Content("c"))
	leaderCtx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var calls atomic.Int32
	fill := func(ctx context.Context) (string, int64, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done() // the leader's walk notices its cancellation
			return "truncated", 1, nil
		}
		return "complete", 1, nil
	}
	leaderErr := make(chan error, 1)
	go func() {
		_, err := c.GetOrFill(leaderCtx, k, fill, nil, "")
		leaderErr <- err
	}()
	<-started
	joined := make(chan string, 1)
	go func() {
		v, err := c.GetOrFill(context.Background(), k, fill, nil, "")
		if err != nil {
			t.Error(err)
		}
		joined <- v
	}()
	// Let the joiner reach the flight before the leader leaves (it would also be correct if it did not).
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Errorf("leader err = %v, want its own cancellation", err)
	}
	if v := <-joined; v != "complete" {
		t.Errorf("joiner got %q, want a fresh complete fill", v)
	}
	if v, ok := c.Get(k); !ok || v != "complete" {
		t.Errorf("stored %q, %v; want only the complete fill", v, ok)
	}
}

// TestTyped_UnusableKeys: the zero Key (Key refused its parts) never touches the cache, and a key of another spec
// panics in a test binary and skips the cache in production.
func TestTyped_UnusableKeys(t *testing.T) {
	s, other := typedSpec(t), testSpec(t, "x.other", Keys(KindTenant, KindContent), ReplicaLocal("test"))
	c := New[int](s, 100)
	fills := 0
	fill := func(context.Context) (int, int64, error) { fills++; return 7, 1, nil }
	for range 2 {
		if v, err := c.GetOrFill(context.Background(), Key{}, fill, nil, ""); err != nil || v != 7 {
			t.Errorf("zero key fill = %d, %v", v, err)
		}
	}
	c.Set(Key{}, 1, 1, 0)
	c.Delete(Key{})
	if _, ok := c.Get(Key{}); ok || fills != 2 || c.Len() != 0 {
		t.Errorf("zero key: fills %d len %d; want 2 uncached fills", fills, c.Len())
	}
	foreign := mustKey(t, other, User("u"), Content("c"))
	mustPanic(t, "Get with another spec's key", func() { c.Get(foreign) })
	restore := SetKeyErrorPanicForTesting(false)
	defer restore()
	c.Set(foreign, 1, 1, 0)
	if v, err := c.GetOrFill(context.Background(), foreign, fill, nil, ""); err != nil || v != 7 || c.Len() != 0 {
		t.Errorf("foreign key in production: %d, %v, len %d; want an uncached fill", v, err, c.Len())
	}
	if st := s.Info().Stats; st.Rejected != 3 || st.Hits+st.Misses+st.Stores != 0 {
		t.Errorf("stats %+v; want 3 rejected (the foreign uses) and nothing else", st)
	}
}

// TestTyped_SizeOfCollectedCachesDropsOut: a spec holds its caches weakly, so a cache built per service instance
// stops counting once it is collected.
func TestTyped_SizeOfCollectedCachesDropsOut(t *testing.T) {
	s := typedSpec(t)
	keep := New[int](s, 100)
	keep.Set(mustKey(t, s, Local(), Content("k")), 1, 5, 0)
	func() {
		dropped := New[int](s, 100)
		dropped.Set(mustKey(t, s, Local(), Content("d")), 1, 7, 0)
	}()
	for range 10 {
		runtime.GC()
		if st := s.Info().Stats; st.Entries == 1 && st.Cost == 5 {
			break
		}
	}
	if st := s.Info().Stats; st.Entries != 1 || st.Cost != 5 {
		t.Errorf("entries/cost = %d/%d, want only the live cache's 1/5", st.Entries, st.Cost)
	}
	sizers := func() int {
		s.stats.mu.Lock()
		defer s.stats.mu.Unlock()
		return len(s.stats.sizers)
	}
	for i := range 100 {
		New[int](s, 1)
		if i%10 == 0 {
			runtime.GC()
		}
	}
	if n := sizers(); n > 60 {
		t.Errorf("%d sizers retained over 100 collected caches; Track should prune them", n)
	}
	runtime.GC()
	s.Info()
	if n := sizers(); n != 1 {
		t.Errorf("%d sizers after a snapshot, want the live cache's only", n)
	}
	runtime.KeepAlive(keep)
}

// TestTyped_SharedSpecs: Typed is replica-local storage, so a Shared spec may build one only where the sharing is
// the invalidation fan-out, which leaves the entries in each replica's memory and tells the others to drop them
// (MechanismInvalidation). Any other mechanism would promise entries Typed cannot share.
func TestTyped_SharedSpecs(t *testing.T) {
	fanned := testSpec(t, "x.fanned", Keys(KindTenant), Shared(MechanismInvalidation, "dropped on every replica"))
	c := New[int](fanned, 1)
	k, _ := fanned.Key(Org("acme"))
	c.Set(k, 7, 1, 0)
	if v, ok := c.Get(k); !ok || v != 7 {
		t.Errorf("Get after Set on a fan-out spec = %v, %v, want 7, true", v, ok)
	}
	other := testSpec(t, "x.other", Keys(KindTenant), Shared("redis", "entries live in Redis"))
	mustPanic(t, "New with a Shared spec whose mechanism Typed cannot provide", func() { New[int](other, 1) })
	mustPanic(t, "New with a nil spec", func() { New[int](nil, 1) })
}

// TestTyped_Clear: the whole cache goes at once (an invalidation fan-out resync) and is counted as deletions.
func TestTyped_Clear(t *testing.T) {
	s := testSpec(t, "x.clear", Keys(KindTenant), ReplicaLocal("recomputed on a miss"))
	c := New[string](s, 10)
	for _, org := range []string{"a", "b", "c"} {
		k, _ := s.Key(Org(org))
		c.Set(k, org, 1, 0)
	}
	before := s.Info().Stats.Evictions[EvictDeleted]
	c.Clear()
	if c.Len() != 0 || c.Cost() != 0 {
		t.Errorf("after Clear: %d entries, cost %d, want none", c.Len(), c.Cost())
	}
	if got := s.Info().Stats.Evictions[EvictDeleted] - before; got != 3 {
		t.Errorf("Clear counted %d deletions, want 3", got)
	}
	k, _ := s.Key(Org("a"))
	if _, ok := c.Get(k); ok {
		t.Error("an entry survived Clear")
	}
	c.Set(k, "again", 1, 0)
	if v, ok := c.Get(k); !ok || v != "again" {
		t.Errorf("the cache is not usable after Clear: %v, %v", v, ok)
	}
}

// TestRecord_OwnStorage: a cache with its own storage records its events and size through its spec.
func TestRecord_OwnStorage(t *testing.T) {
	s := typedSpec(t)
	own := NewSizedLRU(10)
	Track(s, own)
	own.SetWithCost(context.Background(), "k", 1, 3, 0)
	s.RecordHit()
	s.RecordMiss()
	s.RecordStore()
	s.RecordEviction(EvictExpired)
	s.RecordEviction(numEvictReasons) // out of range: ignored
	st := s.Info().Stats
	if st.Hits != 1 || st.Misses != 1 || st.Stores != 1 || st.Evictions[EvictExpired] != 1 || st.Entries != 1 || st.Cost != 3 {
		t.Errorf("stats = %+v", st)
	}
	runtime.KeepAlive(own)
}

func TestEvictReason_String(t *testing.T) {
	want := []string{"capacity", "expired", "replaced", "deleted"}
	for i, r := range EvictReasons() {
		if r.String() != want[i] {
			t.Errorf("reason %d = %q, want %q (a metric label value)", i, r, want[i])
		}
	}
	if numEvictReasons.String() != "invalid" {
		t.Error("out-of-range reason has a name")
	}
}
