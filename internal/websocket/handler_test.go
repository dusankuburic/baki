package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// fakeChecker returns a fixed error per flowID, standing in for the
// AuthzService-backed adapter wired up in the DI layer.
type fakeChecker struct {
	errs map[string]error
}

func (f *fakeChecker) CheckAccess(_ context.Context, flowID, _ string) error {
	return f.errs[flowID]
}

func newCheckedServer(hub *Hub, userID string, checker FlowAccessChecker) *httptest.Server {
	return httptest.NewServer(Handler(hub, userID, "Test User", nil, checker))
}

// dialRaw attempts a WebSocket upgrade and returns the HTTP response status
// (0 when the upgrade succeeded).
func dialRaw(t *testing.T, server *httptest.Server, flowID string) (int, *websocket.Conn) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(server.URL, "http") + "?flowId=" + flowID
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		if resp == nil {
			t.Fatalf("dial failed with no response: %v", err)
		}
		return resp.StatusCode, nil
	}
	return 0, conn
}

func TestHandler_AccessDenied_Returns403(t *testing.T) {
	hub := NewHub()
	checker := &fakeChecker{errs: map[string]error{"secret-flow": ErrAccessDenied}}
	srv := newCheckedServer(hub, "mallory", checker)
	defer srv.Close()

	status, conn := dialRaw(t, srv, "secret-flow")
	if conn != nil {
		conn.Close()
		t.Fatal("expected upgrade to be rejected")
	}
	if status != http.StatusForbidden {
		t.Errorf("expected 403, got %d", status)
	}
}

func TestHandler_FlowNotFound_Returns404(t *testing.T) {
	hub := NewHub()
	checker := &fakeChecker{errs: map[string]error{"missing": ErrFlowNotFound}}
	srv := newCheckedServer(hub, "alice", checker)
	defer srv.Close()

	status, conn := dialRaw(t, srv, "missing")
	if conn != nil {
		conn.Close()
		t.Fatal("expected upgrade to be rejected")
	}
	if status != http.StatusNotFound {
		t.Errorf("expected 404, got %d", status)
	}
}

func TestHandler_AccessAllowed_Upgrades(t *testing.T) {
	hub := NewHub()
	checker := &fakeChecker{errs: map[string]error{}} // nil error = allowed
	srv := newCheckedServer(hub, "alice", checker)
	defer srv.Close()

	status, conn := dialRaw(t, srv, "my-flow")
	if conn == nil {
		t.Fatalf("expected upgrade to succeed, got status %d", status)
	}
	conn.Close()
}

func TestHandler_NilChecker_SkipsCheck(t *testing.T) {
	// Local mode: no checker wired, any flow ID is accepted.
	hub := NewHub()
	srv := newCheckedServer(hub, "local", nil)
	defer srv.Close()

	status, conn := dialRaw(t, srv, "any-flow")
	if conn == nil {
		t.Fatalf("expected upgrade to succeed without checker, got status %d", status)
	}
	conn.Close()
}
