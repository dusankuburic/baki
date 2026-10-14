package redisx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
)

// newSSE is a backplane for instance over a fresh miniredis (health installed on the client).
func newSSE(t *testing.T, instance string) (*miniredis.Miniredis, *redis.Client, *SSEBackplane) {
	t.Helper()
	mr, client, health := newHealth(t)
	return mr, client, NewSSEBackplane(client, health, instance)
}

// peerOf is a second backplane on the same server, as a second replica.
func peerOf(t *testing.T, mr *miniredis.Miniredis, instance string) *SSEBackplane {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(), ContextTimeoutEnabled: true,
		DialTimeout: 300 * time.Millisecond, ReadTimeout: 300 * time.Millisecond, WriteTimeout: 300 * time.Millisecond, MaxRetries: -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	return NewSSEBackplane(client, NewHealth(client), instance)
}

func fanoutCount(direction, outcome string) float64 {
	return testutil.ToFloat64(sseFanoutTotal.WithLabelValues(direction, outcome))
}
func logCount(outcome string) float64 {
	return testutil.ToFloat64(sseLogTotal.WithLabelValues(outcome))
}
func replayCount(outcome string) float64 {
	return testutil.ToFloat64(sseReplayTotal.WithLabelValues(outcome))
}
func presenceCount(outcome string) float64 {
	return testutil.ToFloat64(ssePresenceTotal.WithLabelValues(outcome))
}

func TestSSE_DeclarationsAreTheDocumentedOnes(t *testing.T) {
	for _, tc := range []struct {
		k       *Key
		kind    Kind
		pattern string
		ttl     time.Duration
	}{
		{SSEEmitChannel, KindPubSub, "baki:sse:ev", 0},
		{SSELogKey, KindStream, "baki:sse:log:{user}", 5 * time.Minute},
		{SSESubsKey, KindHash, "baki:sse:subs:{user}", time.Minute},
	} {
		s := tc.k.Spec()
		if s.Kind != tc.kind || s.Pattern != tc.pattern || s.TTL != tc.ttl || s.Version != 1 || s.OnDown != OnDownOpen {
			t.Errorf("%s = %+v, want kind %v pattern %q ttl %v v1 on-down open", s.Name, s, tc.kind, tc.pattern, tc.ttl)
		}
	}
}

