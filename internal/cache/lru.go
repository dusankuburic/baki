package cache

import (
	"context"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

type lruCache struct {
	inner *lru.Cache[string, cacheEntry]
}

type cacheEntry struct {
	value      any
	expiration int64
}

// NewLRUCache creates a new in-memory LRU cache with the given size.
func NewLRUCache(size int) (Cache, error) {
	inner, err := lru.New[string, cacheEntry](size)
	if err != nil {
		return nil, err
	}
	return &lruCache{inner: inner}, nil
}

func (c *lruCache) Get(ctx context.Context, key string) (any, bool) {
	entry, ok := c.inner.Get(key)
	if !ok {
		return nil, false
	}

	if entry.expiration > 0 && time.Now().UnixNano() > entry.expiration {
		c.inner.Remove(key)
		return nil, false
	}

	return entry.value, true
}

func (c *lruCache) Set(ctx context.Context, key string, value any, ttl time.Duration) {
	var expiration int64
	if ttl > 0 {
		expiration = time.Now().Add(ttl).UnixNano()
	}
	c.inner.Add(key, cacheEntry{value: value, expiration: expiration})
}

func (c *lruCache) Delete(ctx context.Context, key string) {
	c.inner.Remove(key)
}
