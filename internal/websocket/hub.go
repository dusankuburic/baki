package websocket

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/redisx"
)

// maxConnsPerUser bounds live WebSocket connections per user; without it one
// authenticated account can exhaust the process's memory/goroutines. Mirrors
// the SSE cap in events.go (10). Per process, and with more than one replica
// configured also across the replicas (ShareConnCap, conn_cap.go); while that
// Redis count cannot be read, one account can hold 10×N sockets.
const maxConnsPerUser = 10

// presenceDeleteWait bounds how long Leave waits for the presence writer's
// ordered delete of the leaving connection's record; on timeout the delete is
// still queued and the record's TTL reaps it.
const presenceDeleteWait = 2 * time.Second

// Hub manages all active WebSocket rooms: one room per open flow document;
// clients join/leave rooms as they open/close flows.
type Hub struct {
	mu           sync.RWMutex
	rooms        map[string]*room // keyed by flowID
	connsPerUser map[string]int   // userID -> open connection count
	// connSlots counts a user's sockets across the replicas (ShareConnCap); nil is this hub's cap only.
	connSlots *redisx.SlotCap

	// backplane is nil for single-replica/desktop (pure in-memory). When set
	// (multi-replica, PAD_REDIS_URL), room broadcasts fan out over pub/sub and
	// presence is read from a shared store so users on other replicas are seen.
	backplane backplane
	// pub queues backplane publishes so Broadcast never waits on Redis; nil
	// with no backplane.
	pub *publisher
	// presenceW drains presence-record upserts to the backplane on its own
	// goroutine, coalescing per connection, so a selection change or heartbeat
	// never waits on a store round trip; nil with no backplane.
	presenceW *presenceWriter
	// replicaID identifies this process so it can ignore its own broadcasts
	// echoed back over pub/sub.
	replicaID string
	// closed is set by Close: no room started after it runs a heartbeat.
	closed bool

	// heartbeatPeriod is how often each client's writePump pings and each
	// room's heartbeat re-announces the room's users. Always pingPeriod
	// outside tests; a test shortens it on its own Hub before any client
	// connects (a package-level period would race with every other test
	// under -race).
	heartbeatPeriod time.Duration

	// reauthzPeriod is each socket's periodic access recheck, always
	// reauthzEvery outside tests (set like heartbeatPeriod).
	reauthzPeriod time.Duration
	// recheckSlots bounds the access rechecks that access changes start
	// (Hub.RecheckFlow and the rest) running at once across the hub.
	recheckSlots chan struct{}
	// accessChanges counts those rechecks, so a socket admitted while one
	// looked for it can tell (Client.Run).
	accessChanges atomic.Uint64
}

const (
	// reauthzEvery is the periodic access recheck of every socket.
	reauthzEvery = 2 * time.Minute
	// maxConcurrentRechecks bounds the event-driven rechecks in flight (one or
	// two authz lookups each, on the database pool), so an organisation
	// deleted under hundreds of open sockets queues them instead of taking
	// the pool. Small: a recheck is a point lookup, and a socket waiting for
	// a slot still gets its periodic pass.
	maxConcurrentRechecks = 4
)

// room holds all clients subscribed to a particular flow.
type room struct {
	flowID  string
	clients map[*Client]bool
	// stop ends the room's heartbeat goroutine: closed when the room is
	// deleted or the hub closed.
	stop     chan struct{}
	stopOnce sync.Once
}

// stopHeartbeat ends the room's heartbeat goroutine; idempotent — Leave stops
// it when the room empties, Close stops whatever is left.
func (r *room) stopHeartbeat() {
	r.stopOnce.Do(func() { close(r.stop) })
}

// NewHub creates a single-replica, in-memory Hub ready to accept connections.
func NewHub() *Hub {
	return &Hub{rooms: make(map[string]*room), connsPerUser: make(map[string]int), heartbeatPeriod: pingPeriod,
		reauthzPeriod: reauthzEvery, recheckSlots: make(chan struct{}, maxConcurrentRechecks)}
}

// NewHubWithRedis creates a Hub backed by a Redis pub/sub + presence backplane
// so presence and broadcasts are consistent across replicas. A nil client
// (Redis disabled) falls back to the in-memory NewHub — the multi-replica
// opt-in gated on PAD_REDIS_URL, mirroring the rate limiter / chat-resume paths.
func NewHubWithRedis(client *redis.Client) *Hub {
	h := NewHub()
	if client == nil {
		return h
	}
	h.replicaID = uuid.NewString()
	h.useBackplane(newRedisBackplane(client, h.replicaID, h.deliverRemote))
	return h
}

