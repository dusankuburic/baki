package websocket

import (
	"encoding/json"
	"testing"
	"time"
)

func writerRecord(sel string) []byte {
	b, _ := json.Marshal(presenceRecord{
		UserID: "u1", ConnID: "c1", SelectedBlockID: sel, SelectedAt: 1,
		Exp: time.Now().Add(time.Minute).UnixNano(),
	})
	return b
}

// The writer coalesces a connection's pending upserts: two enqueues for one
// connection made while an earlier write is in flight drain as one write
// carrying the LAST record.
func TestPresenceWriter_CoalescesPendingUpserts(t *testing.T) {
	fb := newFakeBackplane()
	hook, release := gate(time.Second)
	fb.beforeWrite = hook
	w := newPresenceWriter(fb)
	go w.run()
	t.Cleanup(w.close)

	w.enqueue("flow", map[string][]byte{"c1": writerRecord("b1")})
	if !eventually(t, time.Second, func() bool { return fb.writes.Load() > 0 }) {
		t.Fatal("first write never started")
	}
	w.enqueue("flow", map[string][]byte{"c1": writerRecord("b2")})
	w.enqueue("flow", map[string][]byte{"c1": writerRecord("b3")})
	release()

	deadline := time.Now().Add(2 * time.Second)
	for {
		recs := fb.listPresence("flow")
		if len(recs) == 1 && recs[0].SelectedBlockID == "b3" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("store = %+v, want the coalesced b3", recs)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := fb.writes.Load(); got != 2 {
		t.Errorf("writes = %d, want 2 (in-flight first + one coalesced drain)", got)
	}
}

// cancelPending drops a queued (not yet in-flight) upsert, so a Leave that
// deletes the record synchronously cannot be undone by it draining after.
func TestPresenceWriter_CancelPendingDropsUpsert(t *testing.T) {
	fb := newFakeBackplane()
	hook, release := gate(time.Second)
	fb.beforeWrite = hook
	w := newPresenceWriter(fb)
	go w.run()
	t.Cleanup(w.close)

	// Hold the writer on an unrelated flow, then queue the upsert behind it.
	w.enqueue("other-flow", map[string][]byte{"cx": writerRecord("b0")})
	if !eventually(t, time.Second, func() bool { return fb.writes.Load() > 0 }) {
		t.Fatal("first write never started")
	}
	w.enqueue("flow", map[string][]byte{"c1": writerRecord("b1")})
	w.cancelPending("flow", "c1")
	release()

	// Drain finishes; give a wrongly-queued write time to (wrongly) land.
	if !eventually(t, time.Second, func() bool { return len(fb.listPresence("other-flow")) == 1 }) {
		t.Fatal("held write never drained")
	}
	time.Sleep(50 * time.Millisecond)
	if recs := fb.listPresence("flow"); len(recs) != 0 {
		t.Errorf("cancelled upsert still drained: store = %+v", recs)
	}
	if got := fb.writes.Load(); got != 1 {
		t.Errorf("writes = %d, want 1 (only the held other-flow write)", got)
	}
}

// drain sends discards whatever a bare client has queued.
func drain(c *Client) {
	for len(c.send) > 0 {
		<-c.send
	}
}

// selectionChange is the exact sequence the read pump runs for a selection
// change (handleIncoming's presence.update case, minus payload parsing).
func selectionChange(c *Client, blockID string) {
	c.presenceMu.Lock()
	c.SetSelectedBlockID(blockID)
	c.hub.writePresenceLocked(c)
	c.broadcastPresence(blockID)
	c.presenceMu.Unlock()
}

// With the store write held in flight, a selection change must still
// complete and announce: the write used to run synchronously inside the
// change, on the read pump, under its presenceMu.
func TestSelectionChange_StoreWriteDoesNotBlock(t *testing.T) {
	hub, fb := hubWithFake()
	t.Cleanup(hub.Close)
	hook, release := gate(time.Second)
	fb.beforeWrite = hook
	t.Cleanup(release)
	c := &Client{UserID: "u1", DisplayName: "Alice", hub: hub, flowID: "flow-1", connID: "c1", send: make(chan Envelope, 8)}
	peer := &Client{UserID: "u2", hub: hub, flowID: "flow-1", connID: "c2", send: make(chan Envelope, 8)}
	hub.Join("flow-1", c)
	hub.Join("flow-1", peer)
	drain(c)
	drain(peer)

	done := make(chan struct{})
	go func() {
		defer close(done)
		selectionChange(c, "b9")
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the selection change blocked on the store write")
	}
	select {
	case env := <-peer.send:
		out, ok := env.Payload.(presenceUpdateOut)
		if !ok || out.SelectedBlockID != "b9" {
			t.Fatalf("announcement = %+v, want u1's b9", env)
		}
	case <-time.After(time.Second):
		t.Fatal("no announcement while the store write was held")
	}
	release()
}

// The heartbeat's store round trips run with no client's presenceMu held: a
// selection change completes while the heartbeat is inside its store read,
// and the heartbeat — reading memory after the round trip — announces the
// NEWEST selection, never the stale one after the fresh announcement.
func TestHeartbeat_AnnouncesNewestSelectionWithoutClientLocks(t *testing.T) {
	hub, fb := hubWithFake()
	t.Cleanup(hub.Close)
	c := &Client{UserID: "u1", DisplayName: "Alice", hub: hub, flowID: "flow-1", connID: "c1", send: make(chan Envelope, 8)}
	peer := &Client{UserID: "u2", hub: hub, flowID: "flow-1", connID: "c2", send: make(chan Envelope, 8)}
	hub.Join("flow-1", c)
	hub.Join("flow-1", peer)
	drain(c)
	drain(peer)

	selectionChange(c, "b1")
	drain(peer) // discard the b1 announcement

	hook, releaseList := gate(time.Second)
	fb.beforeList = hook
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.heartbeat([]*Client{c})
	}()
	if !eventually(t, time.Second, func() bool { return fb.lists.Load() > 0 }) {
		t.Fatal("heartbeat never reached its store read")
	}

	changed := make(chan struct{})
	go func() {
		defer close(changed)
		selectionChange(c, "b9")
	}()
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("selection change blocked while the heartbeat held the store read")
	}
	releaseList()
	<-done

	select {
	case env := <-peer.send:
		out, ok := env.Payload.(presenceUpdateOut)
		if !ok || out.SelectedBlockID != "b9" {
			t.Fatalf("heartbeat announced %+v, want the newest selection b9", env)
		}
	case <-time.After(time.Second):
		t.Fatal("the heartbeat announced nothing after its store read")
	}
	// And no stale b1 lands after the fresh b9.
	select {
	case env := <-peer.send:
		if out, ok := env.Payload.(presenceUpdateOut); ok && out.SelectedBlockID == "b1" {
			t.Fatal("a stale b1 announcement landed after the fresh b9")
		}
	case <-time.After(50 * time.Millisecond):
	}
}

