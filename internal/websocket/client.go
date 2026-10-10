package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"pad-analyzer/internal/auth"
	"pad-core/logger"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 64 * 1024 // 64 KB
	// sendBufferCap bounds per-client outgoing buffering. Collab events
	// (selection updates, block updates) can burst — 64 was too tight and
	// caused silent drops under normal use; 256 gives headroom while bounding
	// worst-case memory.
	sendBufferCap = 256
	// inboundRate / inboundBurst throttle messages ACCEPTED from one
	// connection: each accepted message is amplified (a JSON unmarshal, K
	// marshals and sends for a room of K, and a 64 KB Redis PUBLISH every
	// replica decodes), so an authorized collaborator flooding the socket at
	// line rate becomes room-sized CPU amplification plus a line-rate stream
	// into Redis. The burst must be at least sendBufferCap (256) — outbound
	// bursts are DRIVEN by inbound messages, so a smaller one sheds normal
	// use on the way in. A coarse backstop, not a quota: 200/s sustained
	// still lets one connection push ~12 MB/s of 64 KiB frames.
	inboundRate  = 200
	inboundBurst = 512
	// deniedNoticeEvery bounds the permission_denied answers to one
	// connection, so a client that keeps sending edits it may not make is
	// told without the answers becoming an outbound flood.
	deniedNoticeEvery = 5 * time.Second
)

// Client represents a single WebSocket connection.
type Client struct {
	// UserID is actor.UserID, kept as a field for the hub's many readers.
	UserID      string
	DisplayName string
	// actor is who the connection acts as (Identity.Actor), immutable for its
	// lifetime: the re-authz loop asks the FlowAccessChecker about it.
	actor *auth.Actor

	blockMu         sync.Mutex
	selectedBlockID string
	// selectedAt stamps the last SetSelectedBlockID (selectionStamp; 0 =
	// never), so the user's connections can agree on which of their
	// selections is the user's (see selectionNewer).
	selectedAt int64

	// presenceMu serializes the shared presence writes for this connection.
	// Its two callers — the room heartbeat (Hub.heartbeat, on the room's own
	// goroutine) and a selection change on readPump — each read the selection
	// and then write the shared record in a separate Redis round trip, so
	// without it a heartbeat that read the old selection could land its write
	// after the change's — in the store and in the presence.update broadcast —
	// until the next heartbeat. Run's cleanup takes it once, after marking the
	// connection disconnected, to wait out a heartbeat already working on this
	// connection before Leave. Taken before h.mu and blockMu, never while
	// holding either; nothing holding one waits for another.
	presenceMu sync.Mutex

	hub    *Hub
	flowID string
	// connID uniquely identifies this connection in the shared presence store
	// (a user may hold several across replicas). Stable for the connection's
	// lifetime.
	connID       string
	conn         *websocket.Conn
	send         chan Envelope
	disconnected atomic.Bool // set once when Send or Run tears down the connection
	// authorized is set false by the re-authz goroutine when the user's access
	// is revoked. readPump checks it per-message so a revoked user can't inject
	// events between revocation detection and connection close.
	authorized atomic.Bool
	// canEdit mirrors the "editor" rank and write scope the HTTP write
	// endpoints require (FlowAccessChecker.CanEdit), set at connect and
	// refreshed by the re-authz loop, so a demotion to viewer takes effect on
	// a live connection.
	canEdit atomic.Bool
	// deniedNotice rate-limits the permission_denied answers (readPump only).
	deniedNotice noticeGate

	// flowOwner (from the connect check) and accessJTI (Identity.AccessJTI)
	// are what the hub matches an access change against (Hub.RecheckOrg,
	// RecheckOwner, RecheckCredential); admittedAt is Hub.accessChanges as of
	// the connect check. Set before Run, never changed.
	flowOwner  FlowOwner
	accessJTI  string
	admittedAt uint64
	// recheckNow asks the re-authz loop for a pass now rather than at its
	// next tick (Client.requestRecheck). One slot: a burst of access changes
	// leaves one pass pending, not one per change.
	recheckNow chan struct{}

	// done is closed when Run returns — after BOTH pumps have exited — so
	// Hub.Shutdown can wait for every client's pumps to fully exit before
	// reporting shutdown complete.
	done chan struct{}
	// stop is closed by Run's cleanup to end writePump, which closes
	// writerDone on its way out. Without it writePump only noticed the closed
	// connection at its next write — up to pingPeriod (54 s) later — holding
	// the Client and its send buffer past the per-user connection cap.
	stop       chan struct{}
	writerDone chan struct{}
}

