package websocket

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
)

// declaredEventTypes returns every event type constant the package declares (name → value), read from its
// non-test sources: a constant typed EventType, or one named Event*.
func declaredEventTypes(t *testing.T) map[string]EventType {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]EventType{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				typed := false
				if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "EventType" {
					typed = true
				}
				for i, name := range vs.Names {
					if !typed && !strings.HasPrefix(name.Name, "Event") {
						continue
					}
					if i >= len(vs.Values) {
						t.Fatalf("%s: event type %s has no literal value", f, name.Name)
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: event type %s is not a string literal", f, name.Name)
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[name.Name] = EventType(v)
				}
			}
		}
	}
	return out
}

// TestInboundNeeds_CoverEveryEventType: every event type is either declared client-sendable, with its need, or
// server-only, never both. A new type that is neither fails here, so it cannot ship without deciding what a client
// needs to send it.
func TestInboundNeeds_CoverEveryEventType(t *testing.T) {
	declared := declaredEventTypes(t)
	if len(declared) < 10 {
		t.Fatalf("found only %d event types (%v): the source walk is broken", len(declared), declared)
	}
	values := map[EventType]bool{}
	for name, v := range declared {
		values[v] = true
		_, inbound := inboundNeeds[v]
		switch {
		case inbound && serverOnly[v]:
			t.Errorf("%s (%q) is both client-sendable and server-only", name, v)
		case !inbound && !serverOnly[v]:
			t.Errorf("%s (%q) is in neither inboundNeeds nor serverOnly: declare what a client needs to send it", name, v)
		}
	}
	for v := range inboundNeeds {
		if !values[v] {
			t.Errorf("inboundNeeds lists %q, which is not a declared event type", v)
		}
	}
	for v := range serverOnly {
		if !values[v] {
			t.Errorf("serverOnly lists %q, which is not a declared event type", v)
		}
	}
}

// newTableClient is a connection-less client of flow-1 for driving handleIncoming directly; its queue is the
// sender's view.
func newTableClient(hub *Hub, actor *auth.Actor, canEdit bool) *Client {
	c := &Client{UserID: actor.UserID, DisplayName: actor.UserID, actor: actor, hub: hub, flowID: "flow-1",
		connID: "conn-" + actor.UserID, send: make(chan Envelope, 64)}
	c.authorized.Store(true)
	c.canEdit.Store(canEdit)
	return c
}

// errorCodes drains c's queue and returns the codes of the error envelopes on it.
func errorCodes(c *Client) []string {
	var codes []string
	for len(c.send) > 0 {
		env := <-c.send
		if env.Type == EventError {
			codes = append(codes, env.Payload.(ErrorPayload).Code)
		}
	}
	return codes
}

// TestInboundNeeds_EveryDeclaredTypeIsHandled: a type declared client-sendable must reach a handler, not the
// fail-closed default arm that answers unknown_event.
func TestInboundNeeds_EveryDeclaredTypeIsHandled(t *testing.T) {
	hub := NewHub()
	c := newTableClient(hub, &auth.Actor{Kind: auth.ActorUser, UserID: "u1"}, true)
	for typ := range inboundNeeds {
		env := Envelope{Type: typ, FlowID: "flow-1", UserID: "u1", Timestamp: time.Now()}
		if typ == EventPresenceUpdate {
			env.Payload = json.RawMessage(`{"selectedBlockId":"b1"}`)
		}
		c.handleIncoming(env)
		if codes := errorCodes(c); len(codes) != 0 {
			t.Errorf("%s from an editor was answered %v", typ, codes)
		}
	}
}

