package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pad-analyzer/internal/auth"
)

// testIdentity is a session's Identity: an unscoped user, no revocation re-check.
func testIdentity(userID, displayName string) Identity {
	return Identity{Actor: &auth.Actor{Kind: auth.ActorUser, UserID: userID}, DisplayName: displayName}
}

// rankChecker grants read access to everyone and write access only to the users
// listed in editors — the shape of the real AuthzService adapter, which gates
// the room join at "viewer" and the HTTP write endpoints at "editor". Like
// AuthzService.Can, it asks the actor's scopes first: FlowRead takes read or
// write, FlowEdit takes write.
type rankChecker struct {
	editors map[string]bool
}

func (c *rankChecker) CheckAccess(_ context.Context, _ string, actor *auth.Actor) (FlowOwner, error) {
	if !actor.Allows(auth.ScopeRead) && !actor.Allows(auth.ScopeWrite) {
		return FlowOwner{}, ErrAccessDenied
	}
	return FlowOwner{}, nil
}

func (c *rankChecker) CanEdit(_ context.Context, _ string, actor *auth.Actor) bool {
	return actor.Allows(auth.ScopeWrite) && c.editors[actor.UserID]
}

func newRankServer(hub *Hub, userID string, checker FlowAccessChecker) *httptest.Server {
	return httptest.NewServer(Handler(hub, testIdentity(userID, userID), nil, checker, nil))
}

// newActorServer serves the socket of actor, a PAT's or a session's.
func newActorServer(hub *Hub, actor *auth.Actor, checker FlowAccessChecker) *httptest.Server {
	return httptest.NewServer(Handler(hub, Identity{Actor: actor, DisplayName: actor.UserID}, nil, checker, nil))
}

// TestWS_ViewerCannotBroadcastBlockMutations is the regression gate for a write
// path with no permission check.
//
// The room join is authorized at "viewer", and the HTTP write endpoints require
// "editor" — but handleIncoming broadcast EventBlockUpdate/Create/Delete in the
// same case arm as presence updates, with no rank check at all. So a read-only
// collaborator who was refused by POST /api/flow/save-source could send the
// identical mutation over the socket and have it delivered verbatim to every
// member of the room and out over the Redis backplane.
func TestWS_ViewerCannotBroadcastBlockMutations(t *testing.T) {
	hub := NewHub()
	checker := &rankChecker{editors: map[string]bool{"owner": true}}

	ownerSrv := newRankServer(hub, "owner", checker)
	defer ownerSrv.Close()
	viewerSrv := newRankServer(hub, "viewer", checker)
	defer viewerSrv.Close()

	ownerConn := dial(t, ownerSrv, "flow-1")
	defer ownerConn.Close()
	_ = readEnvelope(t, ownerConn) // own presence.join
	_ = readSync(t, ownerConn)

	viewerConn := dial(t, viewerSrv, "flow-1")
	defer viewerConn.Close()
	_ = readEnvelope(t, viewerConn) // own presence.join
	_ = readSync(t, viewerConn)
	_ = readEnvelope(t, ownerConn) // viewer's presence.join
	time.Sleep(50 * time.Millisecond)

	for _, evt := range []EventType{EventBlockUpdate, EventBlockCreate, EventBlockDelete} {
		sendEnvelope(t, viewerConn, Envelope{
			Type:    evt,
			Payload: map[string]any{"blockId": "BLOCK-1", "properties": map[string]any{"Command": "rm -rf /"}},
		})
	}

	// The sender must be told, rather than silently ignored.
	got := readEnvelope(t, viewerConn)
	if got.Type != EventError {
		t.Fatalf("viewer's block.update produced %q, want an error envelope", got.Type)
	}

	// And nothing may reach the room.
	_ = ownerConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := ownerConn.ReadMessage(); err == nil {
		t.Error("a viewer-tier collaborator's block mutation was delivered to the room — " +
			"the socket write path enforces no permission while the HTTP one requires editor")
	}
}

// TestWS_EditorCanStillBroadcastBlockMutations keeps the check from being drawn
// too wide: collaborative editing is the feature, and a gate that blocks
// everyone is a removal, not a fix.
func TestWS_EditorCanStillBroadcastBlockMutations(t *testing.T) {
	hub := NewHub()
	checker := &rankChecker{editors: map[string]bool{"owner": true, "editor": true}}

	ownerSrv := newRankServer(hub, "owner", checker)
	defer ownerSrv.Close()
	editorSrv := newRankServer(hub, "editor", checker)
	defer editorSrv.Close()

	ownerConn := dial(t, ownerSrv, "flow-1")
	defer ownerConn.Close()
	_ = readEnvelope(t, ownerConn)
	_ = readSync(t, ownerConn)

	editorConn := dial(t, editorSrv, "flow-1")
	defer editorConn.Close()
	_ = readEnvelope(t, editorConn)
	_ = readSync(t, editorConn)
	_ = readEnvelope(t, ownerConn) // editor's presence.join
	time.Sleep(50 * time.Millisecond)

	sendEnvelope(t, editorConn, Envelope{
		Type:    EventBlockUpdate,
		Payload: map[string]any{"blockId": "BLOCK-1"},
	})

	_ = ownerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	env := readEnvelope(t, ownerConn)
	if env.Type != EventBlockUpdate {
		t.Errorf("editor's block.update did not reach the room: got %q", env.Type)
	}
}

