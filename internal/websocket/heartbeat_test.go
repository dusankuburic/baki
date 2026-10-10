package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// gate returns a hook that blocks until release is called (or d elapses), for
// holding a fake backplane call in flight as a slow Redis round trip would.
func gate(d time.Duration) (hook func(), release func()) {
	ch := make(chan struct{})
	hook = func() {
		select {
		case <-ch:
		case <-time.After(d):
		}
	}
	var once bool
	release = func() {
		if !once {
			once = true
			close(ch)
		}
	}
	return hook, release
}

// TestHeartbeat_DoesNotStallTheWriter: the presence heartbeat used to run on
// each connection's writePump, its store round trips (2 s each) blocking
// everything queued on c.send until Send cut the client off as slow. On the
// room's own goroutine, a broadcast during a store stall still reaches the
// socket at once.
func TestHeartbeat_DoesNotStallTheWriter(t *testing.T) {
	hub, fb := hubWithFake()
	hub.heartbeatPeriod = 20 * time.Millisecond
	listHook, releaseList := gate(time.Second)
	publishHook, releasePublish := gate(time.Second)
	// Join reads the room once for the joiner's presence.sync; the stall is
	// for the reads after it, the heartbeat's.
	var listCalls atomic.Int32
	fb.beforeList = func() {
		if listCalls.Add(1) > 1 {
			listHook()
		}
	}
	fb.beforePublish = publishHook
	t.Cleanup(hub.Close)
	t.Cleanup(func() { releaseList(); releasePublish() })
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	t0 := time.Now()
	conn := dial(t, srv, "flow-1")
	defer conn.Close()
	_ = readEnvelope(t, conn) // own presence.join
	_ = readSync(t, conn)
	t.Logf("joined and synced after %v", time.Since(t0))

	// Let a heartbeat start: it is now inside the room read, held for 1 s.
	if !eventually(t, time.Second, func() bool { return fb.lists.Load() > 1 }) { // >1: Join's own snapshot read
		t.Fatal("no heartbeat started")
	}
	t.Logf("heartbeat in its store read after %v", time.Since(t0))
	time.Sleep(10 * time.Millisecond)

	// Alice is alone, so nothing but this broadcast is queued for her (her
	// own user's heartbeat is not sent to her).
	start := time.Now()
	hub.NotifyFlowChanged("flow-1", 7)
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("flow.changed did not reach the socket within 300 ms of the broadcast (%v) — the writer is inside the heartbeat's store round trip", err)
	}
	var env Envelope
	if err := json.Unmarshal(msg, &env); err != nil || env.Type != EventFlowChanged {
		t.Fatalf("got %s, want flow.changed", msg)
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("flow.changed took %v to reach the socket", took)
	}
	t.Logf("flow.changed reached the socket %v after the broadcast", time.Since(start))
}

// flushPresence waits until every presence upsert enqueued so far has reached the store. The writer is
// asynchronous, and a delete is ordered after the upserts enqueued before it, so the delete of a connection that
// does not exist is a barrier; a test that counts store writes or reads the store right after a join, a heartbeat
// or a selection change flushes first (without it the count or the read raced the writer, about one run in five).
func flushPresence(t *testing.T, h *Hub, flowID string) {
	t.Helper()
	if h.presenceW != nil {
		h.presenceW.delete(flowID, "flush-barrier", 2*time.Second)
	}
}

// presenceUpdates drains c's queue and returns the user of every
// presence.update in it, in order.
func presenceUpdates(c *Client) []string {
	var users []string
	for len(c.send) > 0 {
		if env := <-c.send; env.Type == EventPresenceUpdate {
			users = append(users, env.UserID)
		}
	}
	return users
}

// roomOf returns the hub's current room for flowID.
func roomOf(h *Hub, flowID string) *room {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rooms[flowID]
}

func joinRoomOf(t *testing.T, h *Hub, flowID string, users ...string) []*Client {
	t.Helper()
	clients := make([]*Client, 0, len(users))
	for i, u := range users {
		c := &Client{UserID: u, DisplayName: u, hub: h, flowID: flowID, connID: "conn-" + strconv.Itoa(i), send: make(chan Envelope, 256)}
		h.Join(flowID, c)
		clients = append(clients, c)
	}
	for _, c := range clients {
		drainSend(c)
	}
	return clients
}

