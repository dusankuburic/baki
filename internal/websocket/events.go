package websocket

import "time"

// EventType identifies the kind of collaboration event.
type EventType string

const (
	// Presence events
	EventPresenceJoin  EventType = "presence.join"
	EventPresenceLeave EventType = "presence.leave"
	EventPresenceUpdate EventType = "presence.update"

	// Cursor / selection events
	EventCursorMove EventType = "cursor.move"

	// Document edit events
	EventBlockUpdate EventType = "block.update"
	EventBlockCreate EventType = "block.create"
	EventBlockDelete EventType = "block.delete"

	// Server-pushed notification that a flow was saved via HTTP and all
	// viewers should reload to pick up the new content + version.
	EventFlowChanged EventType = "flow.changed"

	// System events
	EventError EventType = "error"
	EventPing  EventType = "ping"
	EventPong  EventType = "pong"
)

// Envelope is the outer wrapper for every WebSocket message.
type Envelope struct {
	Type      EventType `json:"type"`
	FlowID    string    `json:"flowId"`
	UserID    string    `json:"userId,omitempty"`
	Timestamp time.Time `json:"ts"`
	Payload   any       `json:"payload,omitempty"`
}

// PresencePayload carries information about a user's current state in a flow.
type PresencePayload struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName,omitempty"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
	// SelectedBlockID is the block currently focused by the user.
	SelectedBlockID string `json:"selectedBlockId,omitempty"`
}

// CursorPayload carries a user's cursor position within a flow.
type CursorPayload struct {
	UserID  string `json:"userId"`
	BlockID string `json:"blockId"`
	Offset  int    `json:"offset"`
}

// BlockPayload carries a partial or full block update.
type BlockPayload struct {
	BlockID    string         `json:"blockId"`
	SubflowID  string         `json:"subflowId"`
	Properties map[string]any `json:"properties,omitempty"`
	// Version is the optimistic-concurrency version number.
	Version int `json:"version"`
}

// ErrorPayload is sent to a single client when a request cannot be fulfilled.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// FlowChangedPayload is broadcast to all clients in a room after a flow is
// saved via HTTP, so they can reload the content and sync the new version.
type FlowChangedPayload struct {
	Version int `json:"version"`
}
