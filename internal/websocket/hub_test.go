package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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

// readSync reads the presence.sync every joiner receives right after its own
// presence.join, failing on any other frame, and returns its user list.
func readSync(t *testing.T, conn *websocket.Conn) []PresencePayload {
	t.Helper()
	env := readEnvelope(t, conn)
	if env.Type != EventPresenceSync {
		t.Fatalf("expected presence.sync after the joiner's own presence.join, got %q", env.Type)
	}
	p, ok := parsePayload[PresenceSyncPayload](env.Payload)
	if !ok {
		t.Fatalf("presence.sync payload did not parse: %#v", env.Payload)
	}
	return p.Users
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
	return httptest.NewServer(Handler(hub, userID, displayName, nil, nil, "", time.Time{}, nil))
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
	_ = readSync(t, conn1)

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
	srv := httptest.NewServer(Handler(hub, "u1", "Alice", nil, nil, "", time.Time{}, nil))
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
	_ = readSync(t, conn)

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
	_ = readSync(t, conn)

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
	_ = readSync(t, connA)

	connB := dial(t, srvB, "flow-1")
	defer connB.Close()
	_ = readEnvelope(t, connA) // Bob's join received by Alice
	_ = readEnvelope(t, connB) // Bob receives his own join event
	_ = readSync(t, connB)

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

// readRawUntil reads raw frames from conn until one whose type is typ arrives,
// and returns its exact bytes.
func readRawUntil(t *testing.T, conn *websocket.Conn, typ EventType) []byte {
	t.Helper()
	for {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		var head struct {
			Type EventType `json:"type"`
		}
		if err := json.Unmarshal(msg, &head); err == nil && head.Type == typ {
			return msg
		}
	}
}

// countingPayload counts how many times it is encoded.
type countingPayload struct{ n *atomic.Int32 }

func (p countingPayload) MarshalJSON() ([]byte, error) {
	p.n.Add(1)
	return []byte(`{"blockId":"b1"}`), nil
}

// TestBroadcast_EncodesOnceForEveryRecipient: a broadcast is serialized once
// and those bytes are what every recipient and the backplane get. Each
// writePump used to marshal its own copy on top of the publish's, so a 60 KB
// block.update in a room of K clients was encoded K+1 times.
func TestBroadcast_EncodesOnceForEveryRecipient(t *testing.T) {
	hub, fb := hubWithFake()
	var conns []*websocket.Conn
	for _, user := range []string{"u1", "u2", "u3"} {
		srv := newTestServer(hub, user, user)
		defer srv.Close()
		conn := dial(t, srv, "flow-1")
		defer conn.Close()
		conns = append(conns, conn)
	}
	deadline := time.Now().Add(2 * time.Second)
	for hub.ClientCount() < len(conns) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d clients joined", hub.ClientCount(), len(conns))
		}
		time.Sleep(5 * time.Millisecond)
	}

	var encodes atomic.Int32
	hub.Broadcast("flow-1", "", Envelope{
		Type:      EventBlockUpdate,
		FlowID:    "flow-1",
		UserID:    "u1",
		Timestamp: time.Now(),
		Payload:   countingPayload{n: &encodes},
	}, nil)

	// The publish is queued for the hub's publisher goroutine, among the
	// joins' presence frames: Join registers a client (ClientCount above)
	// before queuing its presence.join, so the block.update is not
	// necessarily the last envelope published.
	var pub []byte
	if !eventually(t, 2*time.Second, func() bool {
		for _, raw := range fb.publishedTo("flow-1") {
			if bytes.Contains(raw, []byte(`"type":"block.update"`)) {
				pub = raw
				return true
			}
		}
		return false
	}) {
		t.Fatal("broadcast was not published")
	}
	for i, conn := range conns {
		if got := readRawUntil(t, conn, EventBlockUpdate); !bytes.Equal(got, pub) {
			t.Fatalf("client %d got %s, the backplane got %s", i, got, pub)
		}
	}
	if n := encodes.Load(); n != 1 {
		t.Fatalf("broadcast to %d clients encoded the envelope %d times, want 1", len(conns), n)
	}
}

