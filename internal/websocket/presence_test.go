package websocket

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// frames decodes every frame conn receives onto the returned channel from a
// background reader, until the connection fails or the test ends — needed by
// tests that show a frame did NOT arrive and then keep reading: a gorilla
// read that runs into its deadline poisons every later read.
func frames(t *testing.T, conn *websocket.Conn) <-chan Envelope {
	t.Helper()
	ch := make(chan Envelope, 64)
	done := t.Context().Done()
	go func() {
		defer close(ch)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env Envelope
			if json.Unmarshal(msg, &env) != nil {
				continue
			}
			select {
			case ch <- env:
			case <-done:
				return
			}
		}
	}()
	return ch
}

// waitFrame returns the first frame on ch that match accepts, skipping any
// other frame, or ok=false if none arrives within d.
func waitFrame(ch <-chan Envelope, d time.Duration, match func(Envelope) bool) (Envelope, bool) {
	deadline := time.After(d)
	for {
		select {
		case env, open := <-ch:
			if !open {
				return Envelope{}, false
			}
			if match(env) {
				return env, true
			}
		case <-deadline:
			return Envelope{}, false
		}
	}
}

// readUntil reads frames from conn until one of type typ arrives, failing if
// none does within readEnvelope's per-frame deadline. For setups where another
// replica's broadcast may interleave with the frames a join produces.
func readUntil(t *testing.T, conn *websocket.Conn, typ EventType) Envelope {
	t.Helper()
	for {
		if env := readEnvelope(t, conn); env.Type == typ {
			return env
		}
	}
}

func isLeaveOf(userID string) func(Envelope) bool {
	return func(env Envelope) bool { return env.Type == EventPresenceLeave && env.UserID == userID }
}

// payloadMap decodes an envelope's payload as a raw JSON object, so a test can
// see exactly which keys went over the wire.
func payloadMap(t *testing.T, env Envelope) map[string]any {
	t.Helper()
	m, ok := env.Payload.(map[string]any)
	if !ok {
		t.Fatalf("%s payload is %T, want a JSON object", env.Type, env.Payload)
	}
	return m
}

// sendRaw writes a hand-built frame, for payloads Envelope's typed fields
// cannot express.
func sendRaw(t *testing.T, conn *websocket.Conn, frame string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("sendRaw: %v", err)
	}
}

// joinPair connects Alice (u1) and then Bob (u2) to flow-1 on hub and consumes
// every presence frame the two joins produce, leaving both connections idle.
func joinPair(t *testing.T, hub *Hub) (alice, bob *websocket.Conn) {
	t.Helper()
	srvA := newTestServer(hub, "u1", "Alice")
	t.Cleanup(srvA.Close)
	srvB := newTestServer(hub, "u2", "Bob")
	t.Cleanup(srvB.Close)

	alice = dial(t, srvA, "flow-1")
	t.Cleanup(func() { _ = alice.Close() })
	_ = readEnvelope(t, alice) // own presence.join
	_ = readSync(t, alice)

	bob = dial(t, srvB, "flow-1")
	t.Cleanup(func() { _ = bob.Close() })
	_ = readEnvelope(t, bob) // own presence.join
	_ = readSync(t, bob)
	_ = readEnvelope(t, alice) // Bob's presence.join
	return alice, bob
}

func selectionOf(hub *Hub, flowID, userID string) (string, bool) {
	for _, p := range hub.Presence(flowID) {
		if p.UserID == userID {
			return p.SelectedBlockID, true
		}
	}
	return "", false
}

// TestPresenceUpdate_PeersSeeServerStateNotClientPayload is the regression
// gate for presence spoofing: the client's payload fields (userId,
// displayName, avatarUrl) must never be relayed as the sender wrote them — a
// viewer could rename itself "Admin" or plant an entry under a victim's id.
func TestPresenceUpdate_PeersSeeServerStateNotClientPayload(t *testing.T) {
	hub := NewHub()
	alice, bob := joinPair(t, hub)

	sendRaw(t, alice, `{"type":"presence.update","userId":"victim","payload":{`+
		`"userId":"victim","displayName":"Admin","avatarUrl":"http://x","selectedBlockId":"b1"}}`)

	env := readEnvelope(t, bob)
	if env.Type != EventPresenceUpdate {
		t.Fatalf("Bob expected presence.update, got %q", env.Type)
	}
	if env.UserID != "u1" {
		t.Errorf("envelope userId = %q, want the authenticated sender u1", env.UserID)
	}
	p := payloadMap(t, env)
	if p["userId"] != "u1" {
		t.Errorf("payload userId = %v, want u1 — the sender's spoofed id was relayed", p["userId"])
	}
	if p["displayName"] != "Alice" {
		t.Errorf("payload displayName = %v, want the server-known Alice", p["displayName"])
	}
	if _, has := p["avatarUrl"]; has {
		t.Errorf("payload carries avatarUrl %v — client-supplied fields must not be relayed", p["avatarUrl"])
	}
	if p["selectedBlockId"] != "b1" {
		t.Errorf("payload selectedBlockId = %v, want b1", p["selectedBlockId"])
	}
	if sel, _ := selectionOf(hub, "flow-1", "u1"); sel != "b1" {
		t.Errorf("stored selection = %q, want b1", sel)
	}
}

