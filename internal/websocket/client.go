package websocket

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"pad-analyzer/internal/logger"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 64 * 1024 // 64 KB
	// sendBufferCap bounds per-client outgoing buffering. Collab events
	// (cursor moves, block updates) can burst — 64 was too tight and
	// caused silent drops under normal use. 256 gives modest headroom
	// while still bounding worst-case memory.
	sendBufferCap = 256
)

// Client represents a single WebSocket connection.
type Client struct {
	UserID          string
	DisplayName     string
	SelectedBlockID string

	hub          *Hub
	flowID       string
	conn         *websocket.Conn
	send         chan Envelope
	disconnected atomic.Bool // set once when Send or Run tears down the connection
}

// NewClient wraps an already-upgraded WebSocket connection.
func NewClient(hub *Hub, conn *websocket.Conn, userID, displayName, flowID string) *Client {
	return &Client{
		UserID:      userID,
		DisplayName: displayName,
		hub:         hub,
		flowID:      flowID,
		conn:        conn,
		send:        make(chan Envelope, sendBufferCap),
	}
}

// Run starts the read and write pumps, registers the client in the hub, and
// blocks until the connection is closed.
func (c *Client) Run() {
	c.hub.Join(c.flowID, c)
	defer func() {
		// Cleanup order:
		//   1. Leave the hub so future Broadcast snapshots stop including us.
		//   2. Close the underlying TCP conn so writePump's next WriteMessage
		//      (or ping tick) errors out and the pump returns.
		// We deliberately do NOT close c.send. Another goroutine that already
		// captured an old Broadcast snapshot might still be mid-iteration
		// calling c.Send → c.send <- env; concurrently closing c.send would
		// race against that send and panic. Instead we leak c.send and let
		// GC reclaim it once nothing references this Client. writePump
		// terminates via the network error, not via channel close.
		c.hub.Leave(c.flowID, c)
		c.disconnected.Store(true)
		_ = c.conn.Close()
	}()

	go c.writePump()
	c.readPump()
}

// Send queues an envelope for delivery to this client. If the send buffer
// is full the client is treated as too slow to keep up: instead of silently
// dropping the message (which leaves the UI silently stale — missed cursor
// moves, missed block edits), we close the underlying connection so the
// client reconnects with a fresh state. The close happens at the network
// layer; we never close c.send, so a concurrent caller iterating an old
// Broadcast snapshot can still safely push into the buffered channel.
func (c *Client) Send(env Envelope) {
	if c.disconnected.Load() {
		return
	}
	select {
	case c.send <- env:
		return
	default:
	}
	// Buffer full — slow client. Disconnect exactly once.
	if c.disconnected.CompareAndSwap(false, true) {
		logger.Warn("websocket: slow client disconnected (send buffer full)",
			"userID", c.UserID, "flowID", c.flowID, "buffer_cap", sendBufferCap)
		_ = c.conn.Close()
	}
}

// readPump reads incoming messages and dispatches them to the hub.
func (c *Client) readPump() {
	// NOTE: c.close() is intentionally NOT deferred here. The send channel is
	// closed in Run()'s deferred func, strictly after hub.Leave(), so no Broadcast
	// can ever observe a closed channel while the client is still in the hub.

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			break
		}

		var env Envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			c.Send(Envelope{
				Type:      EventError,
				Timestamp: time.Now(),
				Payload:   ErrorPayload{Code: "invalid_message", Message: err.Error()},
			})
			continue
		}

		env.UserID = c.UserID // always use the authenticated user ID
		env.FlowID = c.flowID
		env.Timestamp = time.Now()

		c.handleIncoming(env)
	}
}

// writePump writes queued messages and sends periodic pings.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case env := <-c.send:
			// c.send is never closed (see Run's defer comment), so the
			// `ok==false` branch isn't reachable. The pump exits when
			// conn.Close happens and WriteMessage below returns an error.
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))

			data, err := json.Marshal(env)
			if err != nil {
				continue
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleIncoming processes a validated inbound event from the client.
func (c *Client) handleIncoming(env Envelope) {
	switch env.Type {
	case EventPing:
		c.Send(Envelope{Type: EventPong, FlowID: c.flowID, Timestamp: time.Now()})

	case EventPresenceUpdate:
		if p, ok := parsePayload[PresencePayload](env.Payload); ok {
			c.SelectedBlockID = p.SelectedBlockID
		}
		c.hub.Broadcast(c.flowID, c.UserID, env, c)

	case EventCursorMove, EventBlockUpdate, EventBlockCreate, EventBlockDelete:
		c.hub.Broadcast(c.flowID, c.UserID, env, c)

	default:
		c.Send(Envelope{
			Type:      EventError,
			Timestamp: time.Now(),
			Payload:   ErrorPayload{Code: "unknown_event", Message: fmt.Sprintf("unknown event type: %s", env.Type)},
		})
	}
}

// parsePayload attempts to cast or re-marshal an arbitrary payload into T.
func parsePayload[T any](raw any) (T, bool) {
	var zero T
	if raw == nil {
		return zero, false
	}
	// Re-marshal via JSON to normalise map[string]any → T
	b, err := json.Marshal(raw)
	if err != nil {
		return zero, false
	}
	if err := json.Unmarshal(b, &zero); err != nil {
		return zero, false
	}
	return zero, true
}
