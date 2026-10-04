package ai

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// modelListCache holds model listings process-wide for providers whose
// Models() dials the wire (Copilot, local servers): the factory builds a
// fresh provider per request while chat consults Models() before every turn
// (modelOutputInfo), so an instance-level cache never survived a turn. Keys
// are caller-built and never hold a raw credential (copilotModelsKey).
// Bounded at max entries, soonest-expiring evicted first; concurrent misses
// share one fetch (singleflight), detached from the callers' cancellation and
// bounded by modelListFetchTimeout.
type modelListCache struct {
	max    int
	mu     sync.Mutex
	m      map[string]modelListEntry
	flight singleflight.Group
	// gen is bumped by forget: a fetch that started before a forget must not
	// store its (now stale) result afterwards.
	gen uint64
}

type modelListEntry struct {
	models  []ModelInfo
	err     error
	expires time.Time
}

// modelListFetchTimeout bounds one detached listing fetch.
const modelListFetchTimeout = 30 * time.Second

func newModelListCache(max int) *modelListCache {
	return &modelListCache{max: max, m: make(map[string]modelListEntry)}
}

// load returns the cached listing for key, fetching on a miss: success kept okTTL unless fetch reports it not live
// (a static fallback the next call must retry); a failure kept errTTL (0 = not cached) so a short negative entry
// spares a failing endpoint one call per turn without pinning an outage. The returned slice is the caller's own.
func (c *modelListCache) load(ctx context.Context, key string, okTTL, errTTL time.Duration, fetch func(context.Context) (models []ModelInfo, live bool, err error)) ([]ModelInfo, error) {
	if models, err, ok := c.get(key); ok {
		return models, err
	}
	ch := c.flight.DoChan(key, func() (any, error) {
		c.mu.Lock()
		gen := c.gen
		c.mu.Unlock()
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelListFetchTimeout)
		defer cancel()
		models, live, err := fetch(fctx)
		ttl := okTTL
		switch {
		case err != nil:
			ttl = errTTL
		case !live:
			ttl = 0
		}
		if ttl > 0 {
			c.put(key, modelListEntry{models: models, err: err, expires: time.Now().Add(ttl)}, gen)
		}
		return models, err
	})
	select {
	case r := <-ch:
		models, _ := r.Val.([]ModelInfo)
		return cloneModelInfos(models), r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *modelListCache) get(key string) ([]ModelInfo, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, nil, false
	}
	if !time.Now().Before(e.expires) {
		delete(c.m, key)
		return nil, nil, false
	}
	return cloneModelInfos(e.models), e.err, true
}

// put stores e unless forget ran since gen was read.
func (c *modelListCache) put(key string, e modelListEntry, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	if _, ok := c.m[key]; !ok && len(c.m) >= c.max {
		now := time.Now()
		victim, soonest := "", time.Time{}
		for k, v := range c.m {
			if !now.Before(v.expires) {
				delete(c.m, k)
				continue
			}
			if victim == "" || v.expires.Before(soonest) {
				victim, soonest = k, v.expires
			}
		}
		if len(c.m) >= c.max && victim != "" {
			delete(c.m, victim)
		}
	}
	c.m[key] = e
}

// forget drops key (tests; a configuration change).
func (c *modelListCache) forget(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.gen++
	c.mu.Unlock()
	// A load after this starts a new fetch rather than joining one that began
	// before it (whose result the generation check above keeps out).
	c.flight.Forget(key)
}

// size reports the number of entries (tests).
func (c *modelListCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func cloneModelInfos(in []ModelInfo) []ModelInfo {
	if in == nil {
		return nil
	}
	out := make([]ModelInfo, len(in))
	copy(out, in)
	return out
}
