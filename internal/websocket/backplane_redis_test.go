package websocket

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newMiniRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// eventually polls cond up to d, so async pub/sub propagation isn't flaky.
func eventually(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestPresence_SharedAcrossReplicas: a client on hub A must be visible in the
// presence read on hub B (the #2 guarantee) via the shared Redis hash.
func TestPresence_SharedAcrossReplicas(t *testing.T) {
	client := newMiniRedis(t)
	hubA := NewHubWithRedis(client)
	hubB := NewHubWithRedis(client)
	t.Cleanup(hubA.Close)
	t.Cleanup(hubB.Close)

	// A client joins on replica A (disconnected send avoided via buffered chan).
	c := &Client{UserID: "alice", DisplayName: "Alice", hub: hubA, flowID: "flow-1", connID: "conn-a", send: make(chan Envelope, 8)}
	hubA.Join("flow-1", c)

	// Replica B, which has no local clients, still sees Alice.
	if !eventually(t, time.Second, func() bool {
		p := hubB.Presence("flow-1")
		return len(p) == 1 && p[0].UserID == "alice"
	}) {
		t.Fatalf("replica B did not see replica A's presence: %+v", hubB.Presence("flow-1"))
	}

	// After leaving, B no longer sees Alice.
	hubA.Leave("flow-1", c)
	if !eventually(t, time.Second, func() bool {
		return len(hubB.Presence("flow-1")) == 0
	}) {
		t.Fatalf("replica B still sees presence after leave: %+v", hubB.Presence("flow-1"))
	}
}

// TestBroadcast_PropagatesAcrossReplicas: an envelope broadcast on hub B must
// reach a client connected on hub A, over Redis pub/sub.
func TestBroadcast_PropagatesAcrossReplicas(t *testing.T) {
	client := newMiniRedis(t)
	hubA := NewHubWithRedis(client)
	hubB := NewHubWithRedis(client)
	t.Cleanup(hubA.Close)
	t.Cleanup(hubB.Close)

	c := &Client{UserID: "alice", hub: hubA, flowID: "flow-1", connID: "conn-a", send: make(chan Envelope, 16)}
	hubA.Join("flow-1", c)
	// Give both PSubscribe loops a moment to register their subscription.
	time.Sleep(50 * time.Millisecond)
	for len(c.send) > 0 { // drain the local presence.join
		<-c.send
	}

	env := Envelope{Type: EventBlockUpdate, FlowID: "flow-1", UserID: "bob", Timestamp: time.Now()}
	hubB.Broadcast("flow-1", "bob", env, nil)

	got := eventually(t, 2*time.Second, func() bool {
		return len(c.send) > 0
	})
	if !got {
		t.Fatal("client on replica A never received the broadcast from replica B")
	}
	received := <-c.send
	if received.Type != EventBlockUpdate || received.UserID != "bob" {
		t.Fatalf("unexpected cross-replica envelope: %+v", received)
	}
}

// TestListPresence_PrunesExpired: entries past their exp are filtered out and
// removed, so a crashed replica's presence ages away.
func TestListPresence_PrunesExpired(t *testing.T) {
	client := newMiniRedis(t)
	b := newRedisBackplane(client, "r1", func(string, []byte) {})
	t.Cleanup(b.close)

	live, _ := json.Marshal(presenceRecord{UserID: "live", Exp: time.Now().Add(time.Minute).UnixNano()})
	dead, _ := json.Marshal(presenceRecord{UserID: "dead", Exp: time.Now().Add(-time.Minute).UnixNano()})
	b.writePresence("flow-1", "c-live", live)
	b.writePresence("flow-1", "c-dead", dead)

	out := b.listPresence("flow-1")
	if len(out) != 1 {
		t.Fatalf("expected only the live entry, got %d", len(out))
	}
	var rec presenceRecord
	_ = json.Unmarshal(out[0], &rec)
	if rec.UserID != "live" {
		t.Fatalf("expected live entry, got %q", rec.UserID)
	}
	// The expired member should have been pruned from the hash.
	if n, _ := client.HLen(context.Background(), presenceKeyPrefix+"flow-1").Result(); n != 1 {
		t.Fatalf("expired member not pruned: hash len = %d", n)
	}
}

// TestOriginDedup: a replica must ignore its own broadcast echoed back over
// pub/sub (it already delivered locally).
func TestOriginDedup(t *testing.T) {
	client := newMiniRedis(t)
	delivered := make(chan []byte, 4)
	b := newRedisBackplane(client, "self", func(_ string, envJSON []byte) {
		delivered <- envJSON
	})
	t.Cleanup(b.close)
	time.Sleep(50 * time.Millisecond) // let PSubscribe register

	env, _ := json.Marshal(Envelope{Type: EventCursorMove})
	b.publish("flow-1", env) // origin == "self"

	select {
	case <-delivered:
		t.Fatal("self-origin message should not be delivered back")
	case <-time.After(300 * time.Millisecond):
		// expected: no delivery
	}
}