func TestStreamIDs(t *testing.T) {
	for id, want := range map[string]bool{
		"1-0": true, "1700000000000-12": true, "0-1": true,
		"": false, "1": false, "-1": false, "1-": false, "a-b": false, "1-2-3": false, "1-0\n": false, "1-0\ndata: x": false,
		"18446744073709551616-0": false, // does not fit 64 bits
	} {
		if got := ValidStreamID(id); got != want {
			t.Errorf("ValidStreamID(%q) = %v, want %v", id, got, want)
		}
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1-0", "1-0", 0}, {"1-0", "1-1", -1}, {"2-0", "1-9", 1}, {"10-0", "9-0", 1}, {"9-5", "10-0", -1},
	} {
		if got := CompareStreamIDs(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareStreamIDs(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSSELog_AssignsIncreasingIDsAndBoundsTheStream(t *testing.T) {
	mr, _, b := newSSE(t, "a")
	ctx := context.Background()
	first := b.Log(ctx, []string{"u1", "u2"}, []byte(`{"name":"x"}`))
	if len(first) != 2 || !ValidStreamID(first[0]) || !ValidStreamID(first[1]) {
		t.Fatalf("Log = %q, want two stream ids", first)
	}
	second := b.Log(ctx, []string{"u1"}, []byte(`{"name":"y"}`))
	if CompareStreamIDs(second[0], first[0]) <= 0 {
		t.Errorf("second id %s is not after the first %s", second[0], first[0])
	}
	if ttl := mr.TTL(SSELogKey.Key("u1")); ttl != SSELogTTL {
		t.Errorf("log TTL = %v, want %v", ttl, SSELogTTL)
	}
	for range 400 {
		b.Log(ctx, []string{"u1"}, []byte(`{"name":"z"}`))
	}
	entries, err := mr.Stream(SSELogKey.Key("u1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 400 || len(entries) < SSELogMaxLen {
		t.Errorf("log holds %d entries after 402 writes, want the stream trimmed near %d", len(entries), SSELogMaxLen)
	}
}

func TestSSELog_OversizedFramesAreNotLogged(t *testing.T) {
	mr, _, b := newSSE(t, "a")
	before := logCount("oversize")
	ids := b.Log(context.Background(), []string{"u1"}, []byte(strings.Repeat("x", SSEMaxLoggedBytes+1)))
	if ids[0] != "" {
		t.Errorf("an oversized frame got id %q", ids[0])
	}
	if mr.Exists(SSELogKey.Key("u1")) {
		t.Error("an oversized frame was stored")
	}
	if got := logCount("oversize") - before; got != 1 {
		t.Errorf("oversize count moved by %v, want 1", got)
	}
}

func TestSSELog_RedisDownGivesNoIDAndDoesNotBlock(t *testing.T) {
	mr, _, b := newSSE(t, "a")
	mr.Close()
	start := time.Now()
	for range 6 {
		if ids := b.Log(context.Background(), []string{"u1"}, []byte(`{}`)); ids[0] != "" {
			t.Fatalf("got id %q from a dead server", ids[0])
		}
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("six logs against a dead server took %v", d)
	}
	// Three failures degrade Health; the rest are skipped without a round trip.
	if logCount("skipped") == 0 {
		t.Error("no log call was skipped once Redis was marked degraded")
	}
}

func TestSSEReplay_ReturnsWhatFollowsTheIDWithinTheWindow(t *testing.T) {
	mr, client, b := newSSE(t, "a")
	ctx := context.Background()
	key := SSELogKey.Key("u1")
	// An entry from the epoch is far outside the five-minute window.
	if err := client.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: "1-1", Values: map[string]any{"v": 1, "d": `{"name":"ancient"}`}}).Err(); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = b.Log(ctx, []string{"u1"}, fmt.Appendf(nil, `{"name":"e%d"}`, i))[0]
	}

	got, err := b.Replay(ctx, "u1", ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != ids[2] || got[1].ID != ids[3] || string(got[0].Data) != `{"name":"e2"}` {
		t.Fatalf("Replay after %s = %+v, want e2 and e3", ids[1], got)
	}

	// An id older than the window cannot be proven complete (the entries between are gone): a gap, not a partial replay.
	got, err = b.Replay(ctx, "u1", "0-1")
	var gap *ReplayGap
	if !errors.As(err, &gap) || got != nil || gap.TailID != ids[3] {
		t.Errorf("Replay after 0-1 = %v, %v, want a gap that ends at the newest entry %s", got, err, ids[3])
	}

	// Nothing after the newest.
	if got, err = b.Replay(ctx, "u1", ids[3]); err != nil || len(got) != 0 {
		t.Errorf("Replay after the newest = %v, %v, want nothing", got, err)
	}
	// Another user's log is not this user's: no frames, only the signal that this log cannot vouch for the cursor.
	if got, err = b.Replay(ctx, "u2", ids[0]); len(got) != 0 || !errors.As(err, &gap) {
		t.Errorf("Replay of an unknown user = %v, %v, want no frames and a gap", got, err)
	}
	_ = mr
}

func TestSSEReplay_ExactlyTheCapIsReplayedWhole(t *testing.T) {
	_, client, b := newSSE(t, "a")
	ctx := context.Background()
	key := SSELogKey.Key("u1")
	base := time.Now().UnixMilli()
	addCursor(t, client, key, base)
	for i := range SSEReplayMax {
		id := fmt.Sprintf("%d-%d", base, i+1)
		if err := client.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: id, Values: map[string]any{"v": 1, "d": `{"name":"e"}`}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.Replay(ctx, "u1", fmt.Sprintf("%d-0", base))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != SSEReplayMax || got[0].ID != fmt.Sprintf("%d-1", base) {
		t.Fatalf("Replay returned %d entries, want all %d (a log exactly at the cap is complete)", len(got), SSEReplayMax)
	}
}

// More pending than the cap: returning the oldest 200 and letting the live frames continue past them would lose the
// middle without a sign. The replay is a gap the caller signals, ending at the newest entry so the client's cursor
// moves past what it will refetch.
func TestSSEReplay_MoreThanTheCapIsAGapNotAPrefix(t *testing.T) {
	_, client, b := newSSE(t, "a")
	ctx := context.Background()
	key := SSELogKey.Key("u1")
	base := time.Now().UnixMilli()
	addCursor(t, client, key, base)
	for i := range 260 {
		id := fmt.Sprintf("%d-%d", base, i+1)
		if err := client.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: id, Values: map[string]any{"v": 1, "d": `{"name":"e"}`}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	before := replayCount("gap")
	got, err := b.Replay(ctx, "u1", fmt.Sprintf("%d-0", base))
	var gap *ReplayGap
	if !errors.As(err, &gap) {
		t.Fatalf("Replay of 260 pending entries = %d entries, %v; want a *ReplayGap", len(got), err)
	}
	if got != nil {
		t.Errorf("a gap replays nothing, got %d entries", len(got))
	}
	if want := fmt.Sprintf("%d-260", base); gap.TailID != want {
		t.Errorf("gap ends at %q, want the newest entry %q", gap.TailID, want)
	}
	if replayCount("gap")-before != 1 {
		t.Error("the gap was not counted")
	}
}

func TestSSEReplay_AnExpiredLogOlderThanTheWindowIsAGapWithoutATail(t *testing.T) {
	_, _, b := newSSE(t, "a")
	_, err := b.Replay(context.Background(), "u1", "5-1")
	var gap *ReplayGap
	if !errors.As(err, &gap) || gap.TailID != "" {
		t.Fatalf("Replay of an ancient cursor with no log = %v, want a gap with no tail id", err)
	}
}

func TestSSEReplay_IgnoresABadCursorAndSkipsEntriesItCannotUse(t *testing.T) {
	_, client, b := newSSE(t, "a")
	ctx := context.Background()
	key := SSELogKey.Key("u1")
	base := time.Now().UnixMilli()
	add := func(seq int, values map[string]any) string {
		id := fmt.Sprintf("%d-%d", base, seq)
		if err := client.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: id, Values: values}).Err(); err != nil {
			t.Fatal(err)
		}
		return id
	}
	add(0, map[string]any{"v": 1, "d": `{"name":"cursor"}`}) // the entry the replays below start after
	add(1, map[string]any{"v": 1, "d": `{"name":"keep"}`})
	add(2, map[string]any{"v": 2, "d": `{"name":"from-the-future"}`})
	add(3, map[string]any{"v": 1})
	add(4, map[string]any{"v": 1, "d": "{\"name\":\"two\nlines\"}"})
	add(5, map[string]any{"d": `{"name":"v0"}`})

	for _, bad := range []string{"", "nonsense", "1-0\ndata: x", "-5"} {
		before := replayCount("ignored")
		got, err := b.Replay(ctx, "u1", bad)
		if err != nil || got != nil {
			t.Errorf("Replay(%q) = %v, %v, want nothing and no error", bad, got, err)
		}
		if replayCount("ignored")-before != 1 {
			t.Errorf("Replay(%q) did not count as ignored", bad)
		}
	}
	got, err := b.Replay(ctx, "u1", fmt.Sprintf("%d-0", base))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, string(e.Data))
	}
	want := []string{`{"name":"keep"}`, `{"name":"v0"}`}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("Replay = %v, want %v (a future version, a missing body and a multi-line body are skipped)", names, want)
	}
	// Skipped entries stay: nothing here deletes.
	if entries, _ := client.XLen(ctx, key).Result(); entries != 6 {
		t.Errorf("the log holds %d entries, want all 6 (a reader never deletes)", entries)
	}
}

