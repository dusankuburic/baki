package websocket

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	platformdrain "pad-analyzer/internal/platform/drain"
)

// While the server drains (ADR 0024), a WebSocket upgrade is refused with 503 SERVER_RESTARTING and a Retry-After
// before anything is allocated, so the client's reconnect reaches another replica; the hub's own shutdown closes
// the sockets already open.
func TestHandler_DrainingRefusesUpgrade(t *testing.T) {
	st := platformdrain.New()
	prev := draining
	draining = st
	t.Cleanup(func() { draining = prev })

	hub := NewHub()
	srv := newCheckedServer(hub, "alice", &fakeChecker{errs: map[string]error{}})
	defer srv.Close()

	status, conn := dialRaw(t, srv, "my-flow")
	if conn == nil {
		t.Fatalf("upgrade before draining refused with %d", status)
	}
	conn.Close()

	st.Begin()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "?flowId=my-flow"
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil {
		conn.Close()
		t.Fatal("upgrade accepted while draining")
	}
	if resp == nil {
		t.Fatalf("dial failed with no response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "2" {
		t.Errorf("refusal %d Retry-After %q, want 503 and 2", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Code != platformdrain.Code {
		t.Errorf("refusal body code %q (%v), want %s", body.Code, err, platformdrain.Code)
	}
}
