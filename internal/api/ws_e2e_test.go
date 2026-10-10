package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	wshub "pad-analyzer/internal/websocket"
)

// socketChecker is internal/di's FlowAccessChecker adapter, which this package cannot import (di imports api): the
// socket's actor goes straight to AuthzService.Can (AuthorizeFlow for the read, which also names the flow's owner).
type socketChecker struct{ authz *service.AuthzService }

func (c socketChecker) CheckAccess(ctx context.Context, flowID string, actor *auth.Actor) (wshub.FlowOwner, error) {
	switch h, err := c.authz.AuthorizeFlow(ctx, actor, service.FlowRead, flowID); {
	case err == nil:
		return wshub.FlowOwner{UserID: h.OwnerID, OrgID: h.OrganizationID}, nil
	case errors.Is(err, storageif.ErrNotFound):
		return wshub.FlowOwner{}, wshub.ErrFlowNotFound
	case errors.Is(err, service.ErrPermissionDenied):
		return wshub.FlowOwner{}, wshub.ErrAccessDenied
	default:
		return wshub.FlowOwner{}, err
	}
}

func (c socketChecker) CanEdit(ctx context.Context, flowID string, actor *auth.Actor) bool {
	return c.authz.Can(ctx, actor, service.FlowEdit, service.FlowByID(flowID)) == nil
}

// wsFrames is one end-to-end socket and the frames it receives, decoded by a background reader (a gorilla read
// that runs into its deadline poisons every later read).
type wsFrames struct {
	conn   *websocket.Conn
	frames chan wshub.Envelope
}

// dialFlow fetches a ticket as bearer through srv and opens /ws on flowID with it.
func dialFlow(t *testing.T, srv *httptest.Server, bearer, flowID string) *wsFrames {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/ws-ticket", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", bearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("ws-ticket: %v", err)
	}
	var body struct {
		Ticket    string    `json:"ticket"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	err = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || body.Ticket == "" || body.ExpiresAt.IsZero() {
		t.Fatalf("ws-ticket: status %d, body %+v, err %v", resp.StatusCode, body, err)
	}

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?flowId=" + flowID + "&ticket=" + body.Ticket
	conn, wsResp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		status := 0
		if wsResp != nil {
			status = wsResp.StatusCode
		}
		t.Fatalf("dial /ws: %v (status %d)", err, status)
	}
	t.Cleanup(func() { conn.Close() })
	ws := &wsFrames{conn: conn, frames: make(chan wshub.Envelope, 64)}
	go func() {
		defer close(ws.frames)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env wshub.Envelope
			if json.Unmarshal(msg, &env) == nil {
				ws.frames <- env
			}
		}
	}()
	ws.await(t, wshub.EventPresenceSync, 2*time.Second)
	return ws
}

func (ws *wsFrames) send(t *testing.T, frame string) {
	t.Helper()
	if err := ws.conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// next returns the first frame of type typ within d, skipping others; ok=false if none arrives.
func (ws *wsFrames) next(typ wshub.EventType, d time.Duration) (wshub.Envelope, bool) {
	deadline := time.After(d)
	for {
		select {
		case env, open := <-ws.frames:
			if !open {
				return wshub.Envelope{}, false
			}
			if env.Type == typ {
				return env, true
			}
		case <-deadline:
			return wshub.Envelope{}, false
		}
	}
}

func (ws *wsFrames) await(t *testing.T, typ wshub.EventType, d time.Duration) wshub.Envelope {
	t.Helper()
	env, ok := ws.next(typ, d)
	if !ok {
		t.Fatalf("no %s frame within %v", typ, d)
	}
	return env
}

// TestWS_EndToEnd_TicketScopesGateMutations is R7 through the real router: a PAT gets a ticket at POST
// /api/ws-ticket and opens /ws with it, and the socket acts as that token. The owner of the flow holds editor
// rank, so only the scope decides: a read PAT's block.update is refused with permission_denied and reaches no one,
// a read+write PAT's is relayed to the owner's own session in the room.
func TestWS_EndToEnd_TicketScopesGateMutations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scopes  []string
		relayed bool
	}{
		{"pat:read", []string{auth.ScopeRead}, false},
		{"pat:read+write", []string{auth.ScopeRead, auth.ScopeWrite}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, seedFlow := newLibraryTestRouter(t)
			seedUserWithRole(t, rt, "u-owner", "u-owner@acme.io", auth.RoleMember)
			seedFlow("f-mine", "u-owner")
			rt.flowChecker = socketChecker{authz: service.NewAuthzService(rt.security.Backend, rt.security.OrgSvc)}
			srv := httptest.NewServer(rt)
			defer srv.Close()

			pair, err := rt.security.AuthMgr.Issue("u-owner", "u-owner@acme.io", auth.RoleMember)
			if err != nil {
				t.Fatal(err)
			}
			session := "Bearer " + pair.AccessToken
			pat := mintPAT(t, rt, session, tc.scopes)

			peer := dialFlow(t, srv, session, "f-mine")
			token := dialFlow(t, srv, pat, "f-mine")
			peer.await(t, wshub.EventPresenceJoin, 2*time.Second) // the token's connection

			token.send(t, `{"type":"block.update","payload":{"blockId":"BLOCK-1","properties":{"Command":"rm -rf /"}}}`)
			if tc.relayed {
				if env := peer.await(t, wshub.EventBlockUpdate, 2*time.Second); env.UserID != "u-owner" || env.FlowID != "f-mine" {
					t.Errorf("relayed frame stamped user %q flow %q", env.UserID, env.FlowID)
				}
				if env, ok := token.next(wshub.EventError, 200*time.Millisecond); ok {
					t.Errorf("an allowed mutation was answered %+v", env.Payload)
				}
				return
			}
			env := token.await(t, wshub.EventError, 2*time.Second)
			if p, _ := env.Payload.(map[string]any); p["code"] != "permission_denied" {
				t.Fatalf("refused mutation answered %+v, want permission_denied", env.Payload)
			}
			if env, ok := peer.next(wshub.EventBlockUpdate, 300*time.Millisecond); ok {
				t.Fatalf("a read PAT's block.update reached the room: %+v", env)
			}
			// Refused, not disconnected: the socket still answers.
			token.send(t, `{"type":"ping"}`)
			token.await(t, wshub.EventPong, 2*time.Second)
		})
	}
}
