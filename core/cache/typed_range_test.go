package cache

import (
	"testing"
	"time"
)

var rangeSpec = Declare("cachetest.range", Keys(KindParam), Global("test"), ReplicaLocal("test"))

// Range walks the unexpired entries, most recently used first, without refreshing recency or counting lookups, and
// stops when fn returns false.
func TestTyped_RangeWalksLiveEntriesWithoutTouchingThem(t *testing.T) {
	c := New[int](rangeSpec, 3, WithMaxEntries(3))
	key := func(s string) Key {
		k, err := rangeSpec.Key(Param("k", s))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	c.Set(key("a"), 1, 1, 0)
	c.Set(key("b"), 2, 1, 0)
	c.Set(key("gone"), 9, 1, time.Nanosecond)
	time.Sleep(time.Millisecond)
	before := rangeSpec.Info().Stats

	var got []int
	c.Range(func(k Key, v int) bool {
		if k.Spec() != rangeSpec {
			t.Errorf("key of spec %v", k.Spec())
		}
		got = append(got, v)
		return true
	})
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("Range = %v, want [2 1] (most recent first, expired skipped)", got)
	}
	after := rangeSpec.Info().Stats
	if after.Hits != before.Hits || after.Misses != before.Misses {
		t.Errorf("Range counted lookups: %+v -> %+v", before, after)
	}
	// Recency untouched: the next store past the cap still evicts "a", the least recently used.
	c.Set(key("c"), 3, 1, 0)
	if _, ok := c.Get(key("a")); ok {
		t.Error("Range refreshed the recency of the entries it walked")
	}

	n := 0
	c.Range(func(Key, int) bool { n++; return false })
	if n != 1 {
		t.Errorf("Range went on after fn returned false: %d calls", n)
	}
}