// shutdown signals the client to exit cleanly: a CloseGoingAway control
// frame, then force-close of the underlying TCP conn so a wedged writePump
// can't keep the client alive past the server's shutdown budget. Closing the
// conn makes writePump's next WriteMessage and readPump's next ReadMessage
// error out; Run's defer then closes c.done so Hub.Shutdown can stop waiting.
func (c *Client) shutdown() {
	_ = c.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutdown"),
		time.Now().Add(writeWait),
	)
	_ = c.conn.Close()
}

func (c *Client) SetSelectedBlockID(id string) {
	at := selectionStamp()
	c.blockMu.Lock()
	c.selectedBlockID = id
	c.selectedAt = at
	c.blockMu.Unlock()
}

// selection returns this connection's selection and when it was set.
func (c *Client) selection() (string, int64) {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	return c.selectedBlockID, c.selectedAt
}

// lastSelectionStamp backs selectionStamp.
var lastSelectionStamp atomic.Int64

// selectionStamp returns the wall clock in unix nanoseconds — comparable
// across replicas, to within their clock skew — made strictly increasing
// within this process, so two selections in one clock tick still have an order.
func selectionStamp() int64 {
	now := time.Now().UnixNano()
	for {
		last := lastSelectionStamp.Load()
		next := max(now, last+1)
		if lastSelectionStamp.CompareAndSwap(last, next) {
			return next
		}
	}
}

// NewClient wraps an already-upgraded WebSocket connection for actor, who must
// not be nil.
func NewClient(hub *Hub, conn *websocket.Conn, actor *auth.Actor, displayName, flowID string, canEdit bool) *Client {
	c := &Client{
		UserID:      actor.UserID,
		DisplayName: displayName,
		actor:       actor,
		hub:         hub,
		flowID:      flowID,
		connID:      uuid.NewString(),
		conn:        conn,
		send:        make(chan Envelope, sendBufferCap),
		recheckNow:  make(chan struct{}, 1),
		done:        make(chan struct{}),
		stop:        make(chan struct{}),
		writerDone:  make(chan struct{}),
	}
	c.authorized.Store(true)
	c.canEdit.Store(canEdit)
	return c
}