func TestSSEReplay_RedisDownIsAnError(t *testing.T) {
	mr, _, b := newSSE(t, "a")
	mr.Close()
	if _, err := b.Replay(context.Background(), "u1", "1-0"); err == nil {
		t.Fatal("Replay against a dead server returned no error")
	}
}

// presenceRecord reads this instance's field of user's subscriber hash.
func presenceRecord(t *testing.T, mr *miniredis.Miniredis, user, instance string) (n int, exp int64, ok bool) {
	t.Helper()
	raw := mr.HGet(SSESubsKey.Key(user), instance)
	if raw == "" {
		return 0, 0, false
	}
	var rec struct {
		V    int    `json:"v"`
		Kind string `json:"kind"`
		N    int    `json:"n"`
		Exp  int64  `json:"exp"`
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("presence record %q: %v", raw, err)
	}
	if rec.V != 1 || rec.Kind != "sse.subs" {
		t.Errorf("presence record %q lacks the envelope", raw)
	}
	return rec.N, rec.Exp, true
}

func TestSSEPresence_SetLocalWritesAnEnvelopedRecordAndRemovesIt(t *testing.T) {
	mr, _, b := newSSE(t, "a")
	ctx := context.Background()
	b.SetLocal(ctx, map[string]int{"u1": 2, "u2": 1})
	n, exp, ok := presenceRecord(t, mr, "u1", "a")
	if !ok || n != 2 {
		t.Fatalf("u1 record = n %d ok %v, want 2", n, ok)
	}
	if left := time.Until(time.UnixMilli(exp)); left <= 0 || left > SSERecordTTL {
		t.Errorf("record expires in %v, want within (0, %v]", left, SSERecordTTL)
	}
	if ttl := mr.TTL(SSESubsKey.Key("u1")); ttl != SSESubsTTL {
		t.Errorf("hash TTL = %v, want %v", ttl, SSESubsTTL)
	}
	b.SetLocal(ctx, map[string]int{"u1": 0})
	if _, _, ok := presenceRecord(t, mr, "u1", "a"); ok {
		t.Error("a user whose last stream closed keeps a record")
	}
	if _, _, ok := presenceRecord(t, mr, "u2", "a"); !ok {
		t.Error("u2's record was removed with u1's")
	}
}