// refusedCount reads pad_ws_inbound_refused_total{type,reason} from the process registry.
func refusedCount(t *testing.T, typ, reason string) float64 {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "pad_ws_inbound_refused_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["type"] == typ && labels["reason"] == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestRefusedEdit_DroppedCountedAndNoticeRateLimited: a mutation the connection may not send is dropped (no peer
// sees it), counted per frame, and answered with permission_denied at most once per deniedNoticeEvery, so a
// client that keeps sending edits is told without the answers becoming a flood. The socket stays open.
func TestRefusedEdit_DroppedCountedAndNoticeRateLimited(t *testing.T) {
	hub := NewHub()
	reader := newTableClient(hub, &auth.Actor{Kind: auth.ActorToken, UserID: "u1", TokenID: "tok1", Scopes: []string{auth.ScopeRead}}, false)
	peer := newTableClient(hub, &auth.Actor{Kind: auth.ActorUser, UserID: "u2"}, true)
	hub.Join("flow-1", reader)
	hub.Join("flow-1", peer)
	t.Cleanup(func() { hub.Leave("flow-1", reader); hub.Leave("flow-1", peer) })
	drainSend(reader)
	drainSend(peer)

	before := refusedCount(t, string(EventBlockUpdate), refusedEdit)
	for range 3 {
		reader.handleIncoming(Envelope{Type: EventBlockUpdate, FlowID: "flow-1", UserID: "u1", Payload: json.RawMessage(`{"blockId":"B1"}`)})
	}
	reader.handleIncoming(Envelope{Type: EventBlockDelete, FlowID: "flow-1", UserID: "u1"})

	if got := refusedCount(t, string(EventBlockUpdate), refusedEdit) - before; got != 3 {
		t.Errorf("counted %v refused block.update frames, want 3", got)
	}
	if len(peer.send) != 0 {
		t.Fatalf("a refused mutation reached a peer: %+v", <-peer.send)
	}
	var notices []ErrorPayload
	for len(reader.send) > 0 {
		if env := <-reader.send; env.Type == EventError {
			notices = append(notices, env.Payload.(ErrorPayload))
		}
	}
	if len(notices) != 1 || notices[0].Code != "permission_denied" || !strings.Contains(notices[0].Message, "write scope") {
		t.Fatalf("four refused frames answered %+v, want one permission_denied naming the write scope", notices)
	}
	if !reader.authorized.Load() {
		t.Fatal("a refused frame deauthorized the connection")
	}

	// The window passes: the next refusal is answered again.
	reader.deniedNotice.at = reader.deniedNotice.at.Add(-deniedNoticeEvery)
	reader.handleIncoming(Envelope{Type: EventBlockCreate, FlowID: "flow-1", UserID: "u1"})
	if codes := errorCodes(reader); len(codes) != 1 || codes[0] != "permission_denied" {
		t.Fatalf("after the window a refused frame was answered %v, want one permission_denied", codes)
	}
}

// TestRefusedUnknownAndInvalid_Counted: unknown and server-only types, and malformed frames, are counted under a
// bounded type label and answered as before (every frame).
func TestRefusedUnknownAndInvalid_Counted(t *testing.T) {
	hub := NewHub()
	c := newTableClient(hub, &auth.Actor{Kind: auth.ActorUser, UserID: "u1"}, true)

	other, join := refusedCount(t, "other", refusedUnknown), refusedCount(t, string(EventPresenceJoin), refusedUnknown)
	invalid := refusedCount(t, string(EventPresenceUpdate), refusedInvalid)
	c.handleIncoming(Envelope{Type: "cursor.move"})
	c.handleIncoming(Envelope{Type: "cursor.move"})
	c.handleIncoming(Envelope{Type: EventPresenceJoin})
	c.handleIncoming(Envelope{Type: EventPresenceUpdate, Payload: json.RawMessage(`{"selectedBlockId":42}`)})

	if got := refusedCount(t, "other", refusedUnknown) - other; got != 2 {
		t.Errorf("made-up type counted %v times, want 2", got)
	}
	if got := refusedCount(t, string(EventPresenceJoin), refusedUnknown) - join; got != 1 {
		t.Errorf("server-only type counted %v times, want 1", got)
	}
	if got := refusedCount(t, string(EventPresenceUpdate), refusedInvalid) - invalid; got != 1 {
		t.Errorf("malformed presence.update counted %v times, want 1", got)
	}
	want := []string{"unknown_event", "unknown_event", "unknown_event", "invalid_message"}
	if got := errorCodes(c); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("answers = %v, want %v", got, want)
	}
}