// Join writes the joiner's record and reads the room in one pipelined round
// trip, and the joiner's own record is in the snapshot it is sent.
func TestJoin_PresencePipelined(t *testing.T) {
	hub, fb := hubWithFake()
	t.Cleanup(hub.Close)
	peer := &Client{UserID: "u2", hub: hub, flowID: "flow-1", connID: "c2", send: make(chan Envelope, 8)}
	hub.Join("flow-1", peer)
	drain(peer)

	c := &Client{UserID: "u1", DisplayName: "Alice", hub: hub, flowID: "flow-1", connID: "c1", send: make(chan Envelope, 8)}
	hub.Join("flow-1", c)
	if got := fb.pipelined.Load(); got != 2 {
		t.Errorf("pipelined joins = %d, want 2 (one per join)", got)
	}
	// Every room read the join made rode a pipeline (the fake's pipelined
	// method counts its internal read, so the two counters must agree).
	if got, pipelined := fb.lists.Load(), fb.pipelined.Load(); got != pipelined {
		t.Errorf("list calls = %d, pipelined = %d — a join read outside the pipeline", got, pipelined)
	}
	// c.send: own presence.join, then the presence.sync snapshot.
	var sync Envelope
	for i := 0; i < 2; i++ {
		env := <-c.send
		if env.Type == EventPresenceSync {
			sync = env
		}
	}
	users, ok := sync.Payload.(PresenceSyncPayload)
	if !ok {
		t.Fatalf("sync payload = %T, want PresenceSyncPayload", sync.Payload)
	}
	if !hasUser(users.Users, "u1") {
		t.Errorf("snapshot %v lacks the joiner's own record", users.Users)
	}
}
