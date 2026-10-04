package websocket

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

	// B1.10: poll to the expected count instead of a fixed sleep — under a
	// parallel -race load, registration could exceed 200ms and the count
	// check flaked. 10s ceiling mirrors the other stress deadlines.
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := hub.ClientCount()
		if got == 50 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 50 clients, got %d", got)
		}
		time.Sleep(10 * time.Millisecond)
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

	// B1.10: the join-phase reads use a stress-sized retry deadline — the
	// shared 2s single-shot readEnvelope deadline under parallel -race load
	// produced the flaky "i/o timeout"; retrying transient deadline errors
	// up to 10s keeps the sequencing assertions without loosening them.
	readStressEnvelope := func(conn *websocket.Conn) Envelope {
		t.Helper()
		overall := time.Now().Add(10 * time.Second)
		for {
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err == nil {
				var env Envelope
				if json.Unmarshal(msg, &env) == nil {
					return env
				}
				continue
			}
			// Retry ONLY the read deadline set just above. Retrying on a
			// permanent error re-reads a failed connection, which gorilla
			// answers with `panic: repeated read on failed websocket
			// connection` — so the real error was replaced by a panic naming
			// the harness instead of the cause, on every flake.
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() && time.Now().Before(overall) {
				continue
			}
			t.Fatalf("readStressEnvelope: %v", err)
		}
	}

	// Join one at a time. With both dials issued back to back the receiver
	// could register first; the sender's presence.join then landed in the
	// receiver's stream where the first block.update was expected.
	connA := dial(t, srvA, "stress-broadcast-room")
	defer connA.Close()
	_ = readStressEnvelope(connA) // sender join
	_ = readStressEnvelope(connA) // sender's presence.sync

	connB := dial(t, srvB, "stress-broadcast-room")
	defer connB.Close()
	_ = readStressEnvelope(connB) // receiver join
	_ = readStressEnvelope(connB) // receiver's presence.sync
	_ = readStressEnvelope(connA) // receiver join received by sender

	for i := range 100 {
		sendEnvelope(t, connA, Envelope{
			Type: EventBlockUpdate,
			Payload: BlockPayload{
				BlockID:   "block-stress",
				SubflowID: "sf1",
				Version:   i,
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
		if block.Version != i {
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

	// Join one at a time, so each client's stream is known: its own join
	// and presence.sync, then later joiners to its room. Dialed back to back,
	// Charlie could register before Alice and Alice never got his join.
	connA := dial(t, srvA, "room-alpha")
	defer connA.Close()
	_ = readEnvelope(t, connA) // Alice join
	_ = readSync(t, connA)
	connB := dial(t, srvB, "room-beta")
	defer connB.Close()
	_ = readEnvelope(t, connB) // Bob join
	_ = readSync(t, connB)
	connC := dial(t, srvC, "room-alpha")
	defer connC.Close()
	_ = readEnvelope(t, connC) // Charlie's own join
	_ = readSync(t, connC)
	_ = readEnvelope(t, connA) // Charlie join (same room as Alice)

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