func TestSSEPresence_PeerHoldsReadsOtherInstancesOnly(t *testing.T) {
	mr, _, a := newSSE(t, "a")
	b := peerOf(t, mr, "b")
	ctx := context.Background()
	a.SetLocal(ctx, map[string]int{"u1": 1})

	if held, err := a.PeerHolds("u1", 0); err != nil || held {
		t.Errorf("a.PeerHolds(u1) = %v, %v: its own record is not a peer", held, err)
	}
	if held, err := b.PeerHolds("u1", 0); err != nil || !held {
		t.Errorf("b.PeerHolds(u1) = %v, %v, want true (a holds it)", held, err)
	}
	if held, err := b.PeerHolds("nobody", 0); err != nil || held {
		t.Errorf("b.PeerHolds(nobody) = %v, %v, want false", held, err)
	}
	a.SetLocal(ctx, map[string]int{"u1": 0})
	if held, _ := b.PeerHolds("u1", 0); held {
		t.Error("b still sees u1 on a after a removed its record")
	}
}

func TestSSEPresence_ReadersIgnoreWhatIsExpiredOrUnreadableAndNeverDelete(t *testing.T) {
	mr, client, b := newSSE(t, "b")
	ctx := context.Background()
	key := SSESubsKey.Key("u1")
	now := time.Now().UnixMilli()
	rec := func(v, n int, exp int64) string {
		return fmt.Sprintf(`{"v":%d,"kind":"sse.subs","n":%d,"exp":%d}`, v, n, exp)
	}
	for field, value := range map[string]string{
		"expired":   rec(1, 1, now-1),
		"garbage":   "not json",
		"future":    rec(2, 1, now+60_000),
		"zero":      rec(1, 0, now+60_000),
		"otherkind": fmt.Sprintf(`{"v":1,"kind":"ws.presence","n":1,"exp":%d}`, now+60_000),
	} {
		if err := client.HSet(ctx, key, field, value).Err(); err != nil {
			t.Fatal(err)
		}
	}
	held, err := b.PeerHolds("u1", 0)
	if err != nil || held {
		t.Fatalf("PeerHolds = %v, %v, want false (every record is expired, unreadable or empty)", held, err)
	}
	for _, f := range []string{"expired", "garbage", "future", "zero", "otherkind"} {
		if mr.HGet(key, f) == "" {
			t.Errorf("reader deleted the %q record", f)
		}
	}
	// A live record of the next release's version 1 shape with extra fields still counts.
	if err := client.HSet(ctx, key, "c", fmt.Sprintf(`{"v":1,"kind":"sse.subs","n":3,"exp":%d,"extra":true}`, now+60_000)).Err(); err != nil {
		t.Fatal(err)
	}
	if held, _ := b.PeerHolds("u1", 0); !held {
		t.Error("a live record with an extra field was not read")
	}
}

