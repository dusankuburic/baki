package cache

import (
	"sync"
	"sync/atomic"
	"weak"
)

// EvictReason is why an entry left a cache.
type EvictReason uint8

const (
	EvictCapacity EvictReason = iota // pushed out to fit the cost budget or the entry cap
	EvictExpired                     // its TTL passed; dropped when next touched
	EvictReplaced                    // overwritten by a store under the same key (or dropped by an oversize one)
	EvictDeleted                     // removed by Delete: an invalidation
	numEvictReasons
)

func (r EvictReason) String() string {
	switch r {
	case EvictCapacity:
		return "capacity"
	case EvictExpired:
		return "expired"
	case EvictReplaced:
		return "replaced"
	case EvictDeleted:
		return "deleted"
	}
	return "invalid"
}

// EvictReasons lists every reason, in order: the closed label set of the evictions metric.
func EvictReasons() []EvictReason {
	return []EvictReason{EvictCapacity, EvictExpired, EvictReplaced, EvictDeleted}
}

// Stats is a snapshot of one spec's statistics, summed over every cache built on it.
type Stats struct {
	Hits     uint64
	Misses   uint64
	Rejected uint64 // keys Key refused (and keys of another spec): each one a lookup that skipped the cache
	Stores   uint64 // values stored (an oversize value or a fenced fill is not)
	// Evictions counts entries that left, by reason; every reason is present.
	Evictions map[EvictReason]uint64
	Entries   int   // entries held now (expired ones included until touched)
	Cost      int64 // summed cost of those entries: bytes for a byte-budgeted cache, 1 per entry for a counted one
}

// Sizer is storage whose size a spec reports: entries held and their summed cost.
type Sizer interface {
	Len() int
	Cost() int64
}

type stats struct {
	hits, misses, rejected, stores atomic.Uint64
	evictions                      [numEvictReasons]atomic.Uint64

	mu     sync.Mutex
	sizers []func() (entries int, cost int64, live bool)
	prune  int // len(sizers) at which Track next drops collected storage
}

func (s *stats) snapshot() Stats {
	out := Stats{
		Hits:      s.hits.Load(),
		Misses:    s.misses.Load(),
		Rejected:  s.rejected.Load(),
		Stores:    s.stores.Load(),
		Evictions: make(map[EvictReason]uint64, numEvictReasons),
	}
	for _, r := range EvictReasons() {
		out.Evictions[r] = s.evictions[r].Load()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.sizers[:0]
	for _, size := range s.sizers {
		if n, cost, ok := size(); ok {
			out.Entries += n
			out.Cost += cost
			live = append(live, size)
		}
	}
	clear(s.sizers[len(live):])
	s.sizers = live
	return out
}

// RecordHit counts a hit. The Record methods are for caches under s that keep their own storage; a Typed cache
// records its own events.
func (s *Spec) RecordHit() { s.stats.hits.Add(1) }

// RecordMiss counts a miss.
func (s *Spec) RecordMiss() { s.stats.misses.Add(1) }

// RecordStore counts a value stored.
func (s *Spec) RecordStore() { s.stats.stores.Add(1) }

// RecordEviction counts an entry that left, by reason.
func (s *Spec) RecordEviction(r EvictReason) {
	if r < numEvictReasons {
		s.stats.evictions[r].Add(1)
	}
}

// Track adds storage's entries and cost to s's statistics for as long as storage is reachable: s holds it only
// weakly, so storage built per service instance (and per test) drops out once collected.
func Track[T any, PT interface {
	*T
	Sizer
}](s *Spec, storage PT) {
	wp := weak.Make((*T)(storage))
	size := func() (int, int64, bool) {
		p := PT(wp.Value())
		if p == nil {
			return 0, 0, false
		}
		return p.Len(), p.Cost(), true
	}
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()
	if len(s.stats.sizers) >= s.stats.prune {
		live := s.stats.sizers[:0]
		for _, f := range s.stats.sizers {
			if _, _, ok := f(); ok {
				live = append(live, f)
			}
		}
		clear(s.stats.sizers[len(live):])
		s.stats.sizers = live
		s.stats.prune = 2*len(live) + 16
	}
	s.stats.sizers = append(s.stats.sizers, size)
}