// TestHeartbeat_OneStoreRoundTripPerRoom: a room of K connections used to make
// K record writes and K room reads (HGETALL, every record decoded K times) per
// ping period, one set per connection. The room heartbeat makes one of each.
func TestHeartbeat_OneStoreRoundTripPerRoom(t *testing.T) {
	const K = 5

	t.Run("one heartbeat", func(t *testing.T) {
		h, fb := hubWithFake()
		t.Cleanup(h.Close)
		clients := joinRoomOf(t, h, "flow-1", "u1", "u2", "u3", "u4", "u5")
		flushPresence(t, h, "flow-1")
		writes, lists := fb.writes.Load(), fb.lists.Load()

		h.heartbeat(h.liveClients(roomOf(h, "flow-1")))
		flushPresence(t, h, "flow-1")

		if n := fb.lists.Load() - lists; n != 1 {
			t.Fatalf("heartbeat of a room of %d read the store %d times, want 1", K, n)
		}
		if n := fb.writes.Load() - writes; n != 1 {
			t.Fatalf("heartbeat of a room of %d wrote the store %d times, want 1 batch", K, n)
		}
		fb.mu.Lock()
		stored := len(fb.presence["flow-1"])
		fb.mu.Unlock()
		if stored != K {
			t.Fatalf("store holds %d records after the heartbeat, want %d", stored, K)
		}
		for _, c := range clients {
			got := presenceUpdates(c)
			if len(got) != K-1 {
				t.Fatalf("%s got %d presence.update frames from one heartbeat, want one per other user (%d)", c.UserID, len(got), K-1)
			}
			for _, u := range got {
				if u == c.UserID {
					t.Fatalf("%s got its own heartbeat", c.UserID)
				}
			}
		}
	})

	t.Run("ticking", func(t *testing.T) {
		h, fb := hubWithFake()
		h.heartbeatPeriod = 10 * time.Millisecond
		t.Cleanup(h.Close)
		clients := joinRoomOf(t, h, "flow-1", "u1", "u2", "u3", "u4", "u5")
		lists := fb.lists.Load()

		// Wait for the room to have been announced a number of times.
		bob := clients[1]
		rounds := 0
		if !eventually(t, 2*time.Second, func() bool {
			rounds += len(updatesFrom(t, bob, "u1"))
			return rounds >= 10
		}) {
			t.Fatalf("Bob saw %d heartbeats of u1 in 2 s at a 10 ms period", rounds)
		}
		h.Close() // stop the ticker before counting
		rounds += len(updatesFrom(t, bob, "u1"))
		if n := int(fb.lists.Load() - lists); n > rounds+1 {
			t.Fatalf("%d heartbeat rounds read the store %d times, want one per round (at most one in flight)", rounds, n)
		}
	})
}

// TestHeartbeat_OneFramePerUserToOtherUsers: the room heartbeat announces each
// USER once, to everyone but that user's own connections — own tabs never
// needed it (a client exempts itself from its stale sweep), and two tabs of
// one user would announce the same selection twice.
func TestHeartbeat_OneFramePerUserToOtherUsers(t *testing.T) {
	h := NewHub()
	t.Cleanup(h.Close)
	clients := joinRoomOf(t, h, "flow-1", "u1", "u1", "u2")
	tab1, tab2, bob := clients[0], clients[1], clients[2]
	tab2.SetSelectedBlockID("b2")

	h.heartbeat(h.liveClients(roomOf(h, "flow-1")))

	if got := updatesFrom(t, bob, "u1"); len(got) != 1 || got[0] != "b2" {
		t.Fatalf("Bob got %q for u1 (two tabs, one on b2), want exactly [b2]", got)
	}
	for name, tab := range map[string]*Client{"tab1": tab1, "tab2": tab2} {
		var ofU1, ofU2 int
		for len(tab.send) > 0 {
			switch env := <-tab.send; env.UserID {
			case "u1":
				ofU1++
			case "u2":
				ofU2++
			}
		}
		if ofU1 != 0 || ofU2 != 1 {
			t.Fatalf("%s got %d frames for its own user and %d for u2, want 0 and 1", name, ofU1, ofU2)
		}
	}
}

