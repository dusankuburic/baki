package websocket

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 64 * 1024 // 64 KB
)

// Client represents a single WebSocket connection.
type Client struct {
	UserID          string
	DisplayName     string
	SelectedBlockID string

	hub    *Hub
	flowID string
	conn   *websocket.Conn
	send   chan Envelope
	once   sync.Once
}

// NewClient wraps an already-upgraded WebSocket connection.
func NewClient(hub *Hub, conn *websocket.Conn, userID, displayName, flowID string) *Client {
	return &Client{
		UserID:      userID,
		DisplayName: displayName,
		hub:         hub,
		flowID:      flowID,
		conn:        conn,
		send:        make(chan Envelope, 64),
	}
}

// Run starts the read and write pumps, registers the client in the hub, and
// blocks until the connection is closed.
func (c *Client) Run() {
	c.hub.Join(c.flowID, c)
	defer func() {
		// Leave the hub BEFORE closing the send channel. If close() fired first,
		// the client would remain in the hub's snapshot with a closed channel and
		// any concurrent hub.Broadcast() call would panic on "send on closed channel"
		// (Go panics on a select-case send to a closed channel, unlike a receive).
		// Ordering: Leave → close → conn.Close gives writePump a clean exit path.
		c.hub.Leave(c.flowID, c)
		c.close()       // signal writePump to exit (ok==false on c.send)
		c.conn.Close()  // force-close if writePump hasn't already
	}()

	go c.writePump()
	c.readPump()
}

// Send queues an envelope for delivery to this client.
// Drops the message (non-blocking) if the send buffer is full.
func (c *Client) Send(env Envelope) {
	select {
	case c.send <- env:
	default:
	}
}

// close drains and closes the send channel exactly once.
func (c *Client) close() {
	c.once.Do(func() { close(c.send) })
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
		case env, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

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
