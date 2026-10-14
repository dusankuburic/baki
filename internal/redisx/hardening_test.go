package redisx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
)

// The fix round of REVIEW_REDIS (M3, M5, L6, L8) and REVIEW_COMPAT (M3) in the redisx package.

func TestBootOrigin_IsTheInstanceAndAFreshSuffixPerCall(t *testing.T) {
	a, b := BootOrigin("pod-7"), BootOrigin("pod-7")
	if a == b {
		t.Fatalf("two boots of the same instance id share the origin %q: a duplicate PAD_INSTANCE_ID would collide", a)
	}
	for _, o := range []string{a, b} {
		if !strings.HasPrefix(o, "pod-7#") || len(o) != len("pod-7#")+8 {
			t.Errorf("origin %q, want pod-7#<8 hex digits>", o)
		}
		if OriginInstance(o) != "pod-7" {
			t.Errorf("OriginInstance(%q) = %q, want the human-facing id back", o, OriginInstance(o))
		}
	}
	if OriginInstance("plain") != "plain" {
		t.Error("an id without a suffix is its own instance")
	}
}

// Two replicas that were given the same PAD_INSTANCE_ID still tell their own events from each other's and keep
// separate presence records, once each has its boot origin (REVIEW_REDIS M3).
func TestSSE_TwoReplicasWithTheSameInstanceIDStayDistinct(t *testing.T) {
	mr, _, a := newSSE(t, BootOrigin("same"))
	b := peerOf(t, mr, BootOrigin("same"))
	if a.Instance() == b.Instance() {
		t.Fatal("the backplanes share an origin")
	}
	gotB, cancelB := runBackplane(t, b)
	defer cancelB()
	_, cancelA := runBackplane(t, a)
	defer cancelA()
	waitUntil(t, "both subscribed", func() bool { return a.Subscribed() && b.Subscribed() })

	a.Publish(SSEMessage{Name: "flow:changed", All: true, Data: []byte(`{"name":"flow:changed"}`)})
	waitUntil(t, "the peer to receive the event", func() bool { return gotB.len() == 1 })

	a.SetLocal(context.Background(), map[string]int{"u1": 1})
	if held, err := b.PeerHolds("u1", 0); err != nil || !held {
		t.Errorf("PeerHolds on the same-id replica = %v, %v, want true: one replica's record must not be taken for its own", held, err)
	}
	b.SetLocal(context.Background(), map[string]int{"u1": 1})
	a.SetLocal(context.Background(), map[string]int{"u1": 0}) // a's last stream closes
	if held, err := b.PeerHolds("u1", 0); err != nil || held {
		t.Errorf("after a's removal b sees a peer = %v, %v, want none: removing a's record must leave b's alone", held, err)
	}
	if n, _, ok := presenceRecord(t, mr, "u1", b.Instance()); !ok || n != 1 {
		t.Errorf("b's own record = %d, %v, want it kept", n, ok)
	}
}

// XADD and EXPIRE (and HSET and EXPIRE) go out in one MULTI/EXEC: a connection cut between them must not leave a
// key without a TTL, which volatile-lru can never evict (REVIEW_REDIS L6).
func TestSSEWrites_PairTheTTLWithTheWriteInOneTransaction(t *testing.T) {
	_, client, b := newSSE(t, "a")
	rec := &cmdRecorder{}
	client.AddHook(rec)

	b.Log(context.Background(), []string{"u1", "u2"}, []byte(`{"name":"e"}`))
	if !rec.sawTx() {
		t.Errorf("Log wrote XADD+EXPIRE without MULTI/EXEC; commands: %v", rec.names())
	}
	rec.reset()
	b.SetLocal(context.Background(), map[string]int{"u1": 1})
	if !rec.sawTx() {
		t.Errorf("SetLocal wrote HSET+EXPIRE without MULTI/EXEC; commands: %v", rec.names())
	}
}

type cmdRecorder struct {
	mu   sync.Mutex
	cmds []string
}

func (r *cmdRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }
func (r *cmdRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.add(cmd.Name())
		return next(ctx, cmd)
	}
}
func (r *cmdRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, c := range cmds {
			r.add(c.Name())
		}
		return next(ctx, cmds)
	}
}
func (r *cmdRecorder) add(n string) { r.mu.Lock(); r.cmds = append(r.cmds, n); r.mu.Unlock() }
func (r *cmdRecorder) reset()       { r.mu.Lock(); r.cmds = nil; r.mu.Unlock() }
func (r *cmdRecorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.cmds...)
}
func (r *cmdRecorder) sawTx() bool {
	multi, exec := false, false
	for _, n := range r.names() {
		multi = multi || n == "multi"
		exec = exec || n == "exec"
	}
	return multi && exec
}

// --- evictions (REVIEW_REDIS M5) ---

func TestParseInfoField(t *testing.T) {
	info := "# Stats\r\ntotal_connections_received:12\r\nevicted_keys:7\r\nexpired_keys:9\r\n"
	if v, ok := infoField(info, "evicted_keys"); !ok || v != "7" {
		t.Errorf("evicted_keys = %q, %v, want 7", v, ok)
	}
	if _, ok := infoField(info, "evicted_keys_nope"); ok {
		t.Error("a field that is not there was found")
	}
	if _, ok := infoField("garbage", "evicted_keys"); ok {
		t.Error("garbage parsed")
	}
}