// Run starts the read and write pumps, registers the client in the hub, and
// blocks until the connection is closed.
func (c *Client) Run() {
	c.hub.Join(c.flowID, c)
	// An access change that committed after the connect check looked for this
	// socket before it was in its room: re-check once now, not at the first
	// 2-minute pass. A recheck bumps accessChanges before it looks, so it
	// either found the socket or is seen here.
	if c.hub.accessChanges.Load() != c.admittedAt {
		c.requestRecheck()
	}
	defer func() {
		// Cleanup order:
		//   1. Mark disconnected first: Send becomes a no-op and the room
		//      heartbeat skips this connection from now on.
		//   2. Close the underlying TCP conn, so a write in flight in
		//      writePump fails at once.
		//   3. Close stop and wait for writePump to return — it would
		//      otherwise idle until its next ping tick.
		//   4. Wait out a room heartbeat already working on this connection:
		//      it holds presenceMu from its disconnected check to its
		//      broadcast, so taking the lock once here returns only after it
		//      is done, and a heartbeat that takes it later sees step 1 and
		//      skips. Without this wait such a heartbeat could land after
		//      Leave — re-writing the presence record Leave had just removed
		//      (a ghost suppressing the real presence.leave) and
		//      re-broadcasting a presence.update peers answer by re-adding
		//      the departed user. Bounded by the backplane's own per-call
		//      timeouts.
		//   5. Only then Leave the hub.
		//   6. Close done so Hub.Shutdown can stop waiting on this client.
		// c.send is deliberately NOT closed: a goroutine iterating an old
		// Broadcast snapshot might still be mid-Send, and concurrently closing
		// c.send would race that send and panic; writePump terminates via stop
		// (or a network error), not via channel close.
		c.disconnected.Store(true)
		_ = c.conn.Close()
		close(c.stop)
		<-c.writerDone
		c.presenceMu.Lock()
		c.presenceMu.Unlock() //nolint:staticcheck // SA2001: the empty critical section is the wait
		c.hub.Leave(c.flowID, c)
		close(c.done)
	}()

	go c.writePump()
	c.readPump()
}