// TestPresenceUpdate_InvalidPayloadRefusedToSenderOnly: a malformed payload or
// an oversize selectedBlockId is answered with invalid_message to the sender
// and neither stored nor broadcast.
func TestPresenceUpdate_InvalidPayloadRefusedToSenderOnly(t *testing.T) {
	hub := NewHub()
	alice, bob := joinPair(t, hub)

	sendEnvelope(t, alice, Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": "b1"}})
	if env := readEnvelope(t, bob); env.Type != EventPresenceUpdate || payloadMap(t, env)["selectedBlockId"] != "b1" {
		t.Fatalf("baseline selection did not reach Bob: %+v", env)
	}

	invalid := map[string]string{
		"string payload":  `"garbage"`,
		"number payload":  `42`,
		"array payload":   `["b2"]`,
		"non-string id":   `{"selectedBlockId":5}`,
		"id over the cap": `{"selectedBlockId":"` + strings.Repeat("x", maxSelectedBlockIDLen+1) + `"}`,
		"missing payload": ``,
		"null payload":    `null`,
	}
	for name, payload := range invalid {
		frame := `{"type":"presence.update"}`
		if payload != "" {
			frame = `{"type":"presence.update","payload":` + payload + `}`
		}
		t.Logf("sending presence.update with %s", name) // names the case a read timeout below hits
		sendRaw(t, alice, frame)
		env := readEnvelope(t, alice)
		if env.Type != EventError {
			t.Fatalf("%s: sender got %q, want an error envelope", name, env.Type)
		}
		if code := payloadMap(t, env)["code"]; code != "invalid_message" {
			t.Errorf("%s: error code = %v, want invalid_message", name, code)
		}
	}

	// Every refusal above has been answered, so each was fully processed.
	if sel, _ := selectionOf(hub, "flow-1", "u1"); sel != "b1" {
		t.Errorf("a refused presence.update changed the stored selection to %q", sel)
	}

	// readPump handles one connection's frames in order, so had any refused
	// update been broadcast it would reach Bob before this one: his very next
	// frame must be it. An id of exactly the cap is accepted.
	atCap := strings.Repeat("y", maxSelectedBlockIDLen)
	sendEnvelope(t, alice, Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": atCap}})
	env := readEnvelope(t, bob)
	if env.Type != EventPresenceUpdate || payloadMap(t, env)["selectedBlockId"] != atCap {
		t.Fatalf("Bob's next frame after the refused updates is %q %v, want the %d-byte selection",
			env.Type, env.Payload, maxSelectedBlockIDLen)
	}

	// An empty selectedBlockId is a deselect, not an error, and must go out
	// as an explicit "": a client that merges the payload into its entry
	// keeps the old block when the key is simply absent.
	sendEnvelope(t, alice, Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": ""}})
	env = readEnvelope(t, bob)
	if env.Type != EventPresenceUpdate {
		t.Fatalf("deselect produced %q for Bob, want presence.update", env.Type)
	}
	if sel, has := payloadMap(t, env)["selectedBlockId"]; !has || sel != "" {
		t.Errorf("deselect carried selectedBlockId %v (present: %v), want an explicit \"\"", sel, has)
	}
	if sel, _ := selectionOf(hub, "flow-1", "u1"); sel != "" {
		t.Errorf("stored selection after deselect = %q, want empty", sel)
	}
}

// TestJoin_PresenceSyncGoesToJoinerOnly_OneEntryPerUser: a joiner learns who is
// already in the room. presence.join is broadcast once, as each user arrives,
// so a client joining a room in use listed only those who came after it.
func TestJoin_PresenceSyncGoesToJoinerOnly_OneEntryPerUser(t *testing.T) {
	hub := NewHub()
	srv1 := newTestServer(hub, "u1", "Alice")
	defer srv1.Close()
	srv2 := newTestServer(hub, "u2", "Bob")
	defer srv2.Close()

	tab1 := dial(t, srv1, "flow-1")
	defer tab1.Close()
	if env := readEnvelope(t, tab1); env.Type != EventPresenceJoin {
		t.Fatalf("joiner's first frame is %q, want its own presence.join", env.Type)
	}
	if users := readSync(t, tab1); len(users) != 1 || users[0].UserID != "u1" || users[0].DisplayName != "Alice" {
		t.Fatalf("first joiner's sync = %+v, want just itself", users)
	}

	// A second tab of the same user: still one entry for u1.
	tab2 := dial(t, srv1, "flow-1")
	defer tab2.Close()
	_ = readEnvelope(t, tab2) // own presence.join
	if users := readSync(t, tab2); len(users) != 1 || users[0].UserID != "u1" {
		t.Fatalf("second tab's sync = %+v, want u1 exactly once", users)
	}
	if env := readEnvelope(t, tab1); env.Type != EventPresenceJoin {
		t.Fatalf("an existing member got %q for another tab's join, want presence.join only", env.Type)
	}

	// tab1 selects a block; tab2 (the same user's other connection) sees it,
	// which also proves it was stored before Bob joins.
	sendEnvelope(t, tab1, Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": "b1"}})
	if env := readEnvelope(t, tab2); env.Type != EventPresenceUpdate {
		t.Fatalf("tab2 expected tab1's presence.update, got %q", env.Type)
	}

	bob := dial(t, srv2, "flow-1")
	defer bob.Close()
	_ = readEnvelope(t, bob) // own presence.join
	users := readSync(t, bob)
	byID := map[string]PresencePayload{}
	for _, u := range users {
		if _, dup := byID[u.UserID]; dup {
			t.Errorf("user %q listed twice in presence.sync: %+v", u.UserID, users)
		}
		byID[u.UserID] = u
	}
	if len(byID) != 2 {
		t.Fatalf("Bob's sync = %+v, want Alice and Bob", users)
	}
	if a := byID["u1"]; a.DisplayName != "Alice" || a.SelectedBlockID != "b1" {
		t.Errorf("Alice in Bob's sync = %+v, want displayName Alice and selection b1", a)
	}
	if b := byID["u2"]; b.DisplayName != "Bob" {
		t.Errorf("Bob in his own sync = %+v, want displayName Bob", b)
	}

	// The sync is the joiner's alone: existing members get only the join.
	for name, conn := range map[string]*websocket.Conn{"tab1": tab1, "tab2": tab2} {
		if env := readEnvelope(t, conn); env.Type != EventPresenceJoin || env.UserID != "u2" {
			t.Errorf("%s: expected Bob's presence.join, got %q from %q", name, env.Type, env.UserID)
		}
	}
}

// TestDedupePresence_OneEntryPerUserKeepingASelection pins the merge rule
// directly (the room-level test above cannot: which of a user's connections
// the room map yields first is random) — the selection set last wins, not the
// first non-empty one.
func TestDedupePresence_OneEntryPerUserKeepingASelection(t *testing.T) {
	e := func(userID, name, sel string, selectedAt int64) presenceEntry {
		return presenceEntry{PresencePayload{UserID: userID, DisplayName: name, SelectedBlockID: sel}, selectedAt}
	}
	got := dedupePresence([]presenceEntry{
		e("u1", "Alice", "", 0),
		e("u2", "Bob", "b2", 5),
		e("u1", "Alice", "b1", 3),
		e("u1", "Alice", "b9", 7),
		e("u2", "Bob", "", 0),
		e("u3", "Carol", "b3", 2),
		e("u3", "Carol", "", 4),
		e("u4", "Dan", "", 0),
		e("u4", "Dan", "b4", 0),
	})
	want := []PresencePayload{
		{UserID: "u1", DisplayName: "Alice", SelectedBlockID: "b9"}, // the selection set last, whichever tab set it
		{UserID: "u2", DisplayName: "Bob", SelectedBlockID: "b2"},   // a tab that never selected clears nothing
		{UserID: "u3", DisplayName: "Carol"},                        // a later deselect wins over an older selection
		{UserID: "u4", DisplayName: "Dan", SelectedBlockID: "b4"},   // unstamped records: a selection beats none
	}
	if len(got) != len(want) {
		t.Fatalf("dedupePresence = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if out := dedupePresence(nil); out == nil || len(out) != 0 {
		t.Errorf("dedupePresence(nil) = %#v, want an empty non-nil slice (presence.sync must send [], not null)", out)
	}
}

// TestPresenceSync_AcrossReplicas: with the Redis backplane the snapshot spans
// replicas — a user connected to replica A is in the sync a joiner on replica B
// receives.
func TestPresenceSync_AcrossReplicas(t *testing.T) {
	client := newMiniRedis(t)
	hubA := NewHubWithRedis(client)
	hubB := NewHubWithRedis(client)
	t.Cleanup(hubA.Close)
	t.Cleanup(hubB.Close)
	srvA := newTestServer(hubA, "u1", "Alice")
	defer srvA.Close()
	srvB := newTestServer(hubB, "u2", "Bob")
	defer srvB.Close()

	alice := dial(t, srvA, "flow-1")
	defer alice.Close()
	_ = readEnvelope(t, alice) // own presence.join (its record is written first)
	_ = readSync(t, alice)

	bob := dial(t, srvB, "flow-1")
	defer bob.Close()
	_ = readEnvelope(t, bob) // own presence.join
	users := readSync(t, bob)
	names := map[string]string{}
	for _, u := range users {
		names[u.UserID] = u.DisplayName
	}
	if len(users) != 2 || names["u1"] != "Alice" || names["u2"] != "Bob" {
		t.Fatalf("Bob's sync on replica B = %+v, want Alice (replica A) and Bob", users)
	}
}

// TestHeartbeat_PeersReceivePresenceEachPingTick: every ping tick re-announces
// each live connection to the rest of the room — a peer that missed a user's
// join otherwise never listed that user again.
func TestHeartbeat_PeersReceivePresenceEachPingTick(t *testing.T) {
	hub := NewHub()
	hub.heartbeatPeriod = 30 * time.Millisecond
	srvA := newTestServer(hub, "u1", "Alice")
	defer srvA.Close()
	srvB := newTestServer(hub, "u2", "Bob")
	defer srvB.Close()

	alice := dial(t, srvA, "flow-1")
	defer alice.Close()
	bob := dial(t, srvB, "flow-1")
	defer bob.Close()
	bobFrames := frames(t, bob)

	// awaitFromAlice waits for n presence.update frames from Alice that
	// satisfy want, checking that every one is built from server state.
	awaitFromAlice := func(n int, want func(map[string]any) bool) {
		t.Helper()
		fromAlice := func(env Envelope) bool { return env.Type == EventPresenceUpdate && env.UserID == "u1" }
		for seen := 0; seen < n; {
			env, ok := waitFrame(bobFrames, 2*time.Second, fromAlice)
			if !ok {
				t.Fatalf("Bob received %d of %d expected presence.update from Alice — no heartbeat", seen, n)
			}
			p := payloadMap(t, env)
			if p["userId"] != "u1" || p["displayName"] != "Alice" {
				t.Fatalf("heartbeat payload = %v, want server-built u1/Alice", p)
			}
			if want(p) {
				seen++
			}
		}
	}

	// Alice has sent nothing, so any presence.update from her is a heartbeat.
	awaitFromAlice(2, func(map[string]any) bool { return true })

	// Heartbeats carry the stored selection. The selection change itself
	// accounts for one frame carrying b7; the other two can only be heartbeats.
	sendEnvelope(t, alice, Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": "b7"}})
	awaitFromAlice(3, func(p map[string]any) bool { return p["selectedBlockId"] == "b7" })
}

// TestHeartbeat_SkippedOnceDisconnected: a leaving client must not re-announce
// itself — peers would re-add it after its presence.leave.
func TestHeartbeat_SkippedOnceDisconnected(t *testing.T) {
	hub := NewHub()
	c := &Client{UserID: "u1", DisplayName: "Alice", hub: hub, flowID: "flow-1", connID: "c1", send: make(chan Envelope, 8)}
	peer := &Client{UserID: "u2", hub: hub, flowID: "flow-1", connID: "c2", send: make(chan Envelope, 8)}
	hub.Join("flow-1", c)
	hub.Join("flow-1", peer)
	for _, cl := range []*Client{c, peer} {
		for len(cl.send) > 0 {
			<-cl.send
		}
	}

	c.heartbeat()
	select {
	case env := <-peer.send:
		if env.Type != EventPresenceUpdate || env.UserID != "u1" {
			t.Fatalf("heartbeat delivered %+v, want u1's presence.update", env)
		}
	default:
		t.Fatal("a live client's heartbeat reached no peer")
	}
	if len(c.send) != 0 {
		t.Fatalf("heartbeat was echoed to the sender: %+v", <-c.send)
	}

	c.disconnected.Store(true)
	c.heartbeat()
	if len(peer.send) != 0 {
		t.Fatalf("a disconnected client's heartbeat was broadcast: %+v", <-peer.send)
	}
}

// TestLeave_OnlyWhenUsersLastConnectionCloses: presence.leave means the user
// is gone and peers key presence by user — it must not fire on every
// disconnect, or closing one of two tabs removes a user still in the room.
func TestLeave_OnlyWhenUsersLastConnectionCloses(t *testing.T) {
	hub := NewHub()
	srv1 := newTestServer(hub, "u1", "Alice")
	defer srv1.Close()
	srv2 := newTestServer(hub, "u2", "Bob")
	defer srv2.Close()

	tab1 := dial(t, srv1, "flow-1")
	defer tab1.Close()
	_ = readEnvelope(t, tab1)
	_ = readSync(t, tab1)
	tab2 := dial(t, srv1, "flow-1")
	defer tab2.Close()
	_ = readEnvelope(t, tab2)
	_ = readSync(t, tab2)
	bob := dial(t, srv2, "flow-1")
	defer bob.Close()
	_ = readEnvelope(t, bob)
	_ = readSync(t, bob)
	bobFrames := frames(t, bob)

	_ = tab1.Close()
	if !eventually(t, 2*time.Second, func() bool { return hub.ClientCount() == 2 }) {
		t.Fatalf("closed tab never left the room: %d clients", hub.ClientCount())
	}
	if env, got := waitFrame(bobFrames, 300*time.Millisecond, isLeaveOf("u1")); got {
		t.Fatalf("Bob got presence.leave for u1 while u1's other tab is still connected: %+v", env)
	}

	_ = tab2.Close()
	if _, got := waitFrame(bobFrames, 2*time.Second, isLeaveOf("u1")); !got {
		t.Fatal("Bob never got presence.leave after u1's last connection closed")
	}
}

// TestLeave_OnlyWhenUsersLastConnectionClosesAcrossReplicas: the same rule when
// the user's other connection lives on another replica — only the shared
// presence store can tell replica B that u1 is still connected to replica A.
func TestLeave_OnlyWhenUsersLastConnectionClosesAcrossReplicas(t *testing.T) {
	client := newMiniRedis(t)
	hubA := NewHubWithRedis(client)
	hubB := NewHubWithRedis(client)
	t.Cleanup(hubA.Close)
	t.Cleanup(hubB.Close)
	srvA1 := newTestServer(hubA, "u1", "Alice")
	defer srvA1.Close()
	srvB1 := newTestServer(hubB, "u1", "Alice")
	defer srvB1.Close()
	srvA2 := newTestServer(hubA, "u2", "Bob")
	defer srvA2.Close()

	bob := dial(t, srvA2, "flow-1")
	defer bob.Close()
	_ = readEnvelope(t, bob)
	_ = readSync(t, bob)
	bobFrames := frames(t, bob)

	// Prove B → A pub/sub is live before relying on a frame NOT arriving over
	// it: probe until Bob receives a broadcast published on replica B.
	isProbe := func(env Envelope) bool { return env.Type == EventFlowChanged }
	if !eventually(t, 3*time.Second, func() bool {
		hubB.NotifyFlowChanged("flow-1", 1)
		_, got := waitFrame(bobFrames, 25*time.Millisecond, isProbe)
		return got
	}) {
		t.Fatal("replica A never received a broadcast from replica B")
	}

	// The sync is the last thing Join does, so once it is read the
	// connection's presence record is in the shared store. A straggling probe
	// may arrive first, hence readUntil.
	onA := dial(t, srvA1, "flow-1")
	defer onA.Close()
	_ = readUntil(t, onA, EventPresenceSync)
	onB := dial(t, srvB1, "flow-1")
	defer onB.Close()
	_ = readUntil(t, onB, EventPresenceSync)

	_ = onB.Close()
	if !eventually(t, 2*time.Second, func() bool { return hubB.ClientCount() == 0 }) {
		t.Fatalf("closed connection never left replica B: %d clients", hubB.ClientCount())
	}
	if env, got := waitFrame(bobFrames, 500*time.Millisecond, isLeaveOf("u1")); got {
		t.Fatalf("Bob got presence.leave for u1 while u1 is still connected to replica A: %+v", env)
	}

	_ = onA.Close()
	if _, got := waitFrame(bobFrames, 2*time.Second, isLeaveOf("u1")); !got {
		t.Fatal("Bob never got presence.leave after u1's last connection closed")
	}
}

// TestLeave_ConcurrentLeavesOfOneUserAnnounceExactlyOnce pins the order inside
// Leave: the shared record must be removed BEFORE the room check, so a user's
// two connections leaving together cannot both stay silent (each saw the other
// still present — the user then stayed listed on every peer).
func TestLeave_ConcurrentLeavesOfOneUserAnnounceExactlyOnce(t *testing.T) {
	h, fb := hubWithFake()
	fb.removeDelay = map[string]time.Duration{"tab-1": 100 * time.Millisecond}
	tab1 := &Client{UserID: "u1", hub: h, flowID: "flow-1", connID: "tab-1", send: make(chan Envelope, 8)}
	tab2 := &Client{UserID: "u1", hub: h, flowID: "flow-1", connID: "tab-2", send: make(chan Envelope, 8)}
	peer := &Client{UserID: "u2", hub: h, flowID: "flow-1", connID: "peer", send: make(chan Envelope, 16)}
	h.Join("flow-1", tab1)
	h.Join("flow-1", tab2)
	h.Join("flow-1", peer)
	for len(peer.send) > 0 {
		<-peer.send
	}

	// tab1's leave starts first and stalls in the shared store; tab2's
	// leave runs to completion inside that stall.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.Leave("flow-1", tab1) }()
	time.Sleep(10 * time.Millisecond)
	go func() { defer wg.Done(); h.Leave("flow-1", tab2) }()
	wg.Wait()

	leaves := 0
	for len(peer.send) > 0 {
		if env := <-peer.send; env.Type == EventPresenceLeave && env.UserID == "u1" {
			leaves++
		}
	}
	if leaves != 1 {
		t.Fatalf("peer got %d presence.leave for u1 after both of its connections left, want exactly 1", leaves)
	}
}

// TestRun_NothingFromTheWriterLandsAfterLeave guards Run's cleanup order:
// Leave must run after writePump has been stopped and any in-flight heartbeat
// waited out, or that heartbeat re-creates the entry Leave removed (a ghost
// suppressing the real presence.leave) and re-adds the departed user on
// every peer.
func TestRun_NothingFromTheWriterLandsAfterLeave(t *testing.T) {
	hub, fb := hubWithFake()
	fb.writeDelay = 20 * time.Millisecond
	hub.heartbeatPeriod = time.Millisecond
	srv := newTestServer(hub, "u1", "Alice")
	defer srv.Close()

	conn := dial(t, srv, "flow-1")
	_ = readEnvelope(t, conn)
	_ = readSync(t, conn)
	time.Sleep(4 * fb.writeDelay) // let writePump settle into its heartbeats
	_ = conn.Close()

	if !eventually(t, 2*time.Second, func() bool { return hub.ClientCount() == 0 }) {
		t.Fatalf("client never left: %d clients", hub.ClientCount())
	}
	time.Sleep(3 * fb.writeDelay) // any in-flight heartbeat has landed by now

	fb.mu.Lock()
	defer fb.mu.Unlock()
	if n := len(fb.presence["flow-1"]); n != 0 {
		t.Errorf("%d presence record(s) survived the leave — a heartbeat re-wrote it after Leave", n)
	}
	published := fb.published["flow-1"]
	if len(published) == 0 {
		t.Fatal("nothing was published for the room")
	}
	var last Envelope
	if err := json.Unmarshal(published[len(published)-1], &last); err != nil {
		t.Fatalf("last published envelope: %v", err)
	}
	if last.Type != EventPresenceLeave {
		t.Errorf("last envelope published for the room is %q, want presence.leave — the writer broadcast after Leave", last.Type)
	}
}

// drainSend empties a test client's queue.
func drainSend(c *Client) {
	for len(c.send) > 0 {
		<-c.send
	}
}

// updatesFrom returns, in order, the selectedBlockId of every presence.update
// for userID queued on c — the payload as it goes over the wire — and fails
// if one of them has no selectedBlockId key.
func updatesFrom(t *testing.T, c *Client, userID string) []string {
	t.Helper()
	var sels []string
	for len(c.send) > 0 {
		env := <-c.send
		if env.Type != EventPresenceUpdate || env.UserID != userID {
			continue
		}
		raw, err := json.Marshal(env.Payload)
		if err != nil {
			t.Fatalf("marshal presence.update payload: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("presence.update payload %s: %v", raw, err)
		}
		sel, has := p["selectedBlockId"]
		if !has {
			t.Fatalf("presence.update %s has no selectedBlockId key; none selected must go out as an explicit \"\"", raw)
		}
		s, _ := sel.(string)
		sels = append(sels, s)
	}
	return sels
}

// TestHeartbeat_CarriesTheUsersSelectionNotTheConnections: peers key presence
// by user and take an update's selection as the user's whole selection, so a
// heartbeat must carry the USER's selection — per-connection announcements
// made a user's tabs flip each other's selection on every peer each period.
func TestHeartbeat_CarriesTheUsersSelectionNotTheConnections(t *testing.T) {
	newClient := func(h *Hub, userID, connID string) *Client {
		return &Client{UserID: userID, DisplayName: userID, hub: h, flowID: "flow-1", connID: connID, send: make(chan Envelope, 64)}
	}

	t.Run("tabs on one replica", func(t *testing.T) {
		h := NewHub()
		tab1, tab2, bob := newClient(h, "u1", "tab-1"), newClient(h, "u1", "tab-2"), newClient(h, "u2", "bob")
		for _, c := range []*Client{tab1, tab2, bob} {
			h.Join("flow-1", c)
		}
		drainSend(bob)

		tab1.SetSelectedBlockID("b1")
		for range 3 {
			tab1.heartbeat()
			tab2.heartbeat()
		}
		got := updatesFrom(t, bob, "u1")
		if len(got) != 6 {
			t.Fatalf("Bob got %d heartbeats from u1's two tabs, want 6", len(got))
		}
		for i, sel := range got {
			if sel != "b1" {
				t.Fatalf("heartbeat %d told Bob u1 has %q selected, want b1 — the sequence was %q", i+1, sel, got)
			}
		}

		// The selection a user made last is theirs, in whichever tab: the
		// idle tab's own selection change moves it, and a later deselect
		// there clears it although tab1 still holds b1.
		tab2.handleIncoming(Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": "b2"}})
		tab1.heartbeat()
		tab2.handleIncoming(Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": ""}})
		tab1.heartbeat()
		if got := updatesFrom(t, bob, "u1"); len(got) != 4 || got[0] != "b2" || got[1] != "b2" || got[2] != "" || got[3] != "" {
			t.Fatalf("after tab2 selected b2 and then deselected, Bob saw %q, want [b2 b2 \"\" \"\"]", got)
		}
		if sel, _ := selectionOf(h, "flow-1", "u1"); sel != "" {
			t.Errorf("presence.sync would list u1 on %q, the heartbeats say none", sel)
		}
	})

	t.Run("tabs on two replicas", func(t *testing.T) {
		h, fb := hubWithFake()
		// tab1 lives on another replica: here it is only a record in the
		// shared store.
		rec, _ := json.Marshal(presenceRecord{
			UserID: "u1", DisplayName: "u1", SelectedBlockID: "b1", SelectedAt: selectionStamp(),
			ConnID: "tab-1", Exp: time.Now().Add(time.Minute).UnixNano(),
		})
		fb.writeOne("flow-1", "tab-1", rec)
		tab2, bob := newClient(h, "u1", "tab-2"), newClient(h, "u2", "bob")
		h.Join("flow-1", tab2)
		h.Join("flow-1", bob)
		drainSend(bob)

		tab2.heartbeat()
		if got := updatesFrom(t, bob, "u1"); len(got) != 1 || got[0] != "b1" {
			t.Fatalf("u1's idle tab heartbeat told Bob %q, want b1 — the selection u1's tab on the other replica holds", got)
		}
		if sel, _ := selectionOf(h, "flow-1", "u1"); sel != "b1" {
			t.Errorf("presence.sync would list u1 on %q, want b1", sel)
		}
	})
}

// TestLeave_AnnouncedWhenTheLeaversOwnRecordSurvives: when its own record
// delete fails, the leaver's record must not answer "still here" to Leave's
// store check, or no replica announces the user's only leave and peers keep
// them listed until the record expires.
func TestLeave_AnnouncedWhenTheLeaversOwnRecordSurvives(t *testing.T) {
	h, fb := hubWithFake()
	fb.failRemove = true
	alice := &Client{UserID: "u1", hub: h, flowID: "f", connID: "c1", send: make(chan Envelope, 8)}
	bob := &Client{UserID: "u2", hub: h, flowID: "f", connID: "c2", send: make(chan Envelope, 8)}
	h.Join("f", alice)
	h.Join("f", bob)
	drainSend(bob)

	h.Leave("f", alice)
	leaves := 0
	for len(bob.send) > 0 {
		if isLeaveOf("u1")(<-bob.send) {
			leaves++
		}
	}
	if leaves != 1 {
		t.Fatalf("Bob got %d presence.leave for u1 after its only connection left, want 1", leaves)
	}
}

// TestWritePresence_AnOlderHeartbeatCannotOverwriteANewerSelection: the
// heartbeat and a selection change both write the connection's shared record;
// a slow heartbeat that read the old selection must not overwrite the newer
// one the change just stored.
func TestWritePresence_AnOlderHeartbeatCannotOverwriteANewerSelection(t *testing.T) {
	h, fb := hubWithFake()
	c := &Client{UserID: "u1", DisplayName: "Alice", hub: h, flowID: "f", connID: "c1", send: make(chan Envelope, 16)}
	c.SetSelectedBlockID("b1")
	h.Join("f", c)

	// Hold the next record write — the heartbeat's, which has read b1 by
	// then — in flight, as a slow HSET would.
	entered, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	fb.beforeWrite = func() {
		if held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		c.heartbeat()
	}()
	<-entered

	changeDone := make(chan struct{})
	go func() {
		defer close(changeDone)
		c.handleIncoming(Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": "b2"}})
	}()
	// Let the change's write land if nothing holds it back, then let the
	// heartbeat's finish.
	select {
	case <-changeDone:
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-heartbeatDone
	<-changeDone

	// The writes drain asynchronously now (the change's write is enqueued,
	// not awaited): the invariant is the CONVERGED store — the heartbeat's
	// older b1 record must never be its final state. The writer is FIFO, so
	// the b2 enqueued after the heartbeat's b1 lands last.
	if !eventually(t, 2*time.Second, func() bool {
		fb.mu.Lock()
		raw := fb.presence["f"]["c1"]
		fb.mu.Unlock()
		if raw == nil {
			return false
		}
		var rec presenceRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return false
		}
		return rec.SelectedBlockID == "b2"
	}) {
		fb.mu.Lock()
		raw := fb.presence["f"]["c1"]
		fb.mu.Unlock()
		t.Fatalf("stored record %s never converged to b2 — the heartbeat's older write landed last", raw)
	}
	if sel, _ := selectionOf(h, "f", "u1"); sel != "b2" {
		t.Errorf("Presence (what presence.sync sends) lists u1 on %q, want b2", sel)
	}
}

// TestHeartbeat_AnOlderSelectionIsNotAnnouncedLast: the heartbeat's store
// read can straddle a concurrent selection change; the newer selection must
// still be announced last, on every peer and in the store, or peers show the
// old block until the next heartbeat.
func TestHeartbeat_AnOlderSelectionIsNotAnnouncedLast(t *testing.T) {
	newClient := func(h *Hub, userID, connID string) *Client {
		return &Client{UserID: userID, DisplayName: userID, hub: h, flowID: "f", connID: connID, send: make(chan Envelope, 64)}
	}
	// race holds hb's heartbeat inside its store read, runs change, gives
	// it 100 ms to finish if nothing holds it back, then lets the
	// heartbeat's read return.
	race := func(fb *fakeBackplane, hb *Client, change func()) {
		entered, release := make(chan struct{}), make(chan struct{})
		var held atomic.Bool
		fb.beforeList = func() {
			if held.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
		}
		heartbeatDone := make(chan struct{})
		go func() {
			defer close(heartbeatDone)
			hb.heartbeat()
		}()
		<-entered

		changeDone := make(chan struct{})
		go func() {
			defer close(changeDone)
			change()
		}()
		select {
		case <-changeDone:
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		<-heartbeatDone
		<-changeDone
	}
	selectB2 := Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": "b2"}}
	check := func(t *testing.T, h *Hub, bob *Client) {
		t.Helper()
		if got := updatesFrom(t, bob, "u1"); len(got) == 0 || got[len(got)-1] != "b2" {
			t.Errorf("Bob's presence.updates for u1 were %q; the last must be b2, u1's newest selection", got)
		}
		if sel, _ := selectionOf(h, "f", "u1"); sel != "b2" {
			t.Errorf("Presence (what presence.sync sends) lists u1 on %q, want b2", sel)
		}
	}

	t.Run("one connection", func(t *testing.T) {
		h, fb := hubWithFake()
		c, bob := newClient(h, "u1", "c1"), newClient(h, "u2", "bob")
		c.SetSelectedBlockID("b1")
		h.Join("f", c)
		h.Join("f", bob)
		drainSend(bob)

		race(fb, c, func() { c.handleIncoming(selectB2) })
		check(t, h, bob)
	})

	// Without a store the window is only the heartbeat's own memory read
	// and broadcast, which presenceMu spans; left open, a change that
	// landed in it went out first in a sizeable share of runs.
	t.Run("one connection, no store", func(t *testing.T) {
		for i := range 5000 {
			h := NewHub()
			c, bob := newClient(h, "u1", "c1"), newClient(h, "u2", "bob")
			c.SetSelectedBlockID("b1")
			h.Join("f", c)
			h.Join("f", bob)
			drainSend(bob)

			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				c.heartbeat()
			}()
			go func() {
				defer wg.Done()
				<-start
				c.handleIncoming(selectB2)
			}()
			close(start)
			wg.Wait()
			if got := updatesFrom(t, bob, "u1"); len(got) != 2 || got[1] != "b2" {
				t.Fatalf("run %d: Bob's presence.updates for u1 were %q; the last must be b2, u1's newest selection", i+1, got)
			}
			h.Close() // stops the room's heartbeat goroutine
		}
	})

	t.Run("sibling tab on one replica", func(t *testing.T) {
		h, fb := hubWithFake()
		tab1, tab2, bob := newClient(h, "u1", "tab-1"), newClient(h, "u1", "tab-2"), newClient(h, "u2", "bob")
		tab1.SetSelectedBlockID("b1")
		for _, c := range []*Client{tab1, tab2, bob} {
			h.Join("f", c)
		}
		drainSend(bob)

		race(fb, tab2, func() { tab1.handleIncoming(selectB2) })
		check(t, h, bob)
	})
}

// TestJoin_PresenceSyncFallsBackToTheLocalRoomWhenTheStoreFails: a failed
// store read comes back as no records, and the joiner's presence.sync must
// not then be an authoritative empty list that wipes the client's roster —
// it falls back to the local room.
func TestJoin_PresenceSyncFallsBackToTheLocalRoomWhenTheStoreFails(t *testing.T) {
	h, fb := hubWithFake()
	zed := &Client{UserID: "u0", DisplayName: "Zed", hub: h, flowID: "f", connID: "c0", send: make(chan Envelope, 8)}
	h.Join("f", zed)

	fb.failList = true
	alice := &Client{UserID: "u1", DisplayName: "Alice", hub: h, flowID: "f", connID: "c1", send: make(chan Envelope, 8)}
	h.Join("f", alice)

	if env := <-alice.send; env.Type != EventPresenceJoin {
		t.Fatalf("joiner's first frame is %q, want its own presence.join", env.Type)
	}
	env := <-alice.send
	if env.Type != EventPresenceSync {
		t.Fatalf("joiner's second frame is %q, want presence.sync", env.Type)
	}
	users := env.Payload.(PresenceSyncPayload).Users
	names := map[string]string{}
	for _, u := range users {
		names[u.UserID] = u.DisplayName
	}
	if len(users) != 2 || names["u0"] != "Zed" || names["u1"] != "Alice" {
		t.Fatalf("presence.sync with the store unreadable = %+v, want the local room: Zed and Alice", users)
	}
}