func TestRefusedTypeLabel_IsBounded(t *testing.T) {
	for typ, want := range map[EventType]string{
		EventBlockUpdate: "block.update", EventPong: "pong", "": "other", EventType(strings.Repeat("y", 500)): "other",
	} {
		if got := refusedTypeLabel(typ); got != want {
			t.Errorf("refusedTypeLabel(%.20q) = %q, want %q", typ, got, want)
		}
	}
}

// TestWS_ReadScopedTokenOfEditorCannotBroadcast is R7: a read-scoped PAT whose owner holds editor rank on the
// flow joined the room and relayed block.* as an editor, because the socket's actor dropped the token's scopes.
// The actor now carries them, so the write rank the checker grants takes the write scope too. Presence, which is
// what read access is for, still goes through.
func TestWS_ReadScopedTokenOfEditorCannotBroadcast(t *testing.T) {
	hub := NewHub()
	checker := &rankChecker{editors: map[string]bool{"owner": true}}
	pat := &auth.Actor{Kind: auth.ActorToken, UserID: "owner", TokenID: "tok1", Scopes: []string{auth.ScopeRead}}

	peerSrv := newRankServer(hub, "owner", checker)
	defer peerSrv.Close()
	patSrv := newActorServer(hub, pat, checker)
	defer patSrv.Close()

	peer := dial(t, peerSrv, "flow-1")
	defer peer.Close()
	_ = readEnvelope(t, peer)
	_ = readSync(t, peer)
	tokenConn := dial(t, patSrv, "flow-1")
	defer tokenConn.Close()
	_ = readEnvelope(t, tokenConn)
	_ = readSync(t, tokenConn)
	_ = readEnvelope(t, peer) // the token connection's presence.join
	peerFrames := frames(t, peer)

	sendEnvelope(t, tokenConn, Envelope{Type: EventBlockUpdate, Payload: map[string]any{"blockId": "BLOCK-1"}})
	got := readEnvelope(t, tokenConn)
	if code := payloadMap(t, got)["code"]; got.Type != EventError || code != "permission_denied" {
		t.Fatalf("read-scoped token's block.update answered %q %v, want permission_denied", got.Type, code)
	}
	if env, ok := waitFrame(peerFrames, 300*time.Millisecond, func(e Envelope) bool { return strings.HasPrefix(string(e.Type), "block.") }); ok {
		t.Fatalf("a read-scoped token's block mutation reached the room: %+v", env)
	}

	sendEnvelope(t, tokenConn, Envelope{Type: EventPresenceUpdate, Payload: map[string]any{"selectedBlockId": "BLOCK-2"}})
	if _, ok := waitFrame(peerFrames, 2*time.Second, func(e Envelope) bool { return e.Type == EventPresenceUpdate }); !ok {
		t.Fatal("a read-scoped token's presence update was blocked")
	}
}

// TestWS_ReadWriteTokenOfEditorBroadcasts keeps the scope check from being drawn too wide: a token with write
// relays an editor's mutation.
func TestWS_ReadWriteTokenOfEditorBroadcasts(t *testing.T) {
	hub := NewHub()
	checker := &rankChecker{editors: map[string]bool{"owner": true}}
	pat := &auth.Actor{Kind: auth.ActorToken, UserID: "owner", TokenID: "tok1", Scopes: []string{auth.ScopeRead, auth.ScopeWrite}}

	peerSrv := newRankServer(hub, "owner", checker)
	defer peerSrv.Close()
	patSrv := newActorServer(hub, pat, checker)
	defer patSrv.Close()

	peer := dial(t, peerSrv, "flow-1")
	defer peer.Close()
	_ = readEnvelope(t, peer)
	_ = readSync(t, peer)
	tokenConn := dial(t, patSrv, "flow-1")
	defer tokenConn.Close()
	_ = readEnvelope(t, tokenConn)
	_ = readSync(t, tokenConn)

	sendEnvelope(t, tokenConn, Envelope{Type: EventBlockUpdate, Payload: map[string]any{"blockId": "BLOCK-1"}})
	if env := readUntil(t, peer, EventBlockUpdate); env.UserID != "owner" {
		t.Errorf("relayed block.update carries userId %q, want owner", env.UserID)
	}
}