func TestHealth_CountsEvictionsTheServerReports(t *testing.T) {
	_, client, h := newHealth(t)
	var mu sync.Mutex
	evicted, policy := 5, "volatile-lru"
	h.info = func(_ context.Context, section string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch section {
		case "stats":
			return fmt.Sprintf("# Stats\r\nevicted_keys:%d\r\n", evicted), nil
		case "memory":
			return fmt.Sprintf("# Memory\r\nmaxmemory_policy:%s\r\n", policy), nil
		}
		return "# Server\r\n", nil
	}
	set := func(n int) { mu.Lock(); evicted = n; mu.Unlock() }
	lines, restore := captureWarns(t)
	defer restore()
	ctx := context.Background()
	total := func() float64 { return testutil.ToFloat64(redisEvictedKeys) }

	before := total()
	h.pollServerInfo(ctx) // the first sample is the baseline: evictions before this process looked are not its to count
	if got := total() - before; got != 0 {
		t.Fatalf("first sample counted %v evictions, want none (baseline)", got)
	}
	set(8)
	h.pollServerInfo(ctx)
	if got := total() - before; got != 3 {
		t.Errorf("after 5 -> 8 counted %v, want 3", got)
	}
	if len(lines()) == 0 || !strings.Contains(lines()[len(lines())-1], "evict") {
		t.Errorf("an eviction did not warn the operator: %v", lines())
	}
	set(2) // the server restarted: its counter began again
	h.pollServerInfo(ctx)
	if got := total() - before; got != 5 {
		t.Errorf("after a counter reset counted %v in all, want 5 (3 + the new 2)", got)
	}
	_ = client
	// volatile-lru is the intended policy: no policy warning.
	for _, l := range lines() {
		if strings.Contains(l, "maxmemory-policy") {
			t.Errorf("warned about a volatile-lru server: %q", l)
		}
	}
}

func TestHealth_WarnsOnceWhenTheServerCanEvictAnyKey(t *testing.T) {
	_, _, h := newHealth(t)
	h.info = func(_ context.Context, section string) (string, error) {
		if section == "memory" {
			return "# Memory\r\nmaxmemory_policy:allkeys-lru\r\n", nil
		}
		return "# Stats\r\nevicted_keys:0\r\n", nil
	}
	lines, restore := captureWarns(t)
	defer restore()
	h.pollServerInfo(context.Background())
	h.pollServerInfo(context.Background())
	n := 0
	for _, l := range lines() {
		if strings.Contains(l, "maxmemory-policy") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the allkeys policy warned %d times (%v), want once", n, lines())
	}
}

func TestHealth_AFailingInfoIsHarmless(t *testing.T) {
	_, _, h := newHealth(t)
	calls := 0
	h.info = func(context.Context, string) (string, error) {
		calls++
		return "", errors.New("ERR unknown command 'info'")
	}
	before := testutil.ToFloat64(redisEvictedKeys)
	h.pollServerInfo(context.Background())
	if testutil.ToFloat64(redisEvictedKeys) != before {
		t.Error("an INFO the server refuses changed the counter")
	}
	h.pollServerInfo(context.Background())
	if calls != 1 {
		t.Errorf("a server that refuses INFO was asked %d times, want once", calls)
	}
	if h.Degraded() {
		t.Error("a refused INFO degraded Redis")
	}
}

// --- throttled warnings (REVIEW_COMPAT M3, REVIEW_REDIS L8) ---

type warnLine struct {
	msg string
	kv  []any
}

// captureWarns replaces LimitedWarn's sink and clock; the returned function lists the lines written so far.
func captureWarns(t *testing.T) (lines func() []string, restore func()) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	ResetLimitedWarns()
	prevSink, prevNow := warnSink, warnNow
	warnSink = func(msg string, kv ...any) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, msg+" "+fmt.Sprintln(kv...))
	}
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, got...) },
		func() { warnSink, warnNow = prevSink, prevNow }
}

func TestLimitedWarn_LogsOncePerIntervalWithTheSuppressedCountAndNoAddress(t *testing.T) {
	var mu sync.Mutex
	var lines []warnLine
	now := time.Now()
	prevSink, prevNow := warnSink, warnNow
	warnSink = func(msg string, kv ...any) { mu.Lock(); lines = append(lines, warnLine{msg, kv}); mu.Unlock() }
	warnNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	t.Cleanup(func() { warnSink, warnNow = prevSink, prevNow })
	ResetLimitedWarns()

	refused := errors.New("dial tcp 10.1.2.3:6379: connect: connection refused")
	for range 50 {
		LimitedWarn("test_mechanism", "websocket backplane publish failed", refused)
	}
	LimitedWarn("other_mechanism", "websocket backplane presence write failed", refused)
	mu.Lock()
	if len(lines) != 2 {
		t.Fatalf("%d lines for 51 failures of two mechanisms, want 1 per mechanism: %v", len(lines), lines)
	}
	text := fmt.Sprintln(lines[0].kv...)
	mu.Unlock()
	if strings.Contains(text, "10.1.2.3") || strings.Contains(text, "6379") {
		t.Errorf("the log line carries the server address: %s", text)
	}
	if !strings.Contains(text, "connection refused") {
		t.Errorf("the log line lacks the error class: %s", text)
	}

	mu.Lock()
	now = now.Add(LimitedWarnInterval + time.Second)
	mu.Unlock()
	LimitedWarn("test_mechanism", "websocket backplane publish failed", refused)
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 3 {
		t.Fatalf("%d lines after the interval, want 3", len(lines))
	}
	last := fmt.Sprintln(lines[2].kv...)
	if !strings.Contains(last, "suppressed 49") {
		t.Errorf("the second line does not report the 49 it suppressed: %s", last)
	}
}
