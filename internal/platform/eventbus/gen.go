package eventbus

import (
	"context"
	"hash/maphash"
	"sync"
	"sync/atomic"

	"pad-analyzer/internal/platform/txhook"
)

const genStripes = 256

// Gen guards a cache's fills against its invalidations. A reader captures the key's generation before it reads
// the source, and stores what it read only if FillOK: no invalidation of the key since, and the reader's own
// transaction has published no change (what it read may then be its uncommitted writes). Without it, a reader
// that read before a writer committed but stored after the invalidation leaves a stale entry until its TTL.
//
// Generations are striped: keys share 256 counters, so memory does not grow with the keys, and an invalidation
// of one key only ever costs another key sharing its stripe a skipped fill, never a stale one. The zero value is
// ready to use.
type Gen[K comparable] struct {
	seedOnce sync.Once
	seed     maphash.Seed
	all      atomic.Uint64
	stripes  [genStripes]atomic.Uint64
}

func (g *Gen[K]) stripe(k K) *atomic.Uint64 {
	g.seedOnce.Do(func() { g.seed = maphash.MakeSeed() })
	return &g.stripes[maphash.Comparable(g.seed, k)%genStripes]
}

// Load returns k's generation, to capture before reading what will fill the cache.
func (g *Gen[K]) Load(k K) uint64 {
	return g.stripe(k).Load() + g.all.Load()
}

// Bump invalidates k: fills that captured its generation before now are refused.
func (g *Gen[K]) Bump(k K) {
	g.stripe(k).Add(1)
}

// BumpAll invalidates every key.
func (g *Gen[K]) BumpAll() {
	g.all.Add(1)
}

// FillOK reports whether a value read for k after Load(k) returned captured may be stored.
func (g *Gen[K]) FillOK(ctx context.Context, k K, captured uint64) bool {
	return !txhook.Published(ctx) && g.Load(k) == captured
}