func TestSSEPresence_CachesForMaxAgeAndFailsLoudOnRedisError(t *testing.T) {
	mr, _, b := newSSE(t, "b")
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	b.now = func() time.Time { return time.Unix(0, clock.Load()) }

	if held, err := b.PeerHolds("u1", 5*time.Second); err != nil || held {
		t.Fatalf("first read = %v, %v", held, err)
	}
	reads := presenceCount("miss")
	mr.Close()
	// Inside max age the answer comes from the cache, Redis is not asked.
	if held, err := b.PeerHolds("u1", 5*time.Second); err != nil || held {
		t.Errorf("cached read = %v, %v, want false and no error", held, err)
	}
	if presenceCount("hit") == 0 {
		t.Error("the cached read was not counted as a hit")
	}
	// Past it, the read goes to Redis and its failure is reported, never guessed.
	clock.Add(int64(6 * time.Second))
	if _, err := b.PeerHolds("u1", 5*time.Second); err == nil {
		t.Error("an unreadable presence returned no error: a caller cannot tell 'nobody' from 'unknown'")
	}
	if presenceCount("failed") == 0 {
		t.Error("the failed read was not counted")
	}
	_ = reads
}

func TestSSEPresence_ADegradedRedisIsNotAskedAndReportsAnError(t *testing.T) {
	mr, _, b := newSSE(t, "b")
	mr.Close()
	for i := range 4 {
		_, _ = b.PeerHolds(fmt.Sprintf("u%d", i), 0)
	}
	start := time.Now()
	if _, err := b.PeerHolds("other", 0); err == nil {
		t.Error("a degraded Redis answered a presence read")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("a degraded read took %v, want it skipped", d)
	}
}

func TestSSEPresence_HintNeverBlocksOnAStaleEntry(t *testing.T) {
	mr, _, a := newSSE(t, "a")
	b := peerOf(t, mr, "b")
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	b.now = func() time.Time { return time.Unix(0, clock.Load()) }

	// No entry yet: a synchronous read.
	if b.PeerHint("u1") {
		t.Fatal("hint says u1 is held before anyone connected")
	}
	a.SetLocal(context.Background(), map[string]int{"u1": 1})
	// Fresh entry: still the old answer, no read.
	if b.PeerHint("u1") {
		t.Error("hint re-read inside its freshness window")
	}
	// Stale: answers from the entry at once and refreshes in the background.
	clock.Add(int64(2 * time.Second))
	if b.PeerHint("u1") {
		t.Error("a stale hint blocked to re-read instead of answering from the entry")
	}
	waitUntil(t, "the background refresh", func() bool { return b.PeerHint("u1") })
}

func TestSSEPresence_CacheStaysBounded(t *testing.T) {
	_, _, b := newSSE(t, "b")
	for i := range ssePeerCacheMax + 50 {
		_, _ = b.PeerHolds(fmt.Sprintf("user-%d", i), time.Minute)
	}
	b.peers.mu.Lock()
	n := len(b.peers.m)
	b.peers.mu.Unlock()
	if n > ssePeerCacheMax {
		t.Errorf("the presence cache holds %d entries, bound is %d", n, ssePeerCacheMax)
	}
}