// TestHeartbeat_RoomSkipsDisconnectedConnections: a leaving connection is
// neither re-written into the store nor announced, whether it disconnected
// before the room snapshot or after it.
func TestHeartbeat_RoomSkipsDisconnectedConnections(t *testing.T) {
	h, fb := hubWithFake()
	t.Cleanup(h.Close)
	clients := joinRoomOf(t, h, "flow-1", "u1", "u2", "u3")
	fb.mu.Lock()
	delete(fb.presence["flow-1"], clients[0].connID)
	delete(fb.presence["flow-1"], clients[1].connID)
	fb.mu.Unlock()

	clients[0].disconnected.Store(true)
	live := h.liveClients(roomOf(h, "flow-1"))
	if len(live) != 2 {
		t.Fatalf("liveClients lists %d of 3 with one disconnected, want 2", len(live))
	}
	clients[1].disconnected.Store(true) // after the snapshot, before the lock
	h.heartbeat(live)

	fb.mu.Lock()
	_, wrote0 := fb.presence["flow-1"][clients[0].connID]
	_, wrote1 := fb.presence["flow-1"][clients[1].connID]
	fb.mu.Unlock()
	if wrote0 || wrote1 {
		t.Fatalf("a disconnected connection's record was re-written (before snapshot: %v, after: %v)", wrote0, wrote1)
	}
	if got := presenceUpdates(clients[2]); len(got) != 0 {
		t.Fatalf("u3 got presence.update frames for disconnected users %q, want none", got)
	}
}

// TestHeartbeat_StaleRoomTickDoesNotHeartbeatItsSuccessor: a room's
// heartbeat goroutine looked its room up by flow, so a tick that raced the
// Leave emptying the room could run against the room a new join had just
// opened for the same flow, announcing its clients on the old goroutine.
func TestHeartbeat_StaleRoomTickDoesNotHeartbeatItsSuccessor(t *testing.T) {
	h := NewHub()
	t.Cleanup(h.Close)
	first := joinRoomOf(t, h, "flow-1", "u1")[0]
	r1 := roomOf(h, "flow-1")
	h.Leave("flow-1", first)

	successors := joinRoomOf(t, h, "flow-1", "u2", "u3")
	r2 := roomOf(h, "flow-1")
	if r2 == r1 {
		t.Fatal("the flow's room was not replaced")
	}

	h.heartbeat(h.liveClients(r1)) // r1's tick, late
	for _, c := range successors {
		if got := presenceUpdates(c); len(got) != 0 {
			t.Fatalf("%s got presence.update %q from the old room's tick", c.UserID, got)
		}
	}
	h.heartbeat(h.liveClients(r2))
	for _, c := range successors {
		if got := presenceUpdates(c); len(got) != 1 {
			t.Fatalf("%s got %d presence.update from its own room's tick, want 1", c.UserID, len(got))
		}
	}
}

