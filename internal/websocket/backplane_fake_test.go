package websocket

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackplane is a deterministic in-memory stand-in for the Redis backplane,
// used to unit-test the Hub's fan-out/presence wiring without pub/sub timing.
type fakeBackplane struct {
	mu        sync.Mutex
	published map[string][][]byte          // flowID -> envelope JSONs
	presence  map[string]map[string][]byte // flowID -> member -> record
	closed    bool
	// writeDelay, when set before the hub is used, stalls every
	// writePresence before it stores anything — a slow Redis HSET, widening
	// the window in which a heartbeat is under way.
	writeDelay time.Duration
	// removeDelay, when set before the hub is used, stalls removePresence for
	// the listed members before it deletes anything.
	removeDelay map[string]time.Duration
	// failRemove makes removePresence delete nothing — a Redis HDEL that
	// failed or timed out, which redisBackplane only logs.
	failRemove bool
	// failList makes listPresence return nil, as redisBackplane does when its
	// HGETALL fails.
	failList bool
	// beforeWrite, when set, runs at the start of every writePresence, before
	// anything is stored — a hook to hold one write in flight.
	beforeWrite func()
	// beforeList, when set, runs at the start of every listPresence, before
	// anything is read — a hook to hold one HGETALL in flight.
	beforeList func()
	// beforePublish, when set, runs at the start of every publish — a hook to
	// hold the publisher in flight, as a slow Redis PUBLISH would.
	beforePublish func()
	// writes, lists, publishes and pipelined count the calls — the round
	// trips a Redis backplane would make. pipelined counts
	// writeAndListPresence calls, whose store write also counts in writes.
	writes, lists, publishes, pipelined atomic.Int32
}

func newFakeBackplane() *fakeBackplane {
	return &fakeBackplane{
		published: map[string][][]byte{},
		presence:  map[string]map[string][]byte{},
	}
}

func (f *fakeBackplane) publish(flowID string, envJSON []byte) {
	f.publishes.Add(1)
	if f.beforePublish != nil {
		f.beforePublish()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published[flowID] = append(f.published[flowID], envJSON)
}

// publishedTo returns the envelopes published for flowID so far. The hub
// publishes from its own goroutine, so a test that has just broadcast polls
// this (eventually) rather than reading once.
func (f *fakeBackplane) publishedTo(flowID string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.published[flowID]...)
}