// receive runs the backplane's subscription and collects what it delivers.
type received struct {
	mu   sync.Mutex
	msgs []SSEMessage
}

func (r *received) add(m SSEMessage) { r.mu.Lock(); r.msgs = append(r.msgs, m); r.mu.Unlock() }
func (r *received) len() int         { r.mu.Lock(); defer r.mu.Unlock(); return len(r.msgs) }
func (r *received) at(i int) SSEMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.msgs[i]
}

func runBackplane(t *testing.T, b *SSEBackplane) (*received, context.CancelFunc) {
	t.Helper()
	got := &received{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.Run(ctx, got.add) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, "the subscription", b.Subscribed)
	return got, cancel
}

func TestSSERun_DeliversAPeersMessageAndNotItsOwn(t *testing.T) {
	mr, _, a := newSSE(t, "a")
	b := peerOf(t, mr, "b")
	gotA, _ := runBackplane(t, a)
	gotB, _ := runBackplane(t, b)

	a.Publish(SSEMessage{Name: "flow:loaded", To: []SSETarget{{User: "u1", ID: "5-1"}, {User: "u2"}}, Data: json.RawMessage(`{"name":"flow:loaded","data":1}`)})
	waitUntil(t, "b to receive", func() bool { return gotB.len() == 1 })
	m := gotB.at(0)
	if m.Origin != "a" || m.V != 1 || m.Kind != "sse.emit" || m.Name != "flow:loaded" || len(m.To) != 2 || m.To[0].ID != "5-1" || string(m.Data) != `{"name":"flow:loaded","data":1}` {
		t.Errorf("received %+v", m)
	}
	time.Sleep(50 * time.Millisecond)
	if gotA.len() != 0 {
		t.Errorf("the origin received its own message (%d)", gotA.len())
	}
	if fanoutCount("receive", "self") == 0 {
		t.Error("the filtered self-echo was not counted")
	}
}

func TestSSERun_DropsWhatItCannotUse(t *testing.T) {
	mr, client, a := newSSE(t, "a")
	got, _ := runBackplane(t, a)
	ctx := context.Background()
	publish := func(payload string) {
		t.Helper()
		if err := client.Publish(ctx, SSEEmitChannel.Key(), payload).Err(); err != nil {
			t.Fatal(err)
		}
	}
	ignored, unknown := fanoutCount("receive", "ignored"), count("sse.emit", ResultUnknown)
	publish(`{"v":2,"kind":"sse.emit","o":"b","name":"x","all":true,"d":{"name":"x"}}`) // a future version
	publish(`{"v":1,"kind":"other.kind","o":"b","name":"x","all":true,"d":{"name":"x"}}`)
	publish(`not json`)
	publish(`{"v":1,"kind":"sse.emit","o":"b","name":"x","all":true}`)                                       // no body
	publish("{\"v\":1,\"kind\":\"sse.emit\",\"o\":\"b\",\"name\":\"x\",\"all\":true,\"d\":{\"a\":\"b\"\n}}") // body with a newline: it would end the SSE event early
	publish(`{"v":1,"kind":"sse.emit","o":"b","name":"x","d":{"name":"x"}}`)                                 // no audience
	// A good one, behind them: proves the loop survived.
	publish(`{"v":1,"kind":"sse.emit","o":"b","name":"x","to":[{"u":"u1","id":"1-0\ndata: forged"},{"u":"u2","id":"7-3"}],"d":{"name":"x"}}`)
	waitUntil(t, "the good message", func() bool { return got.len() == 1 })
	time.Sleep(50 * time.Millisecond)
	if got.len() != 1 {
		t.Fatalf("delivered %d messages, want only the valid one", got.len())
	}
	m := got.at(0)
	if m.To[0].User != "u1" || m.To[0].ID != "" {
		t.Errorf("an id with a line break survived: %+v", m.To[0])
	}
	if m.To[1].ID != "7-3" {
		t.Errorf("a valid id was dropped: %+v", m.To[1])
	}
	if d := fanoutCount("receive", "ignored") - ignored; d != 4 {
		t.Errorf("ignored moved by %v, want 4 (bad JSON, no body, a body with a line break, no audience)", d)
	}
	if d := count("sse.emit", ResultUnknown) - unknown; d != 2 {
		t.Errorf("unknown moved by %v, want 2 (a future version and another kind)", d)
	}
	_ = mr
}