// TestPublisher_CloseHonoursTheContext: Hub.Shutdown runs on a 3 s budget
// (internal/app) and ends in close; a publisher stuck on a slow backplane must not
// hold it past that.
func TestPublisher_CloseHonoursTheContext(t *testing.T) {
	fb := newFakeBackplane()
	hook, release := gate(5 * time.Second)
	fb.beforePublish = hook
	p := newPublisher(fb)
	t.Cleanup(release)
	p.publish("flow-1", []byte("1"))
	p.publish("flow-1", []byte("2"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	p.close(ctx)
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("close waited %v on a stuck publisher, want to give up at the context's deadline", took)
	}
	release()
	// The drain deadline was the context's, now past: the queued item is
	// discarded rather than published.
	if !eventually(t, 2*time.Second, func() bool {
		select {
		case <-p.done:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("sender did not exit after its publish unblocked")
	}
	if n := len(fb.publishedTo("flow-1")); n != 1 {
		t.Fatalf("%d envelopes published after the deadline, want only the one in flight", n)
	}
}

// TestBroadcast_NeverBlocksOnPublish: the backplane publish is a Redis round
// trip with a 2 s timeout, and Broadcast made it inline — readPump, which
// broadcasts every relayed edit, stopped reading while Redis was slow. The
// publish is queued for the hub's publisher goroutine instead, in order.
func TestBroadcast_NeverBlocksOnPublish(t *testing.T) {
	h, fb := hubWithFake()
	hook, release := gate(5 * time.Second)
	fb.beforePublish = hook
	t.Cleanup(h.Close)
	t.Cleanup(release)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for i := range 3 {
			h.Broadcast("flow-1", "", Envelope{Type: EventFlowChanged, FlowID: "flow-1", Payload: FlowChangedPayload{Version: i}}, nil)
		}
	}()
	select {
	case <-returned:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Broadcast blocked on the backplane publish")
	}

	release()
	if !eventually(t, 2*time.Second, func() bool { return len(fb.publishedTo("flow-1")) == 3 }) {
		t.Fatalf("%d of 3 broadcasts reached the backplane after the publish unblocked", len(fb.publishedTo("flow-1")))
	}
	for i, raw := range fb.publishedTo("flow-1") {
		if want := `"version":` + strconv.Itoa(i) + `}`; !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("publish %d is %s, want version %d — the order broadcast was not kept", i, raw, i)
		}
	}
}

// TestPublisher_DropsWhenFull: a publish that does not fit the queue is
// dropped and counted rather than blocking the broadcaster.
func TestPublisher_DropsWhenFull(t *testing.T) {
	fb := newFakeBackplane()
	hook, release := gate(5 * time.Second)
	fb.beforePublish = hook
	p := newPublisher(fb)
	t.Cleanup(func() { p.close(context.Background()) })
	t.Cleanup(release)

	// The sender takes one item and blocks on it; the queue holds
	// publishQueueCap more. Everything past that is dropped.
	const extra = 3
	for i := range publishQueueCap + 1 + extra {
		p.publish("flow-1", []byte(strconv.Itoa(i)))
	}
	dropped := p.dropped.Load()
	if dropped < extra || dropped > extra+1 {
		t.Fatalf("%d publishes dropped with the queue full, want %d (or %d if the sender had not taken its first item yet)", dropped, extra, extra+1)
	}

	release()
	want := publishQueueCap + 1 + extra - int(dropped)
	if !eventually(t, 3*time.Second, func() bool { return len(fb.publishedTo("flow-1")) == want }) {
		t.Fatalf("%d publishes reached the backplane, want %d (queued and in flight)", len(fb.publishedTo("flow-1")), want)
	}
}

// TestHub_Shutdown_ReleasesEveryGoroutine: the room heartbeats and the
// publisher are goroutines the hub starts; Shutdown must end them along with
// the clients' pumps.
func TestHub_Shutdown_ReleasesEveryGoroutine(t *testing.T) {
	hub, _ := hubWithFake()
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	// Warm the server and dialer, then take the baseline with the hub's own
	// goroutines (the publisher) running and no room open.
	warm := dial(t, srv, "flow-warm")
	_ = readEnvelope(t, warm)
	_ = warm.Close()
	if !eventually(t, 2*time.Second, func() bool { return hub.RoomCount() == 0 }) {
		t.Fatal("warm room never closed")
	}
	base := goroutinesSettleTo(0, 300*time.Millisecond)

	conns := make([]*websocket.Conn, 0, 3)
	for _, flowID := range []string{"flow-a", "flow-b", "flow-a"} {
		conn := dial(t, srv, flowID)
		_ = readEnvelope(t, conn)
		conns = append(conns, conn)
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	if n := runtime.NumGoroutine(); n <= base {
		t.Fatalf("connections started no goroutines (base=%d now=%d)", base, n)
	}

	if err := hub.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// The publisher is gone too, so the count ends below the baseline.
	if n := goroutinesSettleTo(base-1, 2*time.Second); n > base-1 {
		t.Fatalf("goroutines after Shutdown: %d, want at most %d (baseline minus the publisher) — a room heartbeat or the publisher outlived it", n, base-1)
	}
	select {
	case <-hub.pub.done:
	default:
		t.Fatal("publisher still running after Shutdown")
	}
}
