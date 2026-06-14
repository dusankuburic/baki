package cache

import (
	"context"
	"testing"
)

func TestLRUCache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"set and get basic operation", func(t *testing.T) {
			t.Parallel()
			c, err := NewLRUCache(10)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			c.Set(ctx, "key1", "value1", 0)
			val, ok := c.Get(ctx, "key1")
			if !ok {
				t.Fatal("expected key1 to be found")
			}
			if val != "value1" {
				t.Fatalf("expected value1, got %v", val)
			}
		}},

		{"get missing key returns false", func(t *testing.T) {
			t.Parallel()
			c, err := NewLRUCache(10)
			if err != nil {
				t.Fatal(err)
			}
			_, ok := c.Get(context.Background(), "missing")
			if ok {
				t.Fatal("expected missing key to return false")
			}
		}},

		{"eviction when capacity exceeded", func(t *testing.T) {
			t.Parallel()
			c, err := NewLRUCache(2)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			c.Set(ctx, "a", 1, 0)
			c.Set(ctx, "b", 2, 0)
			c.Set(ctx, "c", 3, 0)

			_, ok := c.Get(ctx, "a")
			if ok {
				t.Fatal("expected key 'a' to be evicted")
			}
			if val, found := c.Get(ctx, "b"); !found || val != 2 {
				t.Fatal("expected key 'b' to exist with value 2")
			}
			if val, found := c.Get(ctx, "c"); !found || val != 3 {
				t.Fatal("expected key 'c' to exist with value 3")
			}
		}},

		{"delete removes item", func(t *testing.T) {
			t.Parallel()
			c, err := NewLRUCache(10)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			c.Set(ctx, "key1", "value1", 0)
			c.Delete(ctx, "key1")

			_, ok := c.Get(ctx, "key1")
			if ok {
				t.Fatal("expected key1 to be deleted")
			}
		}},

		{"len returns correct count", func(t *testing.T) {
			t.Parallel()
			c, err := NewLRUCache(10)
			if err != nil {
				t.Fatal(err)
			}
			lc := c.(*lruCache)
			ctx := context.Background()

			if lc.inner.Len() != 0 {
				t.Fatalf("expected len 0, got %d", lc.inner.Len())
			}
			c.Set(ctx, "a", 1, 0)
			c.Set(ctx, "b", 2, 0)
			if lc.inner.Len() != 2 {
				t.Fatalf("expected len 2, got %d", lc.inner.Len())
			}
		}},

		{"overwrite existing key updates value", func(t *testing.T) {
			t.Parallel()
			c, err := NewLRUCache(10)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			c.Set(ctx, "key1", "old", 0)
			c.Set(ctx, "key1", "new", 0)

			val, ok := c.Get(ctx, "key1")
			if !ok {
				t.Fatal("expected key1 to be found")
			}
			if val != "new" {
				t.Fatalf("expected 'new', got %v", val)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
