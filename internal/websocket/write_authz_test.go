package websocket

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// rankChecker grants read access to everyone and write access only to the users
// listed in editors — the shape of the real AuthzService adapter, which gates
// the room join at "viewer" and the HTTP write endpoints at "editor".
type rankChecker struct {
	editors map[string]bool
}

func (c *rankChecker) CheckAccess(_ context.Context, _, _ string) error { return nil }

func (c *rankChecker) CanEdit(_ context.Context, _, userID string) bool {
	return c.editors[userID]
}

func newRankServer(hub *Hub, userID string, checker FlowAccessChecker) *httptest.Server {
	return httptest.NewServer(Handler(hub, userID, userID, nil, checker, "", time.Time{}, nil))
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

	viewerConn := dial(t, viewerSrv, "flow-1")
	defer viewerConn.Close()
	_ = readEnvelope(t, viewerConn) // own presence.join
	_ = readEnvelope(t, ownerConn)  // viewer's presence.join
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

	editorConn := dial(t, editorSrv, "flow-1")
	defer editorConn.Close()
	_ = readEnvelope(t, editorConn)
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

	viewerConn := dial(t, viewerSrv, "flow-1")
	defer viewerConn.Close()
	_ = readEnvelope(t, viewerConn)
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
