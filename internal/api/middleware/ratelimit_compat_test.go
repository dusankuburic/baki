package middleware

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pad-analyzer/internal/testutil/redisreplica"
)

// A token bucket a release-0043 replica left in Redis (the hash after one admitted request, captured from that tree
// before the key registry existed) keeps counting under this release's script and key builder: the key is the same
// string, so a rolling deploy shares one bucket instead of splitting it.
func TestPrevRelease_BucketHashKeepsCounting(t *testing.T) {
	var fx struct {
		Key    string            `json:"key"`
		Fields map[string]string `json:"fields"`
		TTLMs  int64             `json:"ttlMs"`
		With   struct {
			Rate     float64 `json:"rate"`
			Capacity float64 `json:"capacity"`
			NowMs    int64   `json:"nowMs"`
		} `json:"capturedWith"`
	}
	if err := json.Unmarshal(redisreplica.PrevFixture(t, "0043", "ratelimit_bucket.json"), &fx); err != nil {
		t.Fatal(err)
	}
	if got := BucketKey("general", "10.0.0.1"); got != fx.Key {
		t.Fatalf("BucketKey = %q, but release 0043 wrote %q", got, fx.Key)
	}

	c := redisreplica.New(t, 1)
	for field, value := range fx.Fields {
		c.Redis.HSet(fx.Key, field, value)
	}
	c.Redis.SetTTL(fx.Key, time.Duration(fx.TTLMs)*time.Millisecond)

	s := newRedisStore(c.Replicas[0].Client)
	s.now = func() time.Time { return time.UnixMilli(fx.With.NowMs + 2000) } // two seconds later: two tokens refilled
	if !s.Allow(context.Background(), fx.Key, fx.With.Rate, fx.With.Capacity) {
		t.Fatal("a bucket with tokens left refused a request")
	}
	// 4 tokens + 2 refilled, capped at the capacity 5, minus the one just taken.
	if got := c.Redis.HGet(fx.Key, "tokens"); got != "4" {
		t.Errorf("tokens = %q, want 4", got)
	}
	if ttl := c.Redis.TTL(fx.Key); ttl <= 0 || ttl > bucketKey.Spec().TTL {
		t.Errorf("TTL = %v, want within the declared %v", ttl, bucketKey.Spec().TTL)
	}
}

// The key shapes are shipped (testdata/ratelimit_groups.golden in internal/api pins every group): BucketKey builds
// ratelimit:<group>:<addr> exactly, including for the per-user forms.
func TestBucketKey_Shapes(t *testing.T) {
	for _, tc := range []struct{ group, addr, want string }{
		{"general", "203.0.113.7", "ratelimit:general:203.0.113.7"},
		{"channel_test", "2001:db8::/64", "ratelimit:channel_test:2001:db8::/64"},
		{"peruser", "ip:203.0.113.7", "ratelimit:peruser:ip:203.0.113.7"},
		{"peruser", "abc123", "ratelimit:peruser:abc123"},
	} {
		if got := BucketKey(tc.group, tc.addr); got != tc.want {
			t.Errorf("BucketKey(%q, %q) = %q, want %q", tc.group, tc.addr, got, tc.want)
		}
	}
}
