package websocket

import (
	"sync"
	"time"

	"pad-analyzer/internal/metrics"
)

// Hub manages all active WebSocket rooms.
// One room exists per open flow document; clients join/leave rooms as they
// open/close flows.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]*room // keyed by flowID
}

// room holds all clients subscribed to a particular flow.
type room struct {
	flowID  string
	clients map[*Client]bool
}

// NewHub creates a Hub ready to accept connections.
func NewHub() *Hub {
	return &Hub{rooms: make(map[string]*room)}
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

	// Notify others that the user left
	h.Broadcast(flowID, c.UserID, Envelope{
		Type:      EventPresenceLeave,
		FlowID:    flowID,
		UserID:    c.UserID,
		Timestamp: time.Now(),
	}, nil)
}

// Broadcast sends env to every client in the room except exclude.
// Pass exclude = nil to broadcast to all clients.
func (h *Hub) Broadcast(flowID, _ string, env Envelope, exclude *Client) {
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

// Presence returns the list of users currently in a flow room.
func (h *Hub) Presence(flowID string) []PresencePayload {
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
			SelectedBlockID: c.SelectedBlockID,
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
