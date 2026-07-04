package websocket

import (
	"encoding/json"
	"sync"
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
}

func newFakeBackplane() *fakeBackplane {
	return &fakeBackplane{
		published: map[string][][]byte{},
		presence:  map[string]map[string][]byte{},
	}
}

func (f *fakeBackplane) publish(flowID string, envJSON []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published[flowID] = append(f.published[flowID], envJSON)
}

func (f *fakeBackplane) writePresence(flowID, member string, record []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.presence[flowID] == nil {
		f.presence[flowID] = map[string][]byte{}
	}
	f.presence[flowID][member] = record
}

func (f *fakeBackplane) removePresence(flowID, member string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.presence[flowID], member)
}

func (f *fakeBackplane) listPresence(flowID string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]byte
	for _, rec := range f.presence[flowID] {
		out = append(out, rec)
	}
	return out
}

func (f *fakeBackplane) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// hubWithFake builds a Hub wired to a fake backplane (as if multi-replica).
func hubWithFake() (*Hub, *fakeBackplane) {
	h := NewHub()
	fb := newFakeBackplane()
	h.backplane = fb
	h.replicaID = "test-replica"
	return h, fb
}

func TestBroadcast_PublishesToBackplane(t *testing.T) {
	h, fb := hubWithFake()
	env := Envelope{Type: EventBlockUpdate, FlowID: "flow-1", UserID: "u1", Timestamp: time.Now()}
	h.Broadcast("flow-1", "u1", env, nil)

	fb.mu.Lock()
	defer fb.mu.Unlock()
	if got := len(fb.published["flow-1"]); got != 1 {
		t.Fatalf("expected 1 published envelope, got %d", got)
	}
	var decoded Envelope
	if err := json.Unmarshal(fb.published["flow-1"][0], &decoded); err != nil {
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
	fb.writePresence("flow-1", "conn-remote", rec)

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
	// Drain the presence.join the joiner receives from its own Join.
	for len(c.send) > 0 {
		<-c.send
	}

	env := Envelope{Type: EventCursorMove, FlowID: "flow-1", UserID: "remote", Timestamp: time.Now()}
	envJSON, _ := json.Marshal(env)
	h.deliverRemote("flow-1", envJSON)

	select {
	case got := <-c.send:
		if got.Type != EventCursorMove || got.UserID != "remote" {
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
