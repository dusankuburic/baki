package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	return httptest.NewServer(Handler(hub, userID, "Test User", nil, checker, "", time.Time{}, nil))
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

// TestHandler_PerUserConnectionCap is the regression test for the WS DoS gap:
// without a per-user connection cap a single authenticated account could open
// thousands of live sockets (each holding goroutines + buffers) and exhaust
// memory for the whole process. The (maxConnsPerUser+1)th connection for the
// same user must now be rejected with 503 before the upgrade allocates resources.
func TestHandler_PerUserConnectionCap(t *testing.T) {
	hub := NewHub()
	srv := newCheckedServer(hub, "cap-user", &fakeChecker{errs: map[string]error{}})
	defer srv.Close()

	// Open exactly the cap number of connections — all must succeed.
	conns := make([]*websocket.Conn, 0, maxConnsPerUser)
	for i := 0; i < maxConnsPerUser; i++ {
		_, conn := dialRaw(t, srv, "flow-cap")
		if conn == nil {
			t.Fatalf("connection %d/%d should have been accepted", i+1, maxConnsPerUser)
		}
		conns = append(conns, conn)
	}
	defer func() {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
	}()

	// The next connection for the SAME user must be refused with 503.
	status, conn := dialRaw(t, srv, "flow-cap")
	if conn != nil {
		conn.Close()
		t.Fatalf("connection %d should have been rejected (cap=%d)", maxConnsPerUser+1, maxConnsPerUser)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("over-cap connection: got status %d, want %d (Too many connections)", status, http.StatusServiceUnavailable)
	}

	// Closing one slot frees capacity for a fresh connection.
	conns[0].Close()
	conns[0] = nil
	// Give the server a moment to process the close and release the slot.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, conn := dialRaw(t, srv, "flow-cap"); conn != nil {
			conn.Close()
			return // slot was freed and re-acquired — pass
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a freed slot was not re-acquirable within timeout")
}

// TestAcquireConn_ReleaseIsIdempotent guards the release callback: calling it
// twice (which can happen if a disconnect path fires twice) must not drive the
// per-user count negative.
func TestAcquireConn_ReleaseIsIdempotent(t *testing.T) {
	hub := NewHub()
	rel, ok := hub.AcquireConn("alice")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	rel()
	rel() // must be a no-op, not drive the count negative

	rel2, ok := hub.AcquireConn("alice")
	if !ok {
		t.Fatal("re-acquire after idempotent double-release should succeed")
	}
	rel2()
}
