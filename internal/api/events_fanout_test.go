package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/redisx"
	"pad-analyzer/internal/testutil/redisreplica"
)

// The SSE fan-out, event ids and subscriber presence (docs/adr/0030-cross-replica-commands-and-sse-replay.md), driven
// through real EventManagers on the redisreplica harness: N replicas, one Redis, a real HTTP stream per client.

// sseFrame is one dispatched SSE event as a client sees it.
type sseFrame struct {
	ID   string
	Name string
	Data string
}

// openSSE opens a stream on srv as user (the test client key reads X-Test-User), optionally resuming from lastID,
// and returns the dispatched frames plus a func that closes the stream.
func openSSE(t *testing.T, srv *httptest.Server, user, lastID string) (<-chan sseFrame, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("X-Test-User", user)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	frames := make(chan sseFrame, 64)
	var once sync.Once
	closeStream := func() { once.Do(func() { cancel(); _ = resp.Body.Close() }) }
	t.Cleanup(closeStream)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var cur sseFrame
		have := false
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if have {
					frames <- cur
				}
				cur, have = sseFrame{}, false
			case strings.HasPrefix(line, "id: "):
				cur.ID, have = strings.TrimPrefix(line, "id: "), true
			case strings.HasPrefix(line, "data: "):
				cur.Data, have = strings.TrimPrefix(line, "data: "), true
				var ev struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal([]byte(cur.Data), &ev)
				cur.Name = ev.Name
			}
		}
	}()
	return frames, closeStream
}

// next waits for the next frame.
func next(t *testing.T, ch <-chan sseFrame, what string) sseFrame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatalf("stream closed while waiting for %s", what)
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// quiet asserts nothing arrives within d.
func quiet(t *testing.T, ch <-chan sseFrame, d time.Duration, what string) {
	t.Helper()
	select {
	case f, ok := <-ch:
		if ok {
			t.Fatalf("%s: unexpected frame %+v", what, f)
		}
	case <-time.After(d):
	}
}

// fanReplica is one simulated replica: an EventManager with the Redis fan-out, its HTTP server and its backplane.
type fanReplica struct {
	*EventManager
	srv     *httptest.Server
	bp      *redisx.SSEBackplane
	rep     *redisreplica.Replica
	cluster *redisreplica.Cluster
	stop    context.CancelFunc
	stopped chan struct{}
}

// subsField is this replica's presence record for user as stored in Redis ("" when absent).
func (r *fanReplica) subsField(user string) string {
	return r.cluster.Redis.HGet(redisx.SSESubsKey.Key(user), r.rep.ID)
}

// stopFanout ends the replica's Redis loops (a clean stop).
func (r *fanReplica) stopFanout() { r.stop(); <-r.stopped }

func newFanReplicas(t *testing.T, n int, rosters ...OrgRoster) (*redisreplica.Cluster, []*fanReplica) {
	t.Helper()
	cluster := redisreplica.New(t, n)
	var out []*fanReplica
	for i, rep := range cluster.Replicas {
		var opts []EventManagerOption
		bp := redisx.NewSSEBackplane(rep.Client, rep.Health, rep.ID)
		opts = append(opts, WithFanout(bp))
		if i < len(rosters) && rosters[i] != nil {
			opts = append(opts, WithOrgRoster(rosters[i]))
		}
		m := NewEventManager(make(chan struct{}), opts...)
		m.SetClientKeyFunc(func(r *http.Request) string { return "user:" + r.Header.Get("X-Test-User") })
		m.fan.presenceEvery = 40 * time.Millisecond
		srv := httptest.NewServer(http.HandlerFunc(m.HandleEvents))
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithCancel(context.Background())
		fr := &fanReplica{EventManager: m, srv: srv, bp: bp, rep: rep, cluster: cluster, stop: cancel, stopped: make(chan struct{})}
		go func() { defer close(fr.stopped); m.RunFanout(ctx) }()
		t.Cleanup(func() { cancel(); <-fr.stopped })
		redisreplica.Eventually(t, 3*time.Second, rep.ID+" to subscribe", bp.Subscribed)
		out = append(out, fr)
	}
	return cluster, out
}

func fanout(direction, outcome string) float64 { return redisx.SSEFanoutCount(direction, outcome) }

