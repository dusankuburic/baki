package websocket

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"pad-core/logger"
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
	UserID      string
	DisplayName string

	blockMu         sync.Mutex
	selectedBlockID string

	hub    *Hub
	flowID string
	// connID uniquely identifies this connection in the shared presence store
	// (a user may hold several connections across replicas). Stable for the
	// connection's lifetime.
	connID       string
	conn         *websocket.Conn
	send         chan Envelope
	disconnected atomic.Bool // set once when Send or Run tears down the connection
	// authorized is set false by the re-authz goroutine when the user's access
	// is revoked. readPump checks it per-message so a revoked user can't inject
	// events in the window between revocation detection and connection close.
	authorized atomic.Bool
}

func (c *Client) SetSelectedBlockID(id string) {
	c.blockMu.Lock()
	c.selectedBlockID = id
	c.blockMu.Unlock()
}

func (c *Client) GetSelectedBlockID() string {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	return c.selectedBlockID
}

// NewClient wraps an already-upgraded WebSocket connection.
func NewClient(hub *Hub, conn *websocket.Conn, userID, displayName, flowID string) *Client {
	c := &Client{
		UserID:      userID,
		DisplayName: displayName,
		hub:         hub,
		flowID:      flowID,
		connID:      uuid.NewString(),
		conn:        conn,
		send:        make(chan Envelope, sendBufferCap),
	}
	c.authorized.Store(true)
	return c
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

// runReauthzLoop periodically re-validates this client's access — access-token
// revocation (blacklist hit) and flow ACL changes — and disconnects it the
// moment either happens, plus a one-shot timer that disconnects when the
// underlying access token's own expiry is reached (otherwise a live socket
// would outlive its credential indefinitely). Runs until ctx is cancelled; the
// caller (Handler) derives ctx from its own request scope and cancels it via
// a deferred call so the goroutine can't outlive the connection even if a
// panic unwinds the handler early. A no-op call (both checker and isRevoked
// nil) has nothing to check and should not be started by the caller.
//
// authorized is set false BEFORE closing the connection so readPump's
// per-message check (see readPump) stops processing inbound events
// immediately — this closes the ~2min stale-relay window where a revoked user
// could otherwise inject live-collab events between revocation detection and
// connection teardown.
func (c *Client) runReauthzLoop(ctx context.Context, checker FlowAccessChecker, accessJTI string, accessExp time.Time, isRevoked func(string) bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("websocket re-authz goroutine panicked", "err", r)
		}
	}()
	// Re-check at a 2-minute cadence (matches the SSE channel) so a
	// revocation is observed promptly rather than up to 5 minutes.
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	var expiryTimer *time.Timer
	if accessJTI != "" && !accessExp.IsZero() {
		if d := time.Until(accessExp); d > 0 {
			expiryTimer = time.AfterFunc(d, func() {
				slog.Info("websocket: disconnecting client after access token expired",
					"flowId", c.flowID, "userID", c.UserID)
				c.authorized.Store(false)
				_ = c.conn.Close()
			})
		}
	}
	if expiryTimer != nil {
		defer expiryTimer.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check the ACCESS token's revocation (logout, password change,
			// admin revoke) — the ticket's own JTI was only blacklisted for
			// ~30s at consumption, so re-checking it would never fire after
			// that.
			if isRevoked != nil && accessJTI != "" && isRevoked(accessJTI) {
				slog.Info("websocket: disconnecting client after token revoked",
					"flowId", c.flowID, "userID", c.UserID)
				c.authorized.Store(false)
				c.conn.Close()
				return
			}
			if checker != nil {
				if err := checker.CheckAccess(ctx, c.flowID, c.UserID); err != nil {
					slog.Info("websocket: disconnecting client after access revoked",
						"flowId", c.flowID, "userID", c.UserID)
					c.authorized.Store(false)
					c.conn.Close()
					return
				}
			}
		}
	}
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
	// NOTE: readPump does not tear the connection down itself — Run()'s deferred
	// cleanup does (hub.Leave then conn.Close). The send channel is deliberately
	// NEVER closed (see Run's comment): a concurrent Send holding an old Broadcast
	// snapshot must be able to push into c.send without racing a close. writePump
	// exits on the resulting conn error, not on a channel close.

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

		// Per-message authz: if the re-authz goroutine has flagged this
		// connection as revoked, stop processing inbound messages immediately
		// rather than waiting for the connection close to propagate. This
		// closes the ~2min stale-relay window where a revoked user could
		// inject live-collab events between revocation detection and the
		// connection teardown.
		if !c.authorized.Load() {
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
			// Piggyback a presence heartbeat on the ping so this client's shared
			// presence entry keeps its TTL refreshed (no-op single-replica).
			c.hub.writePresence(c)
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
			c.SetSelectedBlockID(p.SelectedBlockID)
			// Reflect the new selection in the shared presence store so peers'
			// Presence() queries see it (no-op single-replica).
			c.hub.writePresence(c)
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