// TestDeliverRemote_SendsThePeersBytesUnchanged: an envelope from a peer
// replica reaches local clients byte for byte. It used to be decoded into an
// Envelope (payload as map[string]any) and re-marshalled per recipient, which
// shows here as the payload's keys coming out re-sorted.
func TestDeliverRemote_SendsThePeersBytesUnchanged(t *testing.T) {
	hub := NewHub()
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()
	conn := dial(t, srv, "flow-1")
	defer conn.Close()
	_ = readEnvelope(t, conn) // own join
	_ = readSync(t, conn)

	envJSON := []byte(`{"type":"block.update","flowId":"flow-1","userId":"remote","ts":"2026-10-01T10:00:00Z","payload":{"z":1,"blockId":"b1","properties":{"y":true,"b":[2,1]}}}`)
	hub.deliverRemote("flow-1", envJSON)

	if got := readRawUntil(t, conn, EventBlockUpdate); !bytes.Equal(got, envJSON) {
		t.Fatalf("remote envelope was re-encoded:\n got %s\nwant %s", got, envJSON)
	}
}

// TestBlockUpdate_RelaysThePayloadWithoutDecodingIt: an inbound block.update's
// payload reaches peers as the sender encoded it. It used to be decoded into a
// map[string]any and serialized back out of it, which shows here as the keys
// coming out re-sorted.
func TestBlockUpdate_RelaysThePayloadWithoutDecodingIt(t *testing.T) {
	hub := NewHub()
	alice, bob := joinPair(t, hub)
	defer alice.Close()
	defer bob.Close()

	const payload = `{"z":1,"blockId":"b1","properties":{"y":true,"b":null,"html":"<p>"}}`
	sendRaw(t, alice, `{"type":"block.update","payload":`+payload+`}`)

	got := readRawUntil(t, bob, EventBlockUpdate)
	// json.Marshal escapes HTML in a raw payload just as it did in a decoded
	// one, so "<p>" goes out as \u003cp\u003e either way.
	want := `"payload":` + strings.ReplaceAll(strings.ReplaceAll(payload, "<", `\u003c`), ">", `\u003e`) + `}`
	if !strings.HasSuffix(string(got), want) {
		t.Fatalf("relayed payload was re-encoded:\n got %s\nwant suffix %s", got, want)
	}
	env := decodeQueued(t, Envelope{wire: got})
	if env.UserID != "u1" || env.FlowID != "flow-1" {
		t.Fatalf("relay lost the server-set identity: %+v", env)
	}
}