// probe subscribes a plain client to the SSE channel and counts what crosses it.
func probe(t *testing.T, c *redisreplica.Cluster) *atomic.Int64 {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: c.Redis.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	ps := client.Subscribe(context.Background(), redisx.SSEEmitChannel.Key())
	t.Cleanup(func() { _ = ps.Close() })
	if _, err := ps.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	go func() {
		for range ps.Channel() {
			n.Add(1)
		}
	}()
	return &n
}

func TestSSEFanout_AnEventReachesAClientOnAnotherReplica(t *testing.T) {
	_, rs := newFanReplicas(t, 2)
	a, b := rs[0], rs[1]
	onB, _ := openSSE(t, b.srv, "u1", "")
	waitUntil(t, "u1 on b", func() bool { return b.HasSubscriber("u1") })

	a.EmitTo("u1", "flow:loaded", map[string]string{"k": "v"})
	f := next(t, onB, "the event on b")
	if f.Name != "flow:loaded" || !strings.Contains(f.Data, `"k":"v"`) {
		t.Fatalf("b received %+v", f)
	}
	if !redisx.ValidStreamID(f.ID) {
		t.Errorf("the remote frame carries id %q, want the log's stream id", f.ID)
	}
	quiet(t, onB, 100*time.Millisecond, "a duplicate on b")

	// The origin's own client gets it once, with the same id, and no echo of the published copy.
	onA, _ := openSSE(t, a.srv, "u1", "")
	waitUntil(t, "u1 on a", func() bool { return a.HasSubscriber("u1") })
	a.EmitTo("u1", "flow:loaded", "second")
	fa, fb := next(t, onA, "the event on a"), next(t, onB, "the event on b")
	if fa.ID == "" || fa.ID != fb.ID {
		t.Errorf("ids differ across replicas: a %q b %q", fa.ID, fb.ID)
	}
	quiet(t, onA, 100*time.Millisecond, "an echo on the origin")
	quiet(t, onB, 100*time.Millisecond, "a duplicate on b")
}

func TestSSEFanout_AUserReceivesOnlyTheirOwnEventsAcrossReplicas(t *testing.T) {
	_, rs := newFanReplicas(t, 2)
	a, b := rs[0], rs[1]
	u1, _ := openSSE(t, b.srv, "u1", "")
	u2, _ := openSSE(t, b.srv, "u2", "")
	waitUntil(t, "both on b", func() bool { return b.HasSubscriber("u1") && b.HasSubscriber("u2") })
	a.EmitTo("u2", "settings:changed", "mine")
	if f := next(t, u2, "u2's event"); !strings.Contains(f.Data, "mine") {
		t.Fatalf("u2 got %+v", f)
	}
	quiet(t, u1, 150*time.Millisecond, "u1 receiving u2's event")
}

func TestSSEFanout_EmitToOrgResolvesTheRosterAtTheOrigin(t *testing.T) {
	var reads atomic.Int32
	roster := func(_ context.Context, org string) ([]string, error) {
		reads.Add(1)
		return []string{"owner", "member", "owner"}, nil
	}
	cluster, rs := newFanReplicas(t, 2, roster, nil) // b has no roster at all
	a, b := rs[0], rs[1]
	owner, _ := openSSE(t, b.srv, "owner", "")
	member, _ := openSSE(t, b.srv, "member", "")
	stranger, _ := openSSE(t, b.srv, "stranger", "")
	waitUntil(t, "three on b", func() bool {
		return b.HasSubscriber("owner") && b.HasSubscriber("member") && b.HasSubscriber("stranger")
	})

	a.EmitToOrg(context.Background(), "org1", "settings:changed", "org-wide")
	fo, fm := next(t, owner, "the owner's event"), next(t, member, "the member's event")
	if fo.ID == "" || fm.ID == "" {
		t.Errorf("org events are logged per member: owner %q member %q", fo.ID, fm.ID)
	}
	quiet(t, stranger, 150*time.Millisecond, "a non-member")
	quiet(t, owner, 100*time.Millisecond, "the owner twice (the roster listed them twice)")
	if reads.Load() != 1 {
		t.Errorf("roster read %d times, want once at the origin", reads.Load())
	}
	for _, u := range []string{"owner", "member"} {
		if n := xlen(t, cluster, u); n != 1 {
			t.Errorf("%s's log holds %d entries, want 1 (each member's copy)", u, n)
		}
	}
	if n := xlen(t, cluster, "stranger"); n != 0 {
		t.Errorf("a non-member has %d log entries", n)
	}
}

