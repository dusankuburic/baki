package websocket

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStress_ConcurrentJoin_50Clients(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	roomID := "stress-room-concurrent-join"

	var wg sync.WaitGroup

	type result struct {
		conn *websocket.Conn
		srv  interface{ Close() }
	}

	var results []result
	var mu sync.Mutex

	for i := range 50 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			userID := "user-concurrent-" + toString(idx)
			srv := newTestServer(hub, userID, "User")
			conn := dial(t, srv, roomID)
			mu.Lock()
			results = append(results, result{conn: conn, srv: srv})
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	time.Sleep(200 * time.Millisecond)

	if got := hub.ClientCount(); got != 50 {
		t.Errorf("expected 50 clients, got %d", got)
	}
	if got := hub.RoomCount(); got != 1 {
		t.Errorf("expected 1 room, got %d", got)
	}

	for _, r := range results {
		r.conn.Close()
		r.srv.Close()
	}
}

func TestStress_HighFrequencyBroadcast_100Messages(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	srvA := newTestServer(hub, "sender", "Sender")
	defer srvA.Close()
	srvB := newTestServer(hub, "receiver", "Receiver")
	defer srvB.Close()

	connA := dial(t, srvA, "stress-broadcast-room")
	defer connA.Close()
	connB := dial(t, srvB, "stress-broadcast-room")
	defer connB.Close()

	_ = readEnvelope(t, connA) // sender join
	_ = readEnvelope(t, connA) // receiver join received by sender
	_ = readEnvelope(t, connB) // receiver join

	for i := range 100 {
		sendEnvelope(t, connA, Envelope{
			Type: EventBlockUpdate,
			Payload: BlockPayload{
				BlockID:   "block-stress",
				SubflowID: "sf1",
				Version:   int64(i),
			},
		})
	}

	connB.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := range 100 {
		_, msg, err := connB.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: read error: %v", i, err)
		}
		var env Envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			t.Fatalf("message %d: unmarshal error: %v", i, err)
		}
		if env.Type != EventBlockUpdate {
			t.Errorf("message %d: expected block.update, got %q", i, env.Type)
		}
		block, ok := parsePayload[BlockPayload](env.Payload)
		if !ok {
			t.Fatalf("message %d: failed to parse BlockPayload", i)
		}
		if block.Version != int64(i) {
			t.Errorf("message %d: expected version %d, got %d", i, i, block.Version)
		}
	}
}

func TestStress_RoomIsolation(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	srvA := newTestServer(hub, "user-a", "Alice")
	defer srvA.Close()
	srvB := newTestServer(hub, "user-b", "Bob")
	defer srvB.Close()
	srvC := newTestServer(hub, "user-c", "Charlie")
	defer srvC.Close()

	connA := dial(t, srvA, "room-alpha")
	defer connA.Close()
	connB := dial(t, srvB, "room-beta")
	defer connB.Close()
	connC := dial(t, srvC, "room-alpha")
	defer connC.Close()

	_ = readEnvelope(t, connA) // Alice join
	_ = readEnvelope(t, connB) // Bob join
	_ = readEnvelope(t, connA) // Charlie join (same room as Alice)
	_ = readEnvelope(t, connC) // Charlie's own join

	sendEnvelope(t, connC, Envelope{
		Type:    EventBlockUpdate,
		Payload: BlockPayload{BlockID: "b-isolation", Version: 1},
	})

	connA.SetReadDeadline(time.Now().Add(2 * time.Second))
	env := readEnvelope(t, connA)
	if env.Type != EventBlockUpdate {
		t.Errorf("Alice should receive Charlie's block.update, got %q", env.Type)
	}

	connB.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, _, err := connB.ReadMessage()
	if err == nil {
		t.Error("Bob should NOT receive messages from room-alpha")
	}
}

func toString(n int) string {
	return fmt.Sprintf("%d", n)
}