// TestWS_ViewerCanStillSendPresence pins the other half of the split: cursor
// and presence traffic is exactly what viewer rank is FOR, and it shares the
// case arm the mutations used to sit in.
func TestWS_ViewerCanStillSendPresence(t *testing.T) {
	hub := NewHub()
	checker := &rankChecker{editors: map[string]bool{"owner": true}}

	ownerSrv := newRankServer(hub, "owner", checker)
	defer ownerSrv.Close()
	viewerSrv := newRankServer(hub, "viewer", checker)
	defer viewerSrv.Close()

	ownerConn := dial(t, ownerSrv, "flow-1")
	defer ownerConn.Close()
	_ = readEnvelope(t, ownerConn)
	_ = readSync(t, ownerConn)

	viewerConn := dial(t, viewerSrv, "flow-1")
	defer viewerConn.Close()
	_ = readEnvelope(t, viewerConn)
	_ = readSync(t, viewerConn)
	_ = readEnvelope(t, ownerConn)
	time.Sleep(50 * time.Millisecond)

	sendEnvelope(t, viewerConn, Envelope{
		Type:    EventPresenceUpdate,
		Payload: map[string]any{"blockId": "BLOCK-2"},
	})

	_ = ownerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	env := readEnvelope(t, ownerConn)
	if env.Type != EventPresenceUpdate {
		t.Errorf("a viewer's presence update was blocked: got %q", env.Type)
	}
}

var _ = websocket.TextMessage

// lookupCounter is a FlowAccessChecker whose answers the test sets, counting
// the authz lookups made against it.
type lookupCounter struct {
	read, edit atomic.Bool
	lookups    atomic.Int32
}

func (c *lookupCounter) CheckAccess(_ context.Context, _ string, _ *auth.Actor) (FlowOwner, error) {
	c.lookups.Add(1)
	if !c.read.Load() {
		return FlowOwner{}, ErrAccessDenied
	}
	return FlowOwner{}, nil
}

func (c *lookupCounter) CanEdit(_ context.Context, _ string, _ *auth.Actor) bool {
	c.lookups.Add(1)
	return c.edit.Load()
}

// serverSideConn returns the server end of a live WebSocket connection.
func serverSideConn(t *testing.T) *websocket.Conn {
	t.Helper()
	conns := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conns <- conn
	}))
	t.Cleanup(srv.Close)
	client := dial(t, srv, "flow-1")
	t.Cleanup(func() { client.Close() })
	select {
	case conn := <-conns:
		t.Cleanup(func() { conn.Close() })
		return conn
	case <-time.After(2 * time.Second):
		t.Fatal("no server-side connection")
		return nil
	}
}

// TestRecheckAccess_OneLookupForAnEditor: the re-authz loop runs every 2
// minutes on every connection, and each pass used to make two full authz
// lookups — the read check, then the write rank. The editor rank includes read
// access, so an editor needs only the second; a viewer still needs both, and
// losing read access still disconnects.
func TestRecheckAccess_OneLookupForAnEditor(t *testing.T) {
	checker := &lookupCounter{}
	c := NewClient(NewHub(), serverSideConn(t), testIdentity("u1", "U1").Actor, "U1", "flow-1", true)

	// Editor: one lookup, still connected and still an editor.
	checker.read.Store(true)
	checker.edit.Store(true)
	if !c.recheckAccess(context.Background(), checker) {
		t.Fatal("editor was disconnected")
	}
	if n := checker.lookups.Load(); n != 1 {
		t.Fatalf("editor re-check made %d authz lookups, want 1", n)
	}
	if !c.canEdit.Load() || !c.authorized.Load() {
		t.Fatal("editor lost write rank or authorization")
	}

	// Demoted to viewer: stays connected, loses the write rank.
	checker.lookups.Store(0)
	checker.edit.Store(false)
	if !c.recheckAccess(context.Background(), checker) {
		t.Fatal("viewer was disconnected")
	}
	if n := checker.lookups.Load(); n != 2 {
		t.Fatalf("viewer re-check made %d authz lookups, want 2", n)
	}
	if c.canEdit.Load() || !c.authorized.Load() {
		t.Fatal("demotion was not applied, or the viewer was deauthorized")
	}

	// Read access revoked: deauthorized and disconnected.
	checker.read.Store(false)
	if c.recheckAccess(context.Background(), checker) {
		t.Fatal("revoked user stayed connected")
	}
	if c.authorized.Load() {
		t.Fatal("revoked user still authorized")
	}
	c.conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := c.conn.ReadMessage(); err == nil {
		t.Fatal("revoked user's connection is still open")
	}
}