// TestWS_NoChecker_ScopedActorCannotEdit: without a checker (no storage) the write rank used to be granted to
// everyone; it now follows the actor's scopes, so a scoped actor reaching this path fails closed, while an
// unscoped one edits as before.
func TestWS_NoChecker_ScopedActorCannotEdit(t *testing.T) {
	hub := NewHub()
	readSrv := newActorServer(hub, &auth.Actor{Kind: auth.ActorToken, UserID: "u1", Scopes: []string{auth.ScopeRead}}, nil)
	defer readSrv.Close()
	userSrv := newActorServer(hub, &auth.Actor{Kind: auth.ActorUser, UserID: "u2"}, nil)
	defer userSrv.Close()

	reader := dial(t, readSrv, "flow-1")
	defer reader.Close()
	_ = readEnvelope(t, reader)
	_ = readSync(t, reader)
	user := dial(t, userSrv, "flow-1")
	defer user.Close()
	_ = readEnvelope(t, user)
	_ = readSync(t, user)
	_ = readEnvelope(t, reader) // u2's presence.join

	sendEnvelope(t, reader, Envelope{Type: EventBlockUpdate, Payload: map[string]any{"blockId": "B"}})
	if got := readEnvelope(t, reader); got.Type != EventError || payloadMap(t, got)["code"] != "permission_denied" {
		t.Fatalf("scoped actor without a checker: block.update answered %+v", got)
	}
	sendEnvelope(t, user, Envelope{Type: EventBlockUpdate, Payload: map[string]any{"blockId": "B"}})
	if env := readUntil(t, reader, EventBlockUpdate); env.UserID != "u2" {
		t.Errorf("unscoped user's block.update arrived from %q", env.UserID)
	}
}

// TestRecheckAccess_ReadScopedTokenStaysReadOnly: the re-authz loop asks the checker about the socket's actor, so
// a read-scoped token of an editor-rank user is not promoted to editor on a tick, and keeps its read access.
func TestRecheckAccess_ReadScopedTokenStaysReadOnly(t *testing.T) {
	checker := &rankChecker{editors: map[string]bool{"owner": true}}
	pat := &auth.Actor{Kind: auth.ActorToken, UserID: "owner", TokenID: "tok1", Scopes: []string{auth.ScopeRead}}
	c := NewClient(NewHub(), serverSideConn(t), pat, "owner", "flow-1", false)
	for tick := range 2 {
		if !c.recheckAccess(t.Context(), checker) {
			t.Fatalf("tick %d disconnected a token with read access", tick)
		}
		if c.canEdit.Load() {
			t.Fatalf("tick %d gave a read-scoped token the write rank", tick)
		}
	}

	// A chat-only token has no read access to the flow: the recheck disconnects it.
	chat := &auth.Actor{Kind: auth.ActorToken, UserID: "owner", TokenID: "tok2", Scopes: []string{auth.ScopeChat}}
	c2 := NewClient(NewHub(), serverSideConn(t), chat, "owner", "flow-1", false)
	if c2.recheckAccess(t.Context(), checker) || c2.authorized.Load() {
		t.Fatal("a chat-only token kept a socket that needs read access")
	}
}

// TestHandler_RequiresAnActor: an identity without an actor (or without a user) is refused before anything else.
func TestHandler_RequiresAnActor(t *testing.T) {
	for name, id := range map[string]Identity{
		"no actor": {DisplayName: "x"},
		"no user":  {Actor: &auth.Actor{Kind: auth.ActorUser}},
	} {
		srv := httptest.NewServer(Handler(NewHub(), id, nil, nil, nil))
		status, conn := dialRaw(t, srv, "flow-1")
		srv.Close()
		if conn != nil {
			conn.Close()
			t.Fatalf("%s: upgraded", name)
		}
		if status != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, status)
		}
	}
}