func TestSSEFanout_EmitToAllReachesEveryReplicasClients(t *testing.T) {
	cluster, rs := newFanReplicas(t, 2)
	a, b := rs[0], rs[1]
	onA, _ := openSSE(t, a.srv, "u1", "")
	onB, _ := openSSE(t, b.srv, "u2", "")
	waitUntil(t, "both connected", func() bool { return a.HasSubscriber("u1") && b.HasSubscriber("u2") })
	a.EmitToAll("settings:changed", "everyone")
	fa, fb := next(t, onA, "a's client"), next(t, onB, "b's client")
	if fa.ID != "" || fb.ID != "" {
		t.Errorf("an EmitToAll frame has an id (%q, %q): there is no roster to log it for", fa.ID, fb.ID)
	}
	if n := xlen(t, cluster, "u1") + xlen(t, cluster, "u2"); n != 0 {
		t.Errorf("an EmitToAll frame was logged (%d entries)", n)
	}
}

func xlen(t *testing.T, c *redisreplica.Cluster, user string) int {
	t.Helper()
	if !c.Redis.Exists(redisx.SSELogKey.Key(user)) {
		return 0
	}
	entries, err := c.Redis.Stream(redisx.SSELogKey.Key(user))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func chatEvent(typ string) map[string]any {
	return map[string]any{"streamId": "s1", "type": typ, "data": map[string]any{"text": "hi"}}
}

func TestSSEFanout_ChunksProgressAndHeartbeatsCrossOnlyWhenAnotherReplicaHoldsTheUser(t *testing.T) {
	cluster, rs := newFanReplicas(t, 2)
	a, b := rs[0], rs[1]
	crossed := probe(t, cluster)

	// u1 is held by a alone: nothing is published, the local client still gets everything.
	onA, _ := openSSE(t, a.srv, "u1", "")
	waitUntil(t, "u1 on a", func() bool { return a.HasSubscriber("u1") })
	skipped := fanout("publish", "skipped")
	a.EmitTo("u1", "chat:event", chatEvent("chunk"))
	a.EmitTo("u1", "chat:event", chatEvent("heartbeat"))
	a.EmitTo("u1", "analysis:progress", map[string]int{"pct": 1})
	for _, want := range []string{"chunk", "heartbeat"} {
		if f := next(t, onA, want); !strings.Contains(f.Data, want) {
			t.Fatalf("local client got %+v, want a %s", f, want)
		}
	}
	if f := next(t, onA, "progress"); f.Name != "analysis:progress" {
		t.Fatalf("local client got %+v", f)
	}
	time.Sleep(100 * time.Millisecond)
	if crossed.Load() != 0 {
		t.Errorf("%d ephemeral events were published although no other replica holds u1", crossed.Load())
	}
	if fanout("publish", "skipped")-skipped != 3 {
		t.Errorf("skipped moved by %v, want 3", fanout("publish", "skipped")-skipped)
	}
	if xlen(t, cluster, "u1") != 0 {
		t.Error("an ephemeral frame was logged")
	}

	// u2 is held by b: the same events cross, without ids.
	onB, _ := openSSE(t, b.srv, "u2", "")
	redisreplica.Eventually(t, 3*time.Second, "b's presence record", func() bool { return b.subsField("u2") != "" })
	a.EmitTo("u2", "chat:event", chatEvent("chunk"))
	f := next(t, onB, "the chunk on b")
	if f.ID != "" || !strings.Contains(f.Data, `"type":"chunk"`) {
		t.Errorf("remote chunk = %+v, want no id", f)
	}
	a.EmitTo("u2", "analysis:progress", map[string]int{"pct": 2})
	if f := next(t, onB, "progress on b"); f.Name != "analysis:progress" {
		t.Errorf("remote progress = %+v", f)
	}
	if crossed.Load() != 2 {
		t.Errorf("published %d messages for two ephemeral events held remotely, want 2", crossed.Load())
	}
}

func TestSSEFanout_DurableEventsArePublishedAndLoggedWhetherOrNotAnyoneListens(t *testing.T) {
	cluster, rs := newFanReplicas(t, 2)
	a := rs[0]
	crossed := probe(t, cluster)
	a.EmitTo("nobody", "chat:event", chatEvent("tool"))
	redisreplica.Eventually(t, 3*time.Second, "the publish", func() bool { return crossed.Load() == 1 })
	if xlen(t, cluster, "nobody") != 1 {
		t.Error("the durable frame was not logged for a user with no stream: a reconnecting client would miss it")
	}
}

func TestSSEEventIDs_AreTheLogEntryIDsAndIncrease(t *testing.T) {
	cluster, rs := newFanReplicas(t, 1)
	a := rs[0]
	onA, _ := openSSE(t, a.srv, "u1", "")
	waitUntil(t, "u1", func() bool { return a.HasSubscriber("u1") })
	var ids []string
	for i := range 5 {
		a.EmitTo("u1", "chat:event", chatEvent("tool_result"))
		_ = i
		f := next(t, onA, "a durable frame")
		ids = append(ids, f.ID)
	}
	entries, err := cluster.Redis.Stream(redisx.SSELogKey.Key("u1"))
	if err != nil || len(entries) != 5 {
		t.Fatalf("log = %v, %v, want 5 entries", entries, err)
	}
	for i, id := range ids {
		if id != entries[i].ID {
			t.Errorf("frame %d id %q is not the log entry id %q", i, id, entries[i].ID)
		}
		if i > 0 && redisx.CompareStreamIDs(ids[i], ids[i-1]) <= 0 {
			t.Errorf("ids do not increase: %v", ids)
		}
	}
	if ttl := cluster.Redis.TTL(redisx.SSELogKey.Key("u1")); ttl != redisx.SSELogTTL {
		t.Errorf("log TTL = %v, want %v", ttl, redisx.SSELogTTL)
	}
}

func TestSSEEventIDs_AnOversizedFrameIsDeliveredWithoutAnID(t *testing.T) {
	cluster, rs := newFanReplicas(t, 1)
	a := rs[0]
	onA, _ := openSSE(t, a.srv, "u1", "")
	waitUntil(t, "u1", func() bool { return a.HasSubscriber("u1") })
	a.EmitTo("u1", "analysis:complete", strings.Repeat("x", redisx.SSEMaxLoggedBytes+10))
	f := next(t, onA, "the big frame")
	if f.ID != "" || len(f.Data) < redisx.SSEMaxLoggedBytes {
		t.Errorf("big frame = id %q, %d bytes; want delivered whole and without an id", f.ID, len(f.Data))
	}
	if xlen(t, cluster, "u1") != 0 {
		t.Error("an oversized frame was logged")
	}
}

func TestSSEFanout_RedisDownKeepsLocalDeliveryAndRecovers(t *testing.T) {
	cluster, rs := newFanReplicas(t, 2)
	a, b := rs[0], rs[1]
	onA, _ := openSSE(t, a.srv, "u1", "")
	onB, _ := openSSE(t, b.srv, "u2", "")
	waitUntil(t, "both connected", func() bool { return a.HasSubscriber("u1") && b.HasSubscriber("u2") })
	redisreplica.Eventually(t, 3*time.Second, "b's record", func() bool { return b.subsField("u2") != "" })

	cluster.Kill()
	start := time.Now()
	for range 5 { // enough failures to mark Redis degraded, then the skip path
		a.EmitTo("u1", "chat:event", chatEvent("tool"))
		f := next(t, onA, "a local frame while Redis is down")
		if f.ID != "" {
			t.Errorf("a frame got id %q although the log was unreachable", f.ID)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("five emits against a dead Redis took %v: the emit path waits on it", d)
	}
	a.EmitTo("u2", "flow:loaded", "lost")
	quiet(t, onB, 200*time.Millisecond, "a cross-replica event while Redis is down")
	// A subscriber lookup that cannot be answered says "someone may be listening", never "gone".
	if !a.HasSubscriber("ghost") {
		t.Error("HasSubscriber was false while presence was unreadable: the chat watchdog would cancel a live stream")
	}

	// Back: the health ping notices, ids and fan-out resume.
	cluster.Restore(t)
	redisreplica.Eventually(t, 5*time.Second, "health to recover", func() bool {
		_ = a.rep.Health.Ping(context.Background())
		_ = b.rep.Health.Ping(context.Background())
		return !a.rep.Health.Degraded() && !b.rep.Health.Degraded()
	})
	redisreplica.Eventually(t, 8*time.Second, "the subscriptions to return", func() bool { return a.bp.Subscribed() && b.bp.Subscribed() })
	a.EmitTo("u2", "flow:loaded", "after")
	f := next(t, onB, "an event after the outage")
	if f.ID == "" || !strings.Contains(f.Data, "after") {
		t.Errorf("after recovery b got %+v, want the event with an id", f)
	}
}

func TestSSEReplay_AReconnectGetsWhatItMissedFromAnyReplica(t *testing.T) {
	_, rs := newFanReplicas(t, 2)
	a, b := rs[0], rs[1]
	first, closeFirst := openSSE(t, a.srv, "u1", "")
	waitUntil(t, "u1 on a", func() bool { return a.HasSubscriber("u1") })
	a.EmitTo("u1", "flow:loaded", "e1")
	e1 := next(t, first, "e1")
	a.EmitTo("u1", "flow:loaded", "e2")
	e2 := next(t, first, "e2")
	closeFirst()
	waitUntil(t, "u1 to leave a", func() bool { return !a.HasSubscriber("u1") })

	// Events while the client is away land in the log.
	a.EmitTo("u1", "flow:loaded", "e3")
	a.EmitTo("u1", "chat:event", chatEvent("chunk")) // ephemeral: not replayed
	a.EmitTo("u1", "flow:loaded", "e4")

	// It reconnects, to the OTHER replica, with the last id it saw.
	again, _ := openSSE(t, b.srv, "u1", e2.ID)
	r3, r4 := next(t, again, "replayed e3"), next(t, again, "replayed e4")
	if !strings.Contains(r3.Data, "e3") || !strings.Contains(r4.Data, "e4") {
		t.Fatalf("replay = %+v, %+v, want e3 then e4", r3, r4)
	}
	if redisx.CompareStreamIDs(r3.ID, e2.ID) <= 0 || redisx.CompareStreamIDs(r4.ID, r3.ID) <= 0 {
		t.Errorf("replayed ids %s %s do not follow %s", r3.ID, r4.ID, e2.ID)
	}
	quiet(t, again, 100*time.Millisecond, "the chunk or a repeat of e1/e2")
	_ = e1

	// Then live, once.
	a.EmitTo("u1", "flow:loaded", "e5")
	if f := next(t, again, "live e5"); !strings.Contains(f.Data, "e5") {
		t.Fatalf("live frame = %+v", f)
	}
	quiet(t, again, 100*time.Millisecond, "a duplicate of e5")
}

func TestSSEReplay_NothingIsDeliveredTwiceWhenALiveFrameIsAlsoInTheReplay(t *testing.T) {
	_, rs := newFanReplicas(t, 1)
	a := rs[0]
	a.EmitTo("u1", "flow:loaded", "e1")
	a.EmitTo("u1", "flow:loaded", "e2")
	entries, err := a.cluster.Redis.Stream(redisx.SSELogKey.Key("u1"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("log = %v, %v", entries, err)
	}
	// Between registering the stream and replaying, e3 is emitted: it is in the live buffer AND in the replay.
	a.fan.afterRegister = func() { a.EmitTo("u1", "flow:loaded", "e3") }
	again, _ := openSSE(t, a.srv, "u1", entries[0].ID)
	var seen []string
	for range 2 {
		seen = append(seen, next(t, again, "a replayed frame").Data)
	}
	if !strings.Contains(seen[0], "e2") || !strings.Contains(seen[1], "e3") {
		t.Fatalf("replay = %v, want e2 then e3", seen)
	}
	quiet(t, again, 200*time.Millisecond, "e3 a second time from the live buffer")
	a.EmitTo("u1", "flow:loaded", "e4")
	if f := next(t, again, "live e4"); !strings.Contains(f.Data, "e4") {
		t.Fatalf("live = %+v", f)
	}
}

func TestSSEReplay_ABadLastEventIDIsIgnoredAndOnlyOwnLogIsReadable(t *testing.T) {
	cluster, rs := newFanReplicas(t, 1)
	a := rs[0]
	a.EmitTo("u1", "flow:loaded", "private-to-u1")
	entries, _ := cluster.Redis.Stream(redisx.SSELogKey.Key("u1"))

	for _, bad := range []string{"garbage", "1-2-3", " ", "99999999999999999999-0"} {
		s, closeS := openSSE(t, a.srv, "u2", bad)
		waitUntil(t, "u2 connected", func() bool { return a.HasSubscriber("u2") })
		a.EmitTo("u2", "flow:loaded", "live")
		if f := next(t, s, "a live frame after id "+bad); !strings.Contains(f.Data, "live") {
			t.Errorf("after Last-Event-ID %q the first frame is %+v", bad, f)
		}
		closeS()
		waitUntil(t, "u2 gone", func() bool { return !a.HasSubscriber("u2") })
	}
	// u1's own id sent by u2 replays u2's log from then on, never u1's.
	s, _ := openSSE(t, a.srv, "u2", entries[0].ID)
	deadline := time.After(300 * time.Millisecond)
	for open := true; open; {
		select {
		case f, ok := <-s:
			if !ok {
				open = false
			} else if strings.Contains(f.Data, "private-to-u1") {
				t.Fatalf("u2 was replayed u1's frame: %+v", f)
			}
		case <-deadline:
			open = false
		}
	}
}

func TestSSEReplay_WithoutRedisTheHeaderIsIgnoredAndFramesCarryNoID(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	m.SetClientKeyFunc(func(r *http.Request) string { return "user:" + r.Header.Get("X-Test-User") })
	srv := httptest.NewServer(http.HandlerFunc(m.HandleEvents))
	t.Cleanup(srv.Close)
	s, _ := openSSE(t, srv, "u1", "1700000000000-0")
	waitUntil(t, "u1", func() bool { return m.HasSubscriber("u1") })
	m.EmitTo("u1", "flow:loaded", "x")
	if f := next(t, s, "the event"); f.ID != "" {
		t.Errorf("a deployment without Redis wrote id %q: that is not today's wire format", f.ID)
	}
}

func TestSSEReplay_RedisDownOpensTheStreamLive(t *testing.T) {
	cluster, rs := newFanReplicas(t, 1)
	a := rs[0]
	cluster.Kill()
	s, _ := openSSE(t, a.srv, "u1", fmt.Sprintf("%d-0", time.Now().UnixMilli()))
	// The replay cannot be read: the stream opens, and the client is told it may have missed frames (REVIEW_REDIS H2).
	if f := next(t, s, "the gap marker"); f.Name != "replay:gap" || f.ID != "" {
		t.Fatalf("first frame = %+v, want the replay:gap marker without an id", f)
	}
	waitUntil(t, "u1", func() bool { return a.HasSubscriber("u1") })
	a.EmitTo("u1", "flow:loaded", "live")
	if f := next(t, s, "a live frame"); !strings.Contains(f.Data, "live") {
		t.Fatalf("got %+v", f)
	}
}

func TestSSEPresence_HasSubscriberSeesAStreamOnAnotherReplicaAndTheStreamLeavingClearsIt(t *testing.T) {
	_, rs := newFanReplicas(t, 3)
	a, b, c := rs[0], rs[1], rs[2]
	_, closeB := openSSE(t, b.srv, "u1", "")
	redisreplica.Eventually(t, 3*time.Second, "b's record", func() bool { return b.subsField("u1") != "" })
	var rec struct {
		V, N int
		Kind string
		Exp  int64
	}
	if err := json.Unmarshal([]byte(b.subsField("u1")), &rec); err != nil || rec.V != 1 || rec.Kind != "sse.subs" || rec.N != 1 || rec.Exp <= time.Now().UnixMilli() {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	if !a.HasSubscriber("u1") {
		t.Error("a does not see u1's stream on b")
	}
	if a.HasSubscriber("u-never") {
		t.Error("a reports a subscriber for a user nobody holds")
	}

	// A second stream raises the count; both closing removes the record.
	_, closeB2 := openSSE(t, b.srv, "u1", "")
	redisreplica.Eventually(t, 3*time.Second, "the count to reach 2", func() bool { return strings.Contains(b.subsField("u1"), `"n":2`) })
	closeB()
	closeB2()
	redisreplica.Eventually(t, 3*time.Second, "the record to go", func() bool { return b.subsField("u1") == "" })
	if c.HasSubscriber("u1") {
		t.Error("c still sees u1 after its last stream closed")
	}
}

func TestSSEPresence_ALocalStreamNeverAsksRedis(t *testing.T) {
	cluster, rs := newFanReplicas(t, 1)
	a := rs[0]
	openSSE(t, a.srv, "u1", "")
	waitUntil(t, "u1", func() bool { return a.HasSubscriber("u1") })
	cluster.Kill()
	if !a.HasSubscriber("u1") {
		t.Error("a local stream was not enough")
	}
}

func TestSSEPresence_WithoutRedisItIsLocalOnly(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	if m.HasSubscriber("u1") {
		t.Error("a manager without streams or Redis reported a subscriber")
	}
}

func TestSSEPresence_UnreadableMeansSubscribedAndAnAnswerIsCached(t *testing.T) {
	cluster, rs := newFanReplicas(t, 2)
	a := rs[0]
	if a.HasSubscriber("u3") { // nobody anywhere
		t.Fatal("u3 has a subscriber")
	}
	cluster.Kill()
	if a.HasSubscriber("u3") {
		t.Error("the five-second cache was not used: a dead Redis changed a fresh answer")
	}
	if !a.HasSubscriber("u4") {
		t.Error("an unreadable lookup must answer true (decision 3): the watchdog must not cancel a stream it cannot judge")
	}
}

func TestSSEPresence_TheRecordIsRefreshedWhileTheStreamLives(t *testing.T) {
	cluster, rs := newFanReplicas(t, 1)
	a := rs[0]
	openSSE(t, a.srv, "u1", "")
	redisreplica.Eventually(t, 3*time.Second, "the record", func() bool { return a.subsField("u1") != "" })
	// Age the record as if the refresh were overdue.
	cluster.Redis.HSet(redisx.SSESubsKey.Key("u1"), a.rep.ID, `{"v":1,"kind":"sse.subs","n":1,"exp":1}`)
	redisreplica.Eventually(t, 3*time.Second, "the refresh to rewrite it", func() bool { return !strings.Contains(a.subsField("u1"), `"exp":1}`) })
	if ttl := cluster.Redis.TTL(redisx.SSESubsKey.Key("u1")); ttl != redisx.SSESubsTTL {
		t.Errorf("hash TTL = %v, want %v", ttl, redisx.SSESubsTTL)
	}
}

func TestSSEPresence_AStopRemovesTheReplicasRecords(t *testing.T) {
	_, rs := newFanReplicas(t, 2)
	a, b := rs[0], rs[1]
	openSSE(t, a.srv, "u1", "")
	redisreplica.Eventually(t, 3*time.Second, "the record", func() bool { return a.subsField("u1") != "" })
	a.stopFanout()
	if a.subsField("u1") != "" {
		t.Error("a stopped replica left its subscriber record for the 45 s it would take to expire")
	}
	if b.HasSubscriber("u1") {
		t.Error("b still believes a holds u1")
	}
}

// An old replica (no fan-out) beside a new one: it neither publishes nor subscribes, so the two do not see each
// other's events, and each keeps working. That is why the first deploy stays at one replica (ADR 0030).
func TestSSEMixedVersion_AReplicaWithoutTheFanoutIsAnIsland(t *testing.T) {
	_, rs := newFanReplicas(t, 1)
	fresh := rs[0]
	old := NewEventManager(make(chan struct{}))
	old.SetClientKeyFunc(func(r *http.Request) string { return "user:" + r.Header.Get("X-Test-User") })
	oldSrv := httptest.NewServer(http.HandlerFunc(old.HandleEvents))
	t.Cleanup(oldSrv.Close)

	onOld, _ := openSSE(t, oldSrv, "u1", "")
	onFresh, _ := openSSE(t, fresh.srv, "u1", "")
	waitUntil(t, "both", func() bool { return old.HasSubscriber("u1") && fresh.HasSubscriber("u1") })

	fresh.EmitTo("u1", "flow:loaded", "from-new")
	if f := next(t, onFresh, "the new replica's client"); f.ID == "" {
		t.Error("the new replica's own client lost its id")
	}
	quiet(t, onOld, 150*time.Millisecond, "the old replica's client receiving a new replica's event")
	old.EmitTo("u1", "flow:loaded", "from-old")
	if f := next(t, onOld, "the old replica's client"); f.ID != "" {
		t.Errorf("the old replica wrote an id: %+v", f)
	}
	quiet(t, onFresh, 150*time.Millisecond, "the new replica's client receiving an old replica's event")
}

func TestSSEFanout_CORSAllowsLastEventIDForTheStreamRequest(t *testing.T) {
	rt := &Router{AllowedOrigins: []string{"http://localhost:5173"}, security: &SecurityConfig{}}
	h := rt.corsHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	req := httptest.NewRequest(http.MethodOptions, "/api/events", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Headers", "authorization,last-event-id")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(got), "last-event-id") {
		t.Errorf("Access-Control-Allow-Headers = %q: a cross-origin client (the desktop webview) would fail the preflight of a reconnect", got)
	}
}