// runReauthzLoop re-validates this client's access — access-token revocation
// (blacklist hit) and flow ACL changes — every 2 minutes and whenever an access
// change reaches it (recheckNow), and disconnects it once either is gone, plus
// a one-shot timer for the access token's own expiry (a live socket would
// otherwise outlive its credential indefinitely). Runs until ctx is cancelled;
// the caller (Handler) derives ctx from its own request scope and cancels it
// via a deferred call so the goroutine can't outlive the connection even if a
// panic unwinds the handler early.
//
// authorized is set false BEFORE closing the connection so readPump's
// per-message check stops processing inbound events immediately, closing the
// window where a revoked user could inject live-collab events.
func (c *Client) runReauthzLoop(ctx context.Context, checker FlowAccessChecker, accessJTI string, accessExp time.Time, isRevoked func(string) bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("websocket re-authz goroutine panicked", "err", r)
		}
	}()
	// Re-check at a 2-minute cadence (matches the SSE channel) so a
	// revocation is observed promptly rather than up to 5 minutes. The
	// periodic pass is the backstop for what no event reaches here: another
	// replica's change, a change nothing announces.
	ticker := time.NewTicker(c.hub.reauthzPeriod)
	defer ticker.Stop()
	if accessJTI != "" && !accessExp.IsZero() {
		if d := time.Until(accessExp); d > 0 {
			defer time.AfterFunc(d, func() {
				slog.Info("websocket: disconnecting client after access token expired",
					"flowId", c.flowID, "userID", c.UserID)
				c.authorized.Store(false)
				_ = c.conn.Close()
			}).Stop()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.recheckNow:
			// An access change committed (Hub.RecheckFlow and the rest): pass
			// now, holding one of the hub's recheck slots, so a change that
			// reaches hundreds of sockets at once (an organisation deleted)
			// runs a few authz lookups at a time. The periodic pass never
			// waits behind the slots: if it falls due first, it runs as always.
			select {
			case c.hub.recheckSlots <- struct{}{}:
				if !c.reauthorizeInSlot(ctx, checker, accessJTI, isRevoked) {
					return
				}
				continue
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
		if !c.reauthorize(ctx, checker, accessJTI, isRevoked) {
			return
		}
	}
}

// reauthorize is one pass of the re-authz loop: the access token's
// revocation, then the flow ACL. It reports false once it has disconnected
// the client.
func (c *Client) reauthorize(ctx context.Context, checker FlowAccessChecker, accessJTI string, isRevoked func(string) bool) bool {
	// Check the ACCESS token's revocation (logout, password change,
	// admin revoke) — the ticket's own JTI was only blacklisted for
	// ~30s at consumption, so re-checking it would never fire after that.
	if isRevoked != nil && accessJTI != "" && isRevoked(accessJTI) {
		slog.Info("websocket: disconnecting client after token revoked",
			"flowId", c.flowID, "userID", c.UserID)
		c.authorized.Store(false)
		c.conn.Close()
		return false
	}
	return checker == nil || c.recheckAccess(ctx, checker)
}

// reauthorizeInSlot is reauthorize for a caller holding a hub recheck slot,
// which it gives back however the pass ends.
func (c *Client) reauthorizeInSlot(ctx context.Context, checker FlowAccessChecker, accessJTI string, isRevoked func(string) bool) bool {
	defer func() { <-c.hub.recheckSlots }()
	return c.reauthorize(ctx, checker, accessJTI, isRevoked)
}

// requestRecheck asks the re-authz loop for a pass now. It never waits: when a
// pass is already pending it has not started yet, so it will read the change
// this request is for as well.
func (c *Client) requestRecheck() {
	select {
	case c.recheckNow <- struct{}{}:
	default:
	}
}

// recheckAccess is one flow-ACL pass of the re-authz loop: it refreshes the
// write rank and disconnects the client once it has lost read access,
// reporting false in that case. The write rank is refreshed on every pass
// because a demotion from editor to viewer keeps read access — without it the
// demoted user would keep write rights on this socket until they disconnect.
// Asked FIRST: the editor rank includes read access (see FlowAccessChecker),
// so the read check — a second full authz lookup per connection every 2
// minutes — only runs for a user who cannot edit.
func (c *Client) recheckAccess(ctx context.Context, checker FlowAccessChecker) bool {
	canEdit := checker.CanEdit(ctx, c.flowID, c.actor)
	if !canEdit {
		if _, err := checker.CheckAccess(ctx, c.flowID, c.actor); err != nil {
			slog.Info("websocket: disconnecting client after access revoked",
				"flowId", c.flowID, "userID", c.UserID)
			c.authorized.Store(false)
			c.conn.Close()
			return false
		}
	}
	if canEdit != c.canEdit.Load() {
		slog.Info("websocket: client write permission changed",
			"flowId", c.flowID, "userID", c.UserID, "canEdit", canEdit)
		c.canEdit.Store(canEdit)
	}
	return true
}

// Send queues an envelope for delivery to this client. If the send buffer
// is full the client is treated as too slow to keep up: instead of silently
// dropping the message (which leaves the UI stale — missed cursor moves,
// missed block edits), we close the underlying connection so the client
// reconnects with a fresh state. The close happens at the network layer; we
// never close c.send, so a concurrent caller iterating an old Broadcast
// snapshot can still safely push into the buffered channel.
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
	// readPump does not tear the connection down itself — Run()'s deferred
	// cleanup does (conn.Close, then hub.Leave once writePump has returned).
	// The send channel is deliberately NEVER closed (see Run's comment): a
	// concurrent Send holding an old Broadcast snapshot must be able to push
	// into c.send without racing a close; writePump exits on c.stop (or a conn
	// error), not on a channel close.

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	limiter := newInboundLimiter(inboundRate, inboundBurst)

	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			break
		}

		// Shed before doing any work on the message: unmarshal, fan-out and the
		// Redis publish all happen below, and shedding after them would defeat
		// the point. A client that sustains the overrun is told once and then
		// silently dropped, so the error path cannot itself become the flood.
		if !limiter.allow() {
			if limiter.shouldWarn() {
				c.Send(Envelope{
					Type:      EventError,
					Timestamp: time.Now(),
					Payload:   ErrorPayload{Code: "rate_limited", Message: "too many messages; slow down"},
				})
			}
			continue
		}

		// Per-message authz: a connection the re-authz goroutine has flagged
		// as revoked stops processing inbound messages immediately, closing
		// the window where a revoked user could inject live-collab events
		// between revocation detection and teardown.
		if !c.authorized.Load() {
			break
		}

		var in inboundEnvelope
		if err := json.Unmarshal(msg, &in); err != nil {
			recordWSInboundRefused(refusedTypeLabel(in.Type), refusedInvalid)
			c.Send(Envelope{
				Type:      EventError,
				Timestamp: time.Now(),
				Payload:   ErrorPayload{Code: "invalid_message", Message: err.Error()},
			})
			continue
		}

		c.handleIncoming(Envelope{
			Type:      in.Type,
			FlowID:    c.flowID,
			UserID:    c.UserID, // always use the authenticated user ID
			Timestamp: time.Now(),
			Payload:   inboundPayload(in.Payload),
		})
	}
}