func TestSSEPublish_NeverBlocksWhenNothingDrainsTheQueue(t *testing.T) {
	_, _, a := newSSE(t, "a")
	dropped := fanoutCount("publish", "dropped")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ssePublishQueue + 200 {
			a.Publish(SSEMessage{Name: "x", All: true, Data: json.RawMessage(`{}`)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a full queue")
	}
	if fanoutCount("publish", "dropped")-dropped < 200 {
		t.Errorf("dropped %v, want at least 200", fanoutCount("publish", "dropped")-dropped)
	}
}

func TestSSEPublish_ByteBudgetBoundsTheQueue(t *testing.T) {
	_, _, a := newSSE(t, "a")
	big := json.RawMessage(`"` + strings.Repeat("x", 200<<10) + `"`)
	dropped := fanoutCount("publish", "dropped")
	for range 200 { // 40 MB offered, the budget is smaller
		a.Publish(SSEMessage{Name: "x", All: true, Data: big})
	}
	if fanoutCount("publish", "dropped") == dropped {
		t.Error("200 messages of 200 KiB were all queued: the queue has no byte bound")
	}
}

func TestSSEPublish_RedisDownIsCountedAndHarmless(t *testing.T) {
	mr, _, a := newSSE(t, "a")
	runBackplane(t, a)
	mr.Close()
	failed := fanoutCount("publish", "failed") + fanoutCount("publish", "skipped")
	for range 5 {
		a.Publish(SSEMessage{Name: "x", All: true, Data: json.RawMessage(`{}`)})
	}
	waitUntil(t, "the failures to be counted", func() bool {
		return fanoutCount("publish", "failed")+fanoutCount("publish", "skipped") >= failed+5
	})
}

func TestSSERun_ResubscribesWhenRedisReturns(t *testing.T) {
	mr, _, a := newSSE(t, "a")
	b := peerOf(t, mr, "b")
	got, _ := runBackplane(t, a)
	runBackplane(t, b)
	mr.Close()
	waitUntil(t, "the subscription to notice", func() bool { return !a.Subscribed() })
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the resubscription", func() bool { return a.Subscribed() && b.Subscribed() })
	b.Publish(SSEMessage{Name: "x", All: true, Data: json.RawMessage(`{"name":"x"}`)})
	waitUntil(t, "a message after the outage", func() bool { return got.len() == 1 })
}

func TestSSELog_AnAudienceBeyondTheBoundGetsNoIDsForTheRest(t *testing.T) {
	mr, _, b := newSSE(t, "a")
	users := make([]string, sseLogMaxUsers+20)
	for i := range users {
		users[i] = fmt.Sprintf("user-%d", i)
	}
	ids := b.Log(context.Background(), users, []byte(`{"name":"x"}`))
	for i, id := range ids {
		if want := i < sseLogMaxUsers; (id != "") != want {
			t.Fatalf("user %d: id %q, want an id only for the first %d", i, id, sseLogMaxUsers)
		}
	}
	if mr.Exists(SSELogKey.Key(users[sseLogMaxUsers])) {
		t.Error("a user past the bound has a log")
	}
}

func TestSSEPublishHeld_SendsOnlyToUsersAnotherReplicaHolds(t *testing.T) {
	mr, _, a := newSSE(t, "a")
	b := peerOf(t, mr, "b")
	gotA, _ := runBackplane(t, a)
	gotB, _ := runBackplane(t, b)
	_ = gotA
	b.SetLocal(context.Background(), map[string]int{"held": 1})

	a.PublishHeld(SSEMessage{Name: "chat:event", To: []SSETarget{{User: "held"}, {User: "alone"}}, Data: json.RawMessage(`{"name":"chat:event"}`)})
	waitUntil(t, "the message", func() bool { return gotB.len() == 1 })
	if m := gotB.at(0); len(m.To) != 1 || m.To[0].User != "held" {
		t.Errorf("published to %+v, want only the held user", m.To)
	}
	skipped := fanoutCount("publish", "skipped")
	a.PublishHeld(SSEMessage{Name: "chat:event", To: []SSETarget{{User: "alone"}}, Data: json.RawMessage(`{}`)})
	if fanoutCount("publish", "skipped")-skipped != 1 {
		t.Error("a message for nobody held was not skipped")
	}
	time.Sleep(50 * time.Millisecond)
	if gotB.len() != 1 {
		t.Errorf("b received %d messages, want 1", gotB.len())
	}
}

func TestSSELog_ABigFrameIsLoggedForFewerUsers(t *testing.T) {
	_, _, b := newSSE(t, "a")
	users := make([]string, 200)
	for i := range users {
		users[i] = fmt.Sprintf("user-%d", i)
	}
	before := logCount("truncated")
	ids := b.Log(context.Background(), users, []byte(strings.Repeat("x", SSEMaxLoggedBytes)))
	logged := 0
	for _, id := range ids {
		if id != "" {
			logged++
		}
	}
	if want := sseLogMaxPipelineBytes / SSEMaxLoggedBytes; logged != want {
		t.Errorf("a frame of %d bytes was logged for %d users, want %d (the pipeline byte bound)", SSEMaxLoggedBytes, logged, want)
	}
	if got := logCount("truncated") - before; got != float64(200-logged) {
		t.Errorf("truncated moved by %v, want %d", got, 200-logged)
	}
}

// addCursor puts the entry a reconnecting client would hold (base-0) into the log, so a replay after it is a replay
// and not a cursor the log cannot vouch for.
func addCursor(t *testing.T, client *redis.Client, key string, base int64) {
	t.Helper()
	if err := client.XAdd(context.Background(), &redis.XAddArgs{Stream: key, ID: fmt.Sprintf("%d-0", base), Values: map[string]any{"v": 1, "d": `{"name":"cursor"}`}}).Err(); err != nil {
		t.Fatal(err)
	}
}

// A cursor older than the oldest entry the log still holds names an entry that was trimmed (or lost with Redis): what
// came between it and the oldest is unknowable, so the replay is a gap even though what remains is below the cap.
func TestSSEReplay_ACursorOlderThanTheOldestRetainedEntryIsAGap(t *testing.T) {
	_, client, b := newSSE(t, "a")
	ctx := context.Background()
	key := SSELogKey.Key("u1")
	base := time.Now().UnixMilli()
	for i := range 5 {
		if err := client.XAdd(ctx, &redis.XAddArgs{Stream: key, ID: fmt.Sprintf("%d-%d", base, i+1), Values: map[string]any{"v": 1, "d": `{"name":"e"}`}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.Replay(ctx, "u1", fmt.Sprintf("%d-0", base-500))
	var gap *ReplayGap
	if !errors.As(err, &gap) || got != nil || gap.TailID != fmt.Sprintf("%d-5", base) {
		t.Fatalf("Replay of a cursor older than the log's first entry = %v, %v, want a gap ending at the newest entry", got, err)
	}
}

// A log that is gone although the cursor is inside the window was lost with Redis (a restart without persistence): the
// frames since the cursor are gone with it.
func TestSSEReplay_AnEmptyLogWithinTheWindowIsAGap(t *testing.T) {
	_, _, b := newSSE(t, "a")
	_, err := b.Replay(context.Background(), "u1", fmt.Sprintf("%d-3", time.Now().Add(-time.Minute).UnixMilli()))
	var gap *ReplayGap
	if !errors.As(err, &gap) {
		t.Fatalf("Replay on an empty log = %v, want a gap", err)
	}
}
