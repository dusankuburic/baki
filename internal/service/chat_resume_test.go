package service

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestSliceFrom_RuneBoundary covers the delta-resume offset, which arrives as a
// raw byte count on a public endpoint and is therefore untrusted.
//
// Slicing mid-rune produces invalid UTF-8, and encoding/json does not reject
// that — it substitutes U+FFFD, so a caller silently receives a replacement
// character welded to the front of the resumed tail. That is corruption with no
// error anywhere.
func TestSliceFrom_RuneBoundary(t *testing.T) {
	const buf = "héllo wörld ✅ done" // multi-byte at offsets 1, 8 and 12

	t.Run("every byte offset yields valid UTF-8", func(t *testing.T) {
		for from := -3; from <= len(buf)+3; from++ {
			got := sliceFrom(buf, from)
			if !utf8.ValidString(got) {
				t.Errorf("sliceFrom(buf, %d) = %q — not valid UTF-8", from, got)
			}
			// Whatever comes back must be a genuine suffix of the buffer, never
			// a re-encoded or truncated variant.
			if got != "" && !strings.HasSuffix(buf, got) {
				t.Errorf("sliceFrom(buf, %d) = %q — not a suffix of the buffer", from, got)
			}
		}
	})

	t.Run("mid-rune snaps down, never dropping a character", func(t *testing.T) {
		// Byte 2 is the continuation byte of "é" (bytes 1-2).
		got := sliceFrom(buf, 2)
		if !strings.HasPrefix(got, "é") {
			t.Errorf("sliceFrom(buf, 2) = %q, want it to start at the 'é' it straddled", got)
		}
	})

	t.Run("boundaries are exact", func(t *testing.T) {
		if got := sliceFrom(buf, 0); got != buf {
			t.Errorf("from=0 must return the whole buffer, got %q", got)
		}
		if got := sliceFrom(buf, -1); got != buf {
			t.Errorf("negative from must return the whole buffer, got %q", got)
		}
		if got := sliceFrom(buf, len(buf)); got != "" {
			t.Errorf("from=len must return empty, got %q", got)
		}
		if got := sliceFrom(buf, len(buf)+99); got != "" {
			t.Errorf("from past the end must return empty, got %q", got)
		}
		// An exact rune boundary must not be shifted.
		if got := sliceFrom(buf, 13); got != " ✅ done" {
			t.Errorf("from=13 (exact boundary) = %q, want %q", got, " ✅ done")
		}
	})
}

// newMiniRedis stands up an in-process Redis so the cross-replica resume path
// can be exercised without an external dependency.
//
// The project previously deferred work on this path for want of a Redis test
// harness ("modifying them without a Redis test harness repeats the documented
// but NOT validated trap"). miniredis was already a dependency and already used
// by the rate-limiter and WebSocket backplane suites; the resume backplane just
// never got one, so it was the only Redis path with zero coverage.
func newMiniRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestRedisResumeStore_CrossReplica is the point of the backplane: a stream
// mirrored by the replica that ran it must be resumable from a replica that
// never saw it.
func TestRedisResumeStore_CrossReplica(t *testing.T) {
	client := newMiniRedis(t)
	ctx := context.Background()

	writer := redisResumeStore{c: client} // replica that ran the stream
	reader := redisResumeStore{c: client} // replica the client reconnected to

	if !writer.enabled() {
		t.Fatal("a Redis-backed store must report enabled")
	}

	want := resumeSnapshot{
		Owner: "user-42", Text: "partial answer ✅", Done: false,
		TokensIn: 11, TokensOut: 22,
	}
	writer.Save(ctx, "stream-1", want, time.Minute)

	got, ok := reader.Load(ctx, "stream-1")
	if !ok {
		t.Fatal("snapshot written by one replica was not visible to another")
	}
	if got.Owner != want.Owner || got.Text != want.Text ||
		got.TokensIn != want.TokensIn || got.TokensOut != want.TokensOut || got.Done != want.Done {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	// A later Save must overwrite, not append — the mirror republishes the whole
	// buffer on every tick.
	writer.Save(ctx, "stream-1", resumeSnapshot{Owner: "user-42", Text: "full answer ✅", Done: true}, time.Minute)
	got, ok = reader.Load(ctx, "stream-1")
	if !ok {
		t.Fatal("snapshot missing after overwrite")
	}
	if got.Text != "full answer ✅" || !got.Done {
		t.Errorf("overwrite not observed: %+v", got)
	}
}

// TestRedisResumeStore_MissAndFailOpen pins the two degraded paths. Both must
// report "not found" rather than erroring: resume is best-effort, and a
// backplane problem must never surface as a broken chat.
func TestRedisResumeStore_MissAndFailOpen(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown stream is a miss", func(t *testing.T) {
		store := redisResumeStore{c: newMiniRedis(t)}
		if _, ok := store.Load(ctx, "never-existed"); ok {
			t.Error("expected a miss for an unknown stream")
		}
	})

	t.Run("unreachable backplane degrades to a miss", func(t *testing.T) {
		client := newMiniRedis(t)
		_ = client.Close() // simulate a Redis outage
		store := redisResumeStore{c: client}
		// Save must not panic and must not block.
		store.Save(ctx, "s", resumeSnapshot{Owner: "u"}, time.Minute)
		if _, ok := store.Load(ctx, "s"); ok {
			t.Error("expected a miss when the backplane is unreachable")
		}
	})

	t.Run("corrupt payload is a miss, not a panic", func(t *testing.T) {
		client := newMiniRedis(t)
		store := redisResumeStore{c: client}
		if err := client.Set(ctx, store.key("bad"), "not-json", time.Minute).Err(); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, ok := store.Load(ctx, "bad"); ok {
			t.Error("expected a miss for an unparseable snapshot")
		}
	})
}

// TestNoopResumeStore_IsInert guards the single-replica default: the no-op store
// must report disabled (so mirrorStream is never started) and never claim a hit.
func TestNoopResumeStore_IsInert(t *testing.T) {
	var s resumeStore = noopResumeStore{}
	if s.enabled() {
		t.Error("noop store must report disabled so the mirror goroutine is skipped")
	}
	s.Save(context.Background(), "s", resumeSnapshot{Owner: "u"}, time.Minute)
	if _, ok := s.Load(context.Background(), "s"); ok {
		t.Error("noop store must never report a hit")
	}
}
