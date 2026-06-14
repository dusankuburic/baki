package cache

import (
	"context"
	"time"
)

// Cache defines a generic caching interface.
type Cache interface {
	Get(ctx context.Context, key string) (any, bool)
	Set(ctx context.Context, key string, value any, ttl time.Duration)
	Delete(ctx context.Context, key string)
}
