package websocket

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dial connects to a test WebSocket server and returns the connection.
func dial(t *testing.T, server *httptest.Server, flowID string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(server.URL, "http") + "?flowId=" + flowID
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// readEnvelope reads one JSON message from a WebSocket connection.
func readEnvelope(t *testing.T, conn *websocket.Conn) Envelope {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("readEnvelope: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(msg, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return env
}

// sendEnvelope sends one JSON message over a WebSocket connection.
func sendEnvelope(t *testing.T, conn *websocket.Conn, env Envelope) {
	t.Helper()
	data, _ := json.Marshal(env)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("sendEnvelope: %v", err)
	}
}

// newTestServer creates a WebSocket test server backed by a Hub.
// allowedOrigins is nil in tests, meaning all origins are rejected unless empty.
func newTestServer(hub *Hub, userID, displayName string) *httptest.Server {
	return httptest.NewServer(Handler(hub, userID, displayName, nil))
}

// ---- Hub tests ----

func TestHub_JoinIncreasesClientCount(t *testing.T) {
	hub := NewHub()
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	conn := dial(t, srv, "flow-1")
	defer conn.Close()

	// Consume the join event that the hub broadcasts
	_ = readEnvelope(t, conn)

	// Wait briefly for the join to register
	time.Sleep(50 * time.Millisecond)
	if got := hub.ClientCount(); got != 1 {
		t.Errorf("expected 1 client, got %d", got)
	}
	if got := hub.RoomCount(); got != 1 {
		t.Errorf("expected 1 room, got %d", got)
	}
}

func TestHub_Join_SendsPresenceJoinToExistingClients(t *testing.T) {
	hub := NewHub()
	srv1 := newTestServer(hub, "u1", "Alice")
	defer srv1.Close()
	srv2 := newTestServer(hub, "u2", "Bob")
	defer srv2.Close()

	// Alice joins first
	conn1 := dial(t, srv1, "flow-1")
	defer conn1.Close()
	// Alice receives her own join event (sent by the hub on connect)
	env := readEnvelope(t, conn1)
	if env.Type != EventPresenceJoin {
		t.Errorf("expected presence.join, got %q", env.Type)
	}

	// Bob joins the same flow
	conn2 := dial(t, srv2, "flow-1")
	defer conn2.Close()

	// Alice should now receive Bob's join event
	env = readEnvelope(t, conn1)
	if env.Type != EventPresenceJoin {
		t.Errorf("Alice should receive Bob's join; got %q", env.Type)
	}
	if env.UserID != "u2" {
		t.Errorf("expected UserID u2, got %q", env.UserID)
	}
}

func TestHub_Leave_RoomDeletedWhenEmpty(t *testing.T) {
	hub := NewHub()
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	conn := dial(t, srv, "flow-1")
	_ = readEnvelope(t, conn) // join event
	conn.Close()

	time.Sleep(100 * time.Millisecond)
	if got := hub.RoomCount(); got != 0 {
		t.Errorf("expected 0 rooms after last client leaves, got %d", got)
	}
}

func TestHub_MissingFlowID_Returns400(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(Handler(hub, "u1", "Alice", nil))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") // no ?flowId
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil {
		t.Fatal("expected error for missing flowId")
	}
	if resp != nil && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestClient_Ping_ReceivesPong(t *testing.T) {
	hub := NewHub()
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	conn := dial(t, srv, "flow-1")
	defer conn.Close()
	_ = readEnvelope(t, conn) // presence.join

	sendEnvelope(t, conn, Envelope{Type: EventPing})
	env := readEnvelope(t, conn)
	if env.Type != EventPong {
		t.Errorf("expected pong, got %q", env.Type)
	}
}

func TestClient_UnknownEvent_ReceivesError(t *testing.T) {
	hub := NewHub()
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	conn := dial(t, srv, "flow-1")
	defer conn.Close()
	_ = readEnvelope(t, conn) // presence.join

	sendEnvelope(t, conn, Envelope{Type: "unknown.event"})
	env := readEnvelope(t, conn)
	if env.Type != EventError {
		t.Errorf("expected error event, got %q", env.Type)
	}
}

func TestClient_BlockUpdate_BroadcastToOtherClient(t *testing.T) {
	hub := NewHub()
	srvA := newTestServer(hub, "u1", "Alice")
	defer srvA.Close()
	srvB := newTestServer(hub, "u2", "Bob")
	defer srvB.Close()

	connA := dial(t, srvA, "flow-1")
	defer connA.Close()
	_ = readEnvelope(t, connA) // Alice's own join

	connB := dial(t, srvB, "flow-1")
	defer connB.Close()
	_ = readEnvelope(t, connA) // Bob's join received by Alice
	_ = readEnvelope(t, connB) // Bob receives his own join event

	// Alice sends a block update
	sendEnvelope(t, connA, Envelope{
		Type:    EventBlockUpdate,
		Payload: BlockPayload{BlockID: "b1", SubflowID: "sf1", Version: 1},
	})

	// Bob should receive Alice's block update
	env := readEnvelope(t, connB)
	if env.Type != EventBlockUpdate {
		t.Errorf("Bob expected block.update, got %q", env.Type)
	}
	if env.UserID != "u1" {
		t.Errorf("expected UserID u1, got %q", env.UserID)
	}
}

func TestHub_Presence_ReturnsConnectedUsers(t *testing.T) {
	hub := NewHub()
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	conn := dial(t, srv, "flow-1")
	defer conn.Close()
	_ = readEnvelope(t, conn) // join

	time.Sleep(50 * time.Millisecond)
	presence := hub.Presence("flow-1")
	if len(presence) != 1 {
		t.Errorf("expected 1 presence entry, got %d", len(presence))
	}
	if presence[0].UserID != "u1" {
		t.Errorf("expected u1, got %q", presence[0].UserID)
	}
}