func (f *fakeBackplane) writePresence(flowID string, records map[string][]byte) {
	f.writes.Add(1)
	if f.beforeWrite != nil {
		f.beforeWrite()
	}
	if f.writeDelay > 0 {
		time.Sleep(f.writeDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.presence[flowID] == nil {
		f.presence[flowID] = map[string][]byte{}
	}
	for member, record := range records {
		f.presence[flowID][member] = record
	}
}

// writeOne stores a single record, as a replica's join would.
func (f *fakeBackplane) writeOne(flowID, member string, record []byte) {
	f.writePresence(flowID, map[string][]byte{member: record})
}

func (f *fakeBackplane) removePresence(flowID, member string) {
	if d := f.removeDelay[member]; d > 0 {
		time.Sleep(d)
	}
	if f.failRemove {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.presence[flowID], member)
}

func (f *fakeBackplane) listPresence(flowID string) []presenceRecord {
	f.lists.Add(1)
	if f.beforeList != nil {
		f.beforeList()
	}
	if f.failList {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]presenceRecord, 0, len(f.presence[flowID]))
	for _, raw := range f.presence[flowID] {
		var rec presenceRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// writeAndListPresence stores the records then reads the room back — the
// join's single round trip. Its write runs the same beforeWrite/writeDelay
// hooks, its read the beforeList hook, a real pipeline's commands would.
func (f *fakeBackplane) writeAndListPresence(flowID string, records map[string][]byte) []presenceRecord {
	f.pipelined.Add(1)
	f.writePresence(flowID, records)
	return f.listPresence(flowID)
}

func (f *fakeBackplane) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// decodeQueued returns the envelope a client's writePump sends for a queued
// one: an envelope queued pre-encoded (a broadcast, or a peer replica's
// bytes, which carry no decoded fields) is decoded from those bytes; any
// other is returned as is.
func decodeQueued(t *testing.T, env Envelope) Envelope {
	t.Helper()
	if env.wire == nil {
		return env
	}
	var out Envelope
	if err := json.Unmarshal(env.wire, &out); err != nil {
		t.Fatalf("queued envelope is not valid JSON: %v", err)
	}
	return out
}

// hubWithFake builds a Hub wired to a fake backplane (as if multi-replica).
func hubWithFake() (*Hub, *fakeBackplane) {
	h := NewHub()
	fb := newFakeBackplane()
	h.useBackplane(fb)
	h.replicaID = "test-replica"
	return h, fb
}

func TestBroadcast_PublishesToBackplane(t *testing.T) {
	h, fb := hubWithFake()
	env := Envelope{Type: EventBlockUpdate, FlowID: "flow-1", UserID: "u1", Timestamp: time.Now()}
	h.Broadcast("flow-1", "u1", env, nil)

	if !eventually(t, 2*time.Second, func() bool { return len(fb.publishedTo("flow-1")) == 1 }) {
		t.Fatalf("expected 1 published envelope, got %d", len(fb.publishedTo("flow-1")))
	}
	var decoded Envelope
	if err := json.Unmarshal(fb.publishedTo("flow-1")[0], &decoded); err != nil {
		t.Fatalf("published payload not a valid envelope: %v", err)
	}
	if decoded.Type != EventBlockUpdate {
		t.Errorf("published type = %q, want block.update", decoded.Type)
	}
}

func TestPresence_ReadsFromBackplane(t *testing.T) {
	h, fb := hubWithFake()
	rec, _ := json.Marshal(presenceRecord{
		UserID: "remote-user", DisplayName: "Bob", SelectedBlockID: "b7",
		Exp: time.Now().Add(time.Minute).UnixNano(),
	})
	fb.writeOne("flow-1", "conn-remote", rec)

	got := h.Presence("flow-1")
	if len(got) != 1 || got[0].UserID != "remote-user" || got[0].SelectedBlockID != "b7" {
		t.Fatalf("Presence did not read from backplane: %+v", got)
	}
}

func TestJoinLeave_WritesAndRemovesPresence(t *testing.T) {
	h, fb := hubWithFake()
	c := &Client{UserID: "u1", DisplayName: "Alice", hub: h, flowID: "flow-1", connID: "conn-1", send: make(chan Envelope, 8)}

	h.Join("flow-1", c)
	fb.mu.Lock()
	_, present := fb.presence["flow-1"]["conn-1"]
	fb.mu.Unlock()
	if !present {
		t.Fatal("Join should write presence to the backplane")
	}

	h.Leave("flow-1", c)
	fb.mu.Lock()
	_, stillThere := fb.presence["flow-1"]["conn-1"]
	fb.mu.Unlock()
	if stillThere {
		t.Fatal("Leave should remove presence from the backplane")
	}
}

func TestDeliverRemote_DeliversToLocalClient(t *testing.T) {
	h, _ := hubWithFake()
	c := &Client{UserID: "u1", hub: h, flowID: "flow-1", connID: "conn-1", send: make(chan Envelope, 8)}
	h.Join("flow-1", c)
	// Drain the presence.join and presence.sync the joiner receives from its
	// own Join.
	for len(c.send) > 0 {
		<-c.send
	}

	env := Envelope{Type: EventPresenceUpdate, FlowID: "flow-1", UserID: "remote", Timestamp: time.Now()}
	envJSON, _ := json.Marshal(env)
	h.deliverRemote("flow-1", envJSON)

	select {
	case queued := <-c.send:
		got := decodeQueued(t, queued)
		if got.Type != EventPresenceUpdate || got.UserID != "remote" {
			t.Fatalf("unexpected delivered envelope: %+v", got)
		}
	default:
		t.Fatal("remote envelope was not delivered to the local client")
	}
}

func TestClose_ClosesBackplane(t *testing.T) {
	h, fb := hubWithFake()
	h.Close()
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.closed {
		t.Fatal("Hub.Close should close the backplane")
	}
}

func TestNilBackplane_InMemoryUnchanged(t *testing.T) {
	h := NewHub() // no backplane
	c := &Client{UserID: "u1", hub: h, flowID: "flow-1", connID: "conn-1", send: make(chan Envelope, 8)}
	h.Join("flow-1", c)
	if got := h.Presence("flow-1"); len(got) != 1 || got[0].UserID != "u1" {
		t.Fatalf("in-memory presence broken: %+v", got)
	}
	h.Close() // must not panic with nil backplane
}