// useBackplane attaches b to the hub and starts the publisher that feeds it.
// Call before any client connects.
func (h *Hub) useBackplane(b backplane) {
	h.backplane = b
	h.pub = newPublisher(b)
	// The presence writer starts here, stops in close (like the publisher).
	h.presenceW = newPresenceWriter(b)
	go h.presenceW.run()
}

// Close stops every room's heartbeat, drains the publisher and releases the
// backplane subscriber (the latter two no-ops for an in-memory hub). Call on
// shutdown; this form waits at most publishCloseFallback for the publisher.
// Idempotent.
func (h *Hub) Close() {
	h.close(context.Background())
}

// close is Close bounded by ctx: the publisher drains what it can before
// ctx's deadline and is abandoned at it, so Shutdown fits the budget it was
// given (internal/app allows 3 s) even when that budget is already spent.
func (h *Hub) close(ctx context.Context) {
	h.mu.Lock()
	h.closed = true
	for _, r := range h.rooms {
		r.stopHeartbeat()
	}
	h.mu.Unlock()
	if h.pub != nil {
		h.pub.close(ctx)
	}
	if h.presenceW != nil {
		h.presenceW.close()
	}
	if h.backplane != nil {
		h.backplane.close()
	}
}

// Shutdown gracefully drains all connected WebSocket clients: sends each a
// CloseGoingAway control frame, closes the TCP connection, waits for every
// client's read/write pumps to exit (or ctx to elapse), then closes the hub
// within what is left of ctx. Call BEFORE server.Shutdown: http.Server.Shutdown
// does not close hijacked sockets, so without this every rolling restart takes
// the full shutdownCtx budget and drops in-flight collab state silently.
// Idempotent.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	clients := make([]*Client, 0)
	for _, r := range h.rooms {
		for c := range r.clients {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()

	if len(clients) == 0 {
		h.close(ctx)
		return nil
	}

	// Send each client a Close frame + close the conn; writePump sees the
	// WriteMessage error (or completes the queued close) and exits, readPump
	// sees the conn close. Each client's `done` is closed by Run's defer when
	// both pumps have returned.
	for _, c := range clients {
		c.shutdown()
	}
	// Wait for all clients to finish draining, respecting ctx.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		remaining := 0
		for _, c := range clients {
			select {
			case <-c.done:
			default:
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		select {
		case <-ctx.Done():
			h.close(ctx)
			return ctx.Err()
		case <-ticker.C:
		}
	}

	h.close(ctx)
	return nil
}

// AcquireConn atomically reserves a WebSocket connection slot for userID,
// honoring the per-user cap (maxConnsPerUser); returns a release callback
// (idempotent) and ok=false at the cap. Enforced before the WS upgrade so a
// refused connection never allocates goroutines or buffers. With the shared
// cap (ShareConnCap) the user's sockets on every replica count too.
func (h *Hub) AcquireConn(userID string) (release func(), ok bool) {
	h.mu.Lock()
	if h.connsPerUser[userID] >= maxConnsPerUser {
		h.mu.Unlock()
		return func() {}, false
	}
	h.connsPerUser[userID]++
	h.mu.Unlock()
	// Outside the hub's lock: a Redis round trip must not hold up every room.
	shared, ok := h.connSlots.Acquire(context.Background(), userID)
	if !ok {
		h.dropConn(userID)
		return func() {}, false
	}
	var released bool
	return func() {
		if released {
			return
		}
		released = true
		h.dropConn(userID)
		shared()
	}, true
}

// dropConn gives back one of userID's local connection slots.
func (h *Hub) dropConn(userID string) {
	h.mu.Lock()
	h.connsPerUser[userID]--
	if h.connsPerUser[userID] <= 0 {
		delete(h.connsPerUser, userID)
	}
	h.mu.Unlock()
}

// Join registers a client in the room for flowID, announces it with
// presence.join, then hands the joiner a presence.sync snapshot of who is
// already there. The room — and its heartbeat goroutine — is created on the
// first join.
func (h *Hub) Join(flowID string, c *Client) {
	h.mu.Lock()
	r, ok := h.rooms[flowID]
	if !ok {
		r = &room{flowID: flowID, clients: make(map[*Client]bool), stop: make(chan struct{})}
		h.rooms[flowID] = r
		if !h.closed {
			go h.runRoomHeartbeat(r)
		}
	}
	r.clients[c] = true
	h.mu.Unlock()

	recordWSConnectionChange(+1)

	// Write this client's presence and read the room in one round trip when
	// the backplane pipelines the pair (no-op single-replica). The pipeline's
	// HGETALL runs after its HSET, so the snapshot carries the joiner's own
	// record — the read-your-own-write the two-call form guaranteed.
	users := h.joinPresence(flowID, c)

	h.Broadcast(flowID, c.UserID, Envelope{
		Type:      EventPresenceJoin,
		FlowID:    flowID,
		UserID:    c.UserID,
		Timestamp: time.Now(),
		Payload: PresencePayload{
			UserID:      c.UserID,
			DisplayName: c.DisplayName,
		},
	}, nil)

	// Then tell the joiner — and only the joiner — who is already here:
	// presence.join is broadcast once, on arrival, so without this snapshot a
	// client joining a room in use never saw the users already editing.
	//
	// The snapshot is not atomic with the room's other traffic: a peer that
	// joins or leaves between the snapshot and this Send can have its own
	// presence.join/leave queued ahead of it; the room's presence.update
	// heartbeat re-adds a missed peer within one ping period, and the client's
	// stale-entry sweep drops a departed one.
	//
	// A backplane snapshot without the joiner proves the store read (or the
	// record write above) failed: sending it as authoritative would wipe the
	// joiner's roster — its own entry included, which nothing re-adds, since
	// heartbeats skip their sender. The local room is then sent instead: a
	// lower bound that always holds the joiner.
	if !hasUser(users, c.UserID) {
		users = h.localPresence(flowID)
	}
	c.Send(Envelope{
		Type:      EventPresenceSync,
		FlowID:    flowID,
		UserID:    c.UserID,
		Timestamp: time.Now(),
		Payload:   PresenceSyncPayload{Users: users},
	})
}

// hasUser reports whether users lists userID.
func hasUser(users []PresencePayload, userID string) bool {
	for _, u := range users {
		if u.UserID == userID {
			return true
		}
	}
	return false
}

// Leave removes a client from its room — the room is deleted, and its
// heartbeat stopped, when it empties — and broadcasts presence.leave if that
// was the user's last connection to the room.
func (h *Hub) Leave(flowID string, c *Client) {
	// Drop this connection's shared presence record (no-op single-replica)
	// BEFORE the room check below, so two connections of one user leaving at
	// once cannot each conclude the other still holds the user: whichever
	// takes h.mu second sees the other gone from the room AND — because the
	// other removed its record before taking h.mu — cannot find that record
	// in the store either.
	if h.backplane != nil {
		if h.presenceW != nil {
			// Drop any pending async upsert for this connection, then remove
			// the record through the writer, ordered after every upsert
			// enqueued before it and awaited: an in-flight heartbeat batch
			// carries this connection's record, and a delete that ran beside
			// it could be undone by that batch landing afterwards — a ghost
			// presence until the record's TTL.
			h.presenceW.cancelPending(flowID, c.connID)
			h.presenceW.delete(flowID, c.connID, presenceDeleteWait)
		} else {
			h.backplane.removePresence(flowID, c.connID)
		}
	}

	h.mu.Lock()
	lastLocal := true
	r, ok := h.rooms[flowID]
	if ok {
		delete(r.clients, c)
		for other := range r.clients {
			if other.UserID == c.UserID {
				lastLocal = false
				break
			}
		}
		if len(r.clients) == 0 {
			delete(h.rooms, flowID)
			r.stopHeartbeat()
		}
	}
	h.mu.Unlock()

	recordWSConnectionChange(-1)

	// presence.leave means "this user is gone"; peers key presence by user,
	// not connection. It used to go out on every disconnect, so closing one
	// of two tabs — or a reconnect whose old socket closed after the new one
	// joined — removed a user still in the room from every peer's list.
	// Announce it only when the user has no other connection here, nor (with
	// a backplane) a live record on any replica.
	if !lastLocal || h.userPresentInStore(flowID, c.UserID, c.connID) {
		return
	}
	h.Broadcast(flowID, c.UserID, Envelope{
		Type:      EventPresenceLeave,
		FlowID:    flowID,
		UserID:    c.UserID,
		Timestamp: time.Now(),
	}, nil)
}

// Broadcast sends env to every client in the room except exclude
// (nil = all clients). When a backplane is present the envelope is also
// published to peer replicas so their clients receive it (exclude is
// inherently local — the excluded client lives on this replica, so peers
// correctly deliver to all of their own clients).
//
// The envelope is encoded once, here, and every recipient's writePump and the
// backplane publish send those same bytes (Envelope.wire). Broadcast never
// blocks on the backplane: the publish is queued for the hub's publisher
// goroutine (see publisher), so readPump does not stop reading while Redis is
// slow.
func (h *Hub) Broadcast(flowID, _ string, env Envelope, exclude *Client) {
	h.broadcastTo(flowID, env, h.roomTargets(flowID, exclude))
}

// broadcastTo is Broadcast for a caller that has chosen the local recipients
// itself (the room heartbeat, which excludes every connection of the user it
// announces). The backplane publish goes out regardless of targets.
func (h *Hub) broadcastTo(flowID string, env Envelope, targets []*Client) {
	if len(targets) == 0 && h.pub == nil {
		return
	}
	b, err := json.Marshal(env)
	if err != nil {
		// Every recipient's writePump would have failed the same marshal and
		// skipped the envelope.
		return
	}
	env.wire = b
	for _, c := range targets {
		c.Send(env)
	}

	if h.pub != nil {
		h.pub.publish(flowID, b)
	}
}

// roomTargets snapshots the clients of a local room except exclude, so the
// caller does not hold the lock while writing.
func (h *Hub) roomTargets(flowID string, exclude *Client) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r, ok := h.rooms[flowID]
	if !ok {
		return nil
	}
	targets := make([]*Client, 0, len(r.clients))
	for c := range r.clients {
		if c != exclude {
			targets = append(targets, c)
		}
	}
	return targets
}

// deliverRemote is the backplane callback for an envelope published by a peer
// replica: deliver to this replica's local clients without re-publishing. The
// peer's bytes go out as received — the backplane has already parsed the
// message that carried them, so they are valid JSON.
func (h *Hub) deliverRemote(flowID string, envJSON []byte) {
	for _, c := range h.roomTargets(flowID, nil) {
		c.Send(Envelope{wire: envJSON})
	}
}

// joinPresence writes the joining connection's presence record and reads the
// room back — one round trip when the backplane pipelines the pair, the
// two-call form otherwise — and collapses the room's records to one payload
// per user. Returns nil without a backplane (the caller falls back to the
// local room) or when the store cannot be read or the record cannot be
// marshaled: a snapshot without the joiner proves the round trip failed.
func (h *Hub) joinPresence(flowID string, c *Client) []PresencePayload {
	if h.backplane == nil {
		return nil
	}
	c.presenceMu.Lock()
	b, err := json.Marshal(c.presenceRecord())
	c.presenceMu.Unlock()
	if err != nil {
		return nil
	}
	records := map[string][]byte{c.connID: b}
	if p, ok := h.backplane.(presencePipeliner); ok {
		return presenceFromRecords(p.writeAndListPresence(flowID, records))
	}
	h.backplane.writePresence(flowID, records)
	return h.Presence(flowID)
}

// writePresenceLocked enqueues this connection's presence record for the
// shared store, coalesced with the connection's pending upserts. The caller
// holds c.presenceMu, which spans only the in-memory snapshot — the store
// round trip runs on the hub's presence-writer goroutine (it used to run on
// the connection's read pump under this same lock: a slow store stalled the
// pump while the send buffer filled, and Send disconnected the client as
// slow). Peers see the selection live through the broadcast that follows; the
// store copy is reconciled by its SelectedAt stamp (selectionNewer) and
// rewritten by the room's heartbeat within one period, so a briefly stale
// record self-heals.
func (h *Hub) writePresenceLocked(c *Client) {
	if h.presenceW == nil {
		return
	}
	b, err := json.Marshal(c.presenceRecord())
	if err != nil {
		return
	}
	h.presenceW.enqueue(c.flowID, map[string][]byte{c.connID: b})
}

// presenceRecord is this connection's shared presence record as of now: its
// selection and a fresh expiry.
func (c *Client) presenceRecord() presenceRecord {
	sel, selAt := c.selection()
	return presenceRecord{
		V:               payloadVersion1,
		Kind:            presenceKind,
		UserID:          c.UserID,
		DisplayName:     c.DisplayName,
		SelectedBlockID: sel,
		SelectedAt:      selAt,
		ConnID:          c.connID,
		Exp:             time.Now().Add(presenceTTL).UnixNano(),
	}
}

// userPresentInStore reports whether the shared presence store still holds a
// live record for userID in flowID from a connection other than
// exceptConnID — another connection of that user, on this replica or a peer.
// Leave passes the leaving connection, whose own record it has just tried to
// remove: that record is skipped by its ConnID rather than assumed gone,
// because a failed or timed-out delete leaves it in the store, and counting
// it as "another connection" suppressed the user's only presence.leave.
// Always false without a backplane, and false when the store cannot be read,
// which falls back to announcing the leave: a user wrongly dropped is re-added
// by their next heartbeat, whereas a missed leave would keep a departed user
// listed.
func (h *Hub) userPresentInStore(flowID, userID, exceptConnID string) bool {
	if h.backplane == nil {
		return false
	}
	now := time.Now().UnixNano()
	for _, rec := range h.backplane.listPresence(flowID) {
		// Re-check expiry here rather than trusting every backplane to have
		// pruned: a crashed replica's record stops holding a user present
		// once its Exp passes (within presenceTTL of its last heartbeat).
		if rec.UserID == userID && rec.ConnID != exceptConnID && rec.Exp >= now {
			return true
		}
	}
	return false
}

// selectionNewer reports whether a connection's selection sel, set at at,
// replaces cur, set at curAt, as its user's selection. It is the one rule
// every presence reader applies — presence.sync (dedupePresence) and every
// heartbeat (userSelections) — so a peer is told the same selection for a
// user whichever of the user's connections the news comes through: the
// selection set last wins, a deselect included; a connection that never set
// one (at 0) never replaces one that did. At equal stamps — in practice only
// records predating the stamps, all 0 — a selection beats none.
func selectionNewer(sel string, at int64, cur string, curAt int64) bool {
	return at > curAt || (at == curAt && cur == "" && sel != "")
}

// presenceEntry is one connection's presence as the hub reads it: the wire
// payload plus when that connection last set its selection.
type presenceEntry struct {
	PresencePayload
	selectedAt int64
}

// Presence returns the users currently in a flow room, one entry per user. With
// a backplane this is the union across all replicas (read from the shared
// store, which each replica's clients write themselves into); otherwise it is
// the local room.
func (h *Hub) Presence(flowID string) []PresencePayload {
	if h.backplane == nil {
		return h.localPresence(flowID)
	}
	return presenceFromRecords(h.backplane.listPresence(flowID))
}

// presenceFromRecords collapses the store's per-connection records into one
// payload per user: the first entry for a user supplies the identity fields
// and selectionNewer picks the selection across all of the user's
// connections.
func presenceFromRecords(recs []presenceRecord) []PresencePayload {
	result := make([]presenceEntry, 0, len(recs))
	for _, rec := range recs {
		result = append(result, presenceEntry{
			PresencePayload: PresencePayload{
				UserID:          rec.UserID,
				DisplayName:     rec.DisplayName,
				SelectedBlockID: rec.SelectedBlockID,
			},
			selectedAt: rec.SelectedAt,
		})
	}
	return dedupePresence(result)
}

// localPresence is Presence over this replica's own connections only — nil
// when the room does not exist here.
func (h *Hub) localPresence(flowID string) []PresencePayload {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r, ok := h.rooms[flowID]
	if !ok {
		return nil
	}
	result := make([]presenceEntry, 0, len(r.clients))
	for c := range r.clients {
		sel, selAt := c.selection()
		result = append(result, presenceEntry{
			PresencePayload: PresencePayload{
				UserID:          c.UserID,
				DisplayName:     c.DisplayName,
				SelectedBlockID: sel,
			},
			selectedAt: selAt,
		})
	}
	return dedupePresence(result)
}

// dedupePresence collapses per-connection entries into one per user: both
// sources above list connections, not users, and presence.sync consumers key
// their list by userId. The first entry for a user supplies the identity
// fields; the selection is the one selectionNewer picks across all of the
// user's entries, so a user with one idle tab still shows the block their
// other tab has selected.
func dedupePresence(entries []presenceEntry) []PresencePayload {
	out := make([]PresencePayload, 0, len(entries))
	selAt := make([]int64, 0, len(entries))
	at := make(map[string]int, len(entries))
	for _, e := range entries {
		if i, seen := at[e.UserID]; seen {
			if selectionNewer(e.SelectedBlockID, e.selectedAt, out[i].SelectedBlockID, selAt[i]) {
				out[i].SelectedBlockID, selAt[i] = e.SelectedBlockID, e.selectedAt
			}
			continue
		}
		at[e.UserID] = len(out)
		out = append(out, e.PresencePayload)
		selAt = append(selAt, e.selectedAt)
	}
	return out
}

// RoomCount returns the number of active rooms (for monitoring).
func (h *Hub) RoomCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms)
}

// ClientCount returns the total number of connected clients across all rooms.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, r := range h.rooms {
		n += len(r.clients)
	}
	return n
}

// NotifyFlowChanged broadcasts a flow.changed event to every client in the
// room, signalling that the flow was saved via HTTP and they should reload.
func (h *Hub) NotifyFlowChanged(flowID string, version int) {
	h.Broadcast(flowID, "", Envelope{
		Type:      EventFlowChanged,
		FlowID:    flowID,
		Timestamp: time.Now(),
		Payload:   FlowChangedPayload{Version: version},
	}, nil)
}
