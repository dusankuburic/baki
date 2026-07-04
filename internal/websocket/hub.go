package websocket

import (
	"encoding/json"
	"sync"
	"time"

	"pad-analyzer/internal/metrics"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// maxConnsPerUser bounds how many live WebSocket connections a single user may
// hold open. Without it an authenticated account can open thousands of
// connections (each holding 2 goroutines + a 256-slot send buffer + buffers),
// exhausting memory/goroutines for the whole process. Mirrors the SSE cap in
// events.go (10) so the two real-time channels enforce the same per-user
// ceiling.
const maxConnsPerUser = 10

// Hub manages all active WebSocket rooms.
// One room exists per open flow document; clients join/leave rooms as they
// open/close flows.
type Hub struct {
	mu           sync.RWMutex
	rooms        map[string]*room // keyed by flowID
	connsPerUser map[string]int   // userID -> open connection count

	// backplane is nil for single-replica/desktop (pure in-memory). When set
	// (multi-replica, PAD_REDIS_URL), room broadcasts fan out over pub/sub and
	// presence is read from a shared store so users on other replicas are seen.
	backplane backplane
	// replicaID identifies this process so it can ignore its own broadcasts
	// echoed back over pub/sub.
	replicaID string
}

// room holds all clients subscribed to a particular flow.
type room struct {
	flowID  string
	clients map[*Client]bool
}

// NewHub creates a single-replica, in-memory Hub ready to accept connections.
func NewHub() *Hub {
	return &Hub{rooms: make(map[string]*room), connsPerUser: make(map[string]int)}
}

// NewHubWithRedis creates a Hub backed by a Redis pub/sub + presence backplane
// so presence and broadcasts are consistent across replicas. A nil client (Redis
// disabled) falls back to the in-memory NewHub — this is the multi-replica
// opt-in gated on PAD_REDIS_URL, mirroring the rate limiter / chat-resume paths.
func NewHubWithRedis(client *redis.Client) *Hub {
	h := NewHub()
	if client == nil {
		return h
	}
	h.replicaID = uuid.NewString()
	h.backplane = newRedisBackplane(client, h.replicaID, h.deliverRemote)
	return h
}

// Close releases the backplane subscriber (no-op for an in-memory hub). Call on
// shutdown.
func (h *Hub) Close() {
	if h.backplane != nil {
		h.backplane.close()
	}
}

// AcquireConn atomically reserves a WebSocket connection slot for userID,
// honoring the per-user cap (maxConnsPerUser). It returns a release callback
// (call it when the connection closes) and ok=false if the user is already at
// the cap. The release is idempotent. Enforced before the WS upgrade so a
// refused connection never allocates goroutines or buffers.
func (h *Hub) AcquireConn(userID string) (release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connsPerUser[userID] >= maxConnsPerUser {
		return func() {}, false
	}
	h.connsPerUser[userID]++
	var released bool
	return func() {
		if released {
			return
		}
		released = true
		h.mu.Lock()
		h.connsPerUser[userID]--
		if h.connsPerUser[userID] <= 0 {
			delete(h.connsPerUser, userID)
		}
		h.mu.Unlock()
	}, true
}

// Join registers a client in the room for flowID.
// The room is created on the first join.
func (h *Hub) Join(flowID string, c *Client) {
	h.mu.Lock()
	r, ok := h.rooms[flowID]
	if !ok {
		r = &room{flowID: flowID, clients: make(map[*Client]bool)}
		h.rooms[flowID] = r
	}
	r.clients[c] = true
	h.mu.Unlock()

	metrics.RecordWSConnectionChange(+1)

	// Publish this client's presence to the shared store so other replicas see
	// it (no-op single-replica).
	h.writePresence(c)

	// Notify all clients in the room (including the joiner) about the new presence.
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
}

// Leave removes a client from its room.
// The room is deleted when it becomes empty.
func (h *Hub) Leave(flowID string, c *Client) {
	h.mu.Lock()
	r, ok := h.rooms[flowID]
	if ok {
		delete(r.clients, c)
		if len(r.clients) == 0 {
			delete(h.rooms, flowID)
		}
	}
	h.mu.Unlock()

	metrics.RecordWSConnectionChange(-1)

	// Drop this client's presence from the shared store (no-op single-replica).
	if h.backplane != nil {
		h.backplane.removePresence(flowID, c.connID)
	}

	// Notify others that the user left
	h.Broadcast(flowID, c.UserID, Envelope{
		Type:      EventPresenceLeave,
		FlowID:    flowID,
		UserID:    c.UserID,
		Timestamp: time.Now(),
	}, nil)
}

// Broadcast sends env to every client in the room except exclude.
// Pass exclude = nil to broadcast to all clients. When a backplane is present
// the envelope is also published to peer replicas so their clients receive it
// (exclude is inherently local — the excluded client lives on this replica, so
// peers correctly deliver to all of their own clients).
func (h *Hub) Broadcast(flowID, _ string, env Envelope, exclude *Client) {
	h.deliverLocal(flowID, env, exclude)

	if h.backplane != nil {
		if b, err := json.Marshal(env); err == nil {
			h.backplane.publish(flowID, b)
		}
	}
}

// deliverLocal sends env to every client of a local room except exclude. It is
// the shared delivery path for both locally-originated broadcasts and envelopes
// arriving from peer replicas (which never re-publish).
func (h *Hub) deliverLocal(flowID string, env Envelope, exclude *Client) {
	h.mu.RLock()
	r, ok := h.rooms[flowID]
	if !ok {
		h.mu.RUnlock()
		return
	}
	// Snapshot the client list so we don't hold the lock while writing.
	targets := make([]*Client, 0, len(r.clients))
	for c := range r.clients {
		if c != exclude {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.Send(env)
	}
}

// deliverRemote is the backplane callback for an envelope published by a peer
// replica: deliver to this replica's local clients without re-publishing.
func (h *Hub) deliverRemote(flowID string, envJSON []byte) {
	var env Envelope
	if err := json.Unmarshal(envJSON, &env); err != nil {
		return
	}
	h.deliverLocal(flowID, env, nil)
}

// writePresence upserts a client's presence into the shared store with a fresh
// expiry. Called on join, on each heartbeat ping, and on selection change so a
// crashed replica's entries age out while live clients never do. No-op when
// there is no backplane.
func (h *Hub) writePresence(c *Client) {
	if h.backplane == nil {
		return
	}
	rec := presenceRecord{
		UserID:          c.UserID,
		DisplayName:     c.DisplayName,
		SelectedBlockID: c.GetSelectedBlockID(),
		Exp:             time.Now().Add(presenceTTL).UnixNano(),
	}
	if b, err := json.Marshal(rec); err == nil {
		h.backplane.writePresence(c.flowID, c.connID, b)
	}
}

// Presence returns the list of users currently in a flow room. With a backplane
// this is the union across all replicas (read from the shared store, which each
// replica's clients write themselves into); otherwise it is the local room.
func (h *Hub) Presence(flowID string) []PresencePayload {
	if h.backplane != nil {
		raws := h.backplane.listPresence(flowID)
		result := make([]PresencePayload, 0, len(raws))
		for _, raw := range raws {
			var rec presenceRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				continue
			}
			result = append(result, PresencePayload{
				UserID:          rec.UserID,
				DisplayName:     rec.DisplayName,
				SelectedBlockID: rec.SelectedBlockID,
			})
		}
		return result
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	r, ok := h.rooms[flowID]
	if !ok {
		return nil
	}
	result := make([]PresencePayload, 0, len(r.clients))
	for c := range r.clients {
		result = append(result, PresencePayload{
			UserID:          c.UserID,
			DisplayName:     c.DisplayName,
			SelectedBlockID: c.GetSelectedBlockID(),
		})
	}
	return result
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