// writePump writes queued messages and sends periodic pings. Nothing else
// runs on it: the presence heartbeat now runs on the room's own goroutine
// (Hub.runRoomHeartbeat) — its store round trips used to keep queued messages
// waiting here while the send buffer filled and Send cut the client off.
func (c *Client) writePump() {
	ticker := time.NewTicker(c.hub.heartbeatPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
		close(c.writerDone)
	}()

	for {
		select {
		case <-c.stop:
			return

		case env := <-c.send:
			// c.send is never closed (see Run's defer comment), so the
			// `ok==false` branch isn't reachable; the pump exits on c.stop or
			// a failed WriteMessage on a closed conn.
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))

			// A broadcast arrives already encoded (Hub.Broadcast, or a peer
			// replica's bytes); only an envelope sent to this client alone is
			// marshalled here.
			data := env.wire
			if data == nil {
				var err error
				if data, err = json.Marshal(env); err != nil {
					continue
				}
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

// heartbeat is the presence refresh for this connection alone — what the
// room heartbeat (Hub.heartbeat) does for every live connection of the room
// at once, and the unit the presence ordering tests drive.
func (c *Client) heartbeat() {
	c.hub.heartbeat([]*Client{c})
}

// broadcastPresence sends the user's presence to the rest of the room (this
// connection excluded) as a presence.update built from server-side state
// only: the authenticated user ID, the display name the server knows, and
// selectedBlockID — the user's selection as the caller determined it.
func (c *Client) broadcastPresence(selectedBlockID string) {
	c.hub.Broadcast(c.flowID, c.UserID, Envelope{
		Type:      EventPresenceUpdate,
		FlowID:    c.flowID,
		UserID:    c.UserID,
		Timestamp: time.Now(),
		Payload: presenceUpdateOut{
			UserID:          c.UserID,
			DisplayName:     c.DisplayName,
			SelectedBlockID: selectedBlockID,
		},
	}, c)
}

// presenceUpdateIn is the one field the server reads from an inbound
// presence.update. Everything else the client puts in the payload is ignored.
type presenceUpdateIn struct {
	SelectedBlockID string `json:"selectedBlockId"`
}

// parsePresenceUpdate extracts the selection from an inbound presence.update
// payload. An empty selectedBlockId is a valid deselect; a payload that is not
// a JSON object with a string selectedBlockId, or an id over
// maxSelectedBlockIDLen bytes, is rejected.
func parsePresenceUpdate(raw any) (string, error) {
	p, ok := parsePayload[presenceUpdateIn](raw)
	if !ok {
		return "", errors.New("presence.update payload must be an object with a string selectedBlockId")
	}
	if len(p.SelectedBlockID) > maxSelectedBlockIDLen {
		return "", fmt.Errorf("selectedBlockId exceeds %d bytes", maxSelectedBlockIDLen)
	}
	return p.SelectedBlockID, nil
}

// need is what an inbound frame type requires of the connection sending it.
type need uint8

const (
	// needJoined: what joining the room took, FlowRead (checked at the upgrade,
	// on an access change and every 2 minutes; losing it disconnects).
	needJoined need = iota + 1
	// needEdit: FlowEdit, the editor rank and the write scope (canEdit, set at
	// connect and refreshed on an access change and every 2 minutes; one
	// atomic load per frame).
	needEdit
)

// inboundNeeds declares every event type a client may send and what sending it
// needs. A type missing here is refused as unknown_event, and
// TestInboundNeeds_CoverEveryEventType fails an EventType that is neither here
// nor in serverOnly, so a new client-sendable type cannot ship undeclared.
var inboundNeeds = map[EventType]need{
	EventPing:           needJoined,
	EventPresenceUpdate: needJoined,
	EventBlockUpdate:    needEdit,
	EventBlockCreate:    needEdit,
	EventBlockDelete:    needEdit,
}

// serverOnly lists the event types only the server sends; a client sending one
// is answered unknown_event.
var serverOnly = map[EventType]bool{
	EventPresenceJoin:  true,
	EventPresenceLeave: true,
	EventPresenceSync:  true,
	EventFlowChanged:   true,
	EventError:         true,
	EventPong:          true,
}

// Reasons of pad_ws_inbound_refused_total.
const (
	refusedEdit    = "edit"
	refusedUnknown = "unknown"
	refusedInvalid = "invalid"
)

// refusedTypeLabel bounds the type label of pad_ws_inbound_refused_total: a
// declared event type keeps its name, anything a client made up is "other".
func refusedTypeLabel(t EventType) string {
	if _, ok := inboundNeeds[t]; ok || serverOnly[t] {
		return string(t)
	}
	return "other"
}

// handleIncoming processes a validated inbound event from the client: it
// checks the need inboundNeeds declares for the type, then handles the event.
// A refused frame is dropped, counted and answered to the sender, and the
// connection stays open: the shipped client reconnects after every close, so
// closing would turn a refusal into a reconnect loop.
func (c *Client) handleIncoming(env Envelope) {
	switch inboundNeeds[env.Type] {
	case needJoined:
	case needEdit:
		// Mutations require the same rank and scope as the HTTP write
		// endpoints. Without this, a read-only collaborator or a read-scoped
		// token refused by POST /api/flow/save-source could send the identical
		// edit here and have it relayed verbatim to every member of the room
		// and out over the Redis backplane.
		if !c.canEdit.Load() {
			c.refuseEdit(env.Type)
			return
		}
	default:
		c.refuseUnknown(env.Type)
		return
	}

	switch env.Type {
	case EventPing:
		c.Send(Envelope{Type: EventPong, FlowID: c.flowID, Timestamp: time.Now()})

	case EventPresenceUpdate:
		// The client's envelope is never relayed: readPump overwrites only
		// the envelope's own userId, so relaying the payload as received
		// would let a viewer rename itself "Admin" or plant an entry under
		// another user's id in every peer's presence list. Only
		// selectedBlockId is read from the client; what peers receive is
		// rebuilt from the authenticated connection. A malformed payload is
		// refused to the sender, never stored or broadcast.
		sel, err := parsePresenceUpdate(env.Payload)
		if err != nil {
			recordWSInboundRefused(refusedTypeLabel(env.Type), refusedInvalid)
			c.Send(Envelope{
				Type:      EventError,
				Timestamp: time.Now(),
				Payload:   ErrorPayload{Code: "invalid_message", Message: err.Error()},
			})
			return
		}
		// c.presenceMu from the change through its announcement: see
		// Hub.heartbeat.
		c.presenceMu.Lock()
		c.SetSelectedBlockID(sel)
		// Reflect the new selection in the shared presence store so peers'
		// Presence() queries see it (no-op single-replica). The store write
		// is enqueued, not awaited: the pump must never wait on Redis (see
		// Hub.writePresenceLocked).
		c.hub.writePresenceLocked(c)
		// The selection just set is the user's newest by definition, so it
		// goes out as is — passing it keeps a replica whose clock runs behind
		// a peer's from announcing another tab's older selection.
		c.broadcastPresence(sel)
		c.presenceMu.Unlock()

	case EventBlockUpdate, EventBlockCreate, EventBlockDelete:
		c.hub.Broadcast(c.flowID, c.UserID, env, c)

	default:
		// Declared in inboundNeeds but handled nowhere: refuse rather than
		// guess (TestInboundNeeds_EveryDeclaredTypeIsHandled).
		c.refuseUnknown(env.Type)
	}
}

// refuseEdit drops a mutation the connection may not send, counts it, and
// tells the sender at most once per deniedNoticeEvery.
func (c *Client) refuseEdit(t EventType) {
	recordWSInboundRefused(refusedTypeLabel(t), refusedEdit)
	if !c.deniedNotice.allow(time.Now(), deniedNoticeEvery) {
		return
	}
	msg := "editor access is required to modify this flow"
	if c.actor.Scoped() && !c.actor.Allows(auth.ScopeWrite) {
		msg = "this API token lacks the write scope required to modify this flow"
	}
	c.Send(Envelope{
		Type:      EventError,
		Timestamp: time.Now(),
		Payload:   ErrorPayload{Code: "permission_denied", Message: msg},
	})
}

// refuseUnknown drops a frame of a type no client may send, counts it, and
// answers unknown_event to every such frame, as before (the inbound limiter
// bounds them).
func (c *Client) refuseUnknown(t EventType) {
	recordWSInboundRefused(refusedTypeLabel(t), refusedUnknown)
	c.Send(Envelope{
		Type:      EventError,
		Timestamp: time.Now(),
		Payload:   ErrorPayload{Code: "unknown_event", Message: fmt.Sprintf("unknown event type: %s", t)},
	})
}

// inboundPayload turns a client frame's raw payload into Envelope.Payload. It
// stays raw JSON, which json.Marshal re-emits verbatim (compacted), so a
// relayed payload is not decoded into a map only to be serialized again. An
// absent or null payload is nil, which omitempty drops. A payload that is not
// valid UTF-8 is decoded after all: decoding replaces the invalid bytes with
// U+FFFD, whereas relaying them raw would put invalid UTF-8 in peers' text
// frames, which browsers answer by failing the connection.
func inboundPayload(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if !utf8.Valid(raw) {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil
		}
		return v
	}
	return raw
}

// parsePayload attempts to cast or re-marshal an arbitrary payload into T.
func parsePayload[T any](raw any) (T, bool) {
	var zero T
	if raw == nil {
		return zero, false
	}
	// An inbound payload is already JSON: decode it directly.
	if r, ok := raw.(json.RawMessage); ok {
		if err := json.Unmarshal(r, &zero); err != nil {
			return zero, false
		}
		return zero, true
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

// inboundLimiter is a token bucket over accepted inbound messages, owned by one
// readPump goroutine — so it needs no lock.
type inboundLimiter struct {
	rate     float64 // tokens per second
	burst    float64
	tokens   float64
	last     time.Time
	warned   bool
	warnedAt time.Time
}

func newInboundLimiter(rate, burst int) *inboundLimiter {
	return &inboundLimiter{
		rate:   float64(rate),
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
	}
}

func (l *inboundLimiter) allow() bool {
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// shouldWarn rate-limits the rate-limit notice itself to once every 5s, so a
// flooding client cannot turn the warning into an outbound flood.
func (l *inboundLimiter) shouldWarn() bool {
	now := time.Now()
	if l.warned && now.Sub(l.warnedAt) < 5*time.Second {
		return false
	}
	l.warned = true
	l.warnedAt = now
	return true
}

// noticeGate lets a notice to the sender through at most once per period. Owned
// by the readPump goroutine, like inboundLimiter, so it needs no lock.
type noticeGate struct {
	sent bool
	at   time.Time
}

func (g *noticeGate) allow(now time.Time, period time.Duration) bool {
	if g.sent && now.Sub(g.at) < period {
		return false
	}
	g.sent, g.at = true, now
	return true
}