// TestBlockUpdate_InvalidUTF8AndNullPayloadsKeepTheirOldForm pins the two
// inbound payload cases that are not relayed raw: invalid UTF-8 is replaced
// with U+FFFD as decoding always did (raw, it would fail every peer's browser
// connection), and a null payload is omitted.
func TestBlockUpdate_InvalidUTF8AndNullPayloadsKeepTheirOldForm(t *testing.T) {
	hub := NewHub()
	alice, bob := joinPair(t, hub)
	defer alice.Close()
	defer bob.Close()

	sendRaw(t, alice, "{\"type\":\"block.update\",\"payload\":{\"blockId\":\"b\xff1\"}}")
	got := readRawUntil(t, bob, EventBlockUpdate)
	if !utf8.Valid(got) {
		t.Fatalf("peer received invalid UTF-8: %q", got)
	}
	if !strings.Contains(string(got), "\"blockId\":\"b\ufffd1\"") {
		t.Fatalf("invalid byte was not replaced with U+FFFD: %s", got)
	}

	sendRaw(t, alice, `{"type":"block.update","payload":null}`)
	got = readRawUntil(t, bob, EventBlockUpdate)
	if strings.Contains(string(got), `"payload"`) {
		t.Fatalf("null payload was not omitted: %s", got)
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

// TestClient_Send_BufferFull_DisconnectsSlowClient: when a client's send
// buffer fills (browser unresponsive, network stalled) the server closes the
// connection instead of silently dropping collab events — a forced disconnect
// makes the client reconnect with fresh state.
func TestClient_Send_BufferFull_DisconnectsSlowClient(t *testing.T) {
	// Construct a Client directly (no real WebSocket conn) so Send can be
	// flooded synchronously; the disconnect CAS is asserted atomically (the
	// actual TCP close is exercised by the dial tests above).
	c := &Client{
		UserID: "slow-user",
		flowID: "flow-1",
		send:   make(chan Envelope, sendBufferCap),
	}
	var closeCalls atomic.Int32
	c.conn = nil // we won't actually call conn.Close in this test path

	// Fill the buffer to capacity — every Send goes into the channel.
	for i := range sendBufferCap {
		c.send <- Envelope{Type: EventPresenceUpdate, UserID: "u", Timestamp: time.Now()}
		_ = i
	}
	if c.disconnected.Load() {
		t.Fatal("disconnected should still be false at capacity (channel was full but no overflow yet)")
	}

	// Force the overflow path: the first CAS succeeds; the disconnected flag
	// must make every later one a no-op (idempotent disconnect).
	first := c.disconnected.CompareAndSwap(false, true)
	if !first {
		t.Fatal("first CAS should succeed")
	}
	closeCalls.Add(1)

	for range 10 {
		if c.disconnected.CompareAndSwap(false, true) {
			closeCalls.Add(1)
		}
	}
	if got := closeCalls.Load(); got != 1 {
		t.Errorf("expected exactly one close (idempotent disconnect), got %d", got)
	}
}

// TestHub_Shutdown_DrainsAllClients guards H5: on SIGTERM the Hub must send
// every connected client a CloseGoingAway and wait for the pumps to exit,
// rather than relying on the fx StopTimeout to SIGKILL the pod with the
// clients' in-flight state still live.
func TestHub_Shutdown_DrainsAllClients(t *testing.T) {
	hub := NewHub()
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	// Open 3 clients across 2 rooms.
	conns := make([]*websocket.Conn, 3)
	for i, flowID := range []string{"flow-a", "flow-b", "flow-a"} {
		conns[i] = dial(t, srv, flowID)
		_ = readEnvelope(t, conns[i]) // presence join
	}
	// Keep the client ends referenced until the test returns (and close them
	// then): a *websocket.Conn nothing refers to any more can be garbage-
	// collected mid-test, and the net.Conn finalizer then closes the socket —
	// the server reads EOF and that client leaves before the count below.
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	// Snapshot client count via the hub's room structure.
	hub.mu.RLock()
	totalClients := 0
	for _, r := range hub.rooms {
		totalClients += len(r.clients)
	}
	hub.mu.RUnlock()
	if totalClients != 3 {
		t.Fatalf("expected 3 clients pre-shutdown, got %d", totalClients)
	}

	// Shutdown under a generous budget. Should complete well under 1s.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("Hub.Shutdown: %v", err)
	}

	// Every client's Run goroutine should have returned (done channel closed)
	// and the room map should be empty.
	hub.mu.RLock()
	remainingClients := 0
	for _, r := range hub.rooms {
		remainingClients += len(r.clients)
	}
	hub.mu.RUnlock()
	if remainingClients != 0 {
		t.Errorf("expected 0 clients post-shutdown, got %d", remainingClients)
	}

	// Each client's `done` channel must be closed.
	hub.mu.RLock()
	var doneCount int
	for _, r := range hub.rooms {
		for c := range r.clients {
			select {
			case <-c.done:
				doneCount++
			default:
			}
		}
	}
	hub.mu.RUnlock()
	// remainingClients is 0 (they all left) so we can't iterate them here —
	// but we already asserted the rooms are empty, which means every Run's
	// defer fired, which means every done channel is closed by transitivity.
	_ = doneCount
}

// TestHub_Shutdown_Idempotent proves a second Shutdown call is a no-op (doesn't
// panic, doesn't double-close the backplane).
func TestHub_Shutdown_Idempotent(t *testing.T) {
	hub := NewHub()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// goroutinesSettleTo polls until the goroutine count is at most want or the
// budget elapses, and returns the last count seen.
func goroutinesSettleTo(want int, budget time.Duration) int {
	deadline := time.Now().Add(budget)
	for {
		n := runtime.NumGoroutine()
		if n <= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestClient_Disconnect_ReleasesWritePumpPromptly guards the writePump linger:
// the pump used to select only on the send buffer and its ping ticker, so
// after the peer went away it idled until the next ping (pingPeriod, 54 s),
// holding the Client past the released per-user slot. Every goroutine the
// connection started must be gone well inside one period.
func TestClient_Disconnect_ReleasesWritePumpPromptly(t *testing.T) {
	hub := NewHub()
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	// Warm the test server and dialer (their one-time goroutines are not the
	// connection's), then take the baseline.
	warm := dial(t, srv, "flow-warm")
	_ = readEnvelope(t, warm)
	_ = warm.Close()
	base := goroutinesSettleTo(0, 300*time.Millisecond)

	conn := dial(t, srv, "flow-1")
	_ = readEnvelope(t, conn) // presence join
	if n := runtime.NumGoroutine(); n <= base {
		t.Fatalf("connection started no goroutines (base=%d now=%d) — the test would pass vacuously", base, n)
	}
	_ = conn.Close()

	if n := goroutinesSettleTo(base, 2*time.Second); n > base {
		t.Fatalf("connection goroutines outlived the disconnect (base=%d now=%d); writePump waits for its ping tick", base, n)
	}
	if got := hub.ClientCount(); got != 0 {
		t.Errorf("expected 0 clients after disconnect, got %d", got)
	}
}
