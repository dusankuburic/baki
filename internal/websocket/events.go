package websocket

import (
	"encoding/json"
	"time"
)

// EventType identifies the kind of collaboration event.
type EventType string

const (
	// Presence events
	EventPresenceJoin   EventType = "presence.join"
	EventPresenceLeave  EventType = "presence.leave"
	EventPresenceUpdate EventType = "presence.update"
	// EventPresenceSync is sent to the joining connection only, right after its
	// own presence.join: a snapshot of everyone already in the room. Without it
	// a client that joined (or reconnected into) a room in use saw only users
	// who joined after it — presence.join is broadcast once, at the moment each
	// user arrives.
	EventPresenceSync EventType = "presence.sync"

	// Block selection travels as presence.update{selectedBlockId}; there is
	// no separate cursor event (a "cursor.move" type once existed here and in
	// the client protocol mirror, declared on both sides and sent by neither).

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

	// wire, when set, is the envelope's encoded form, which writePump sends
	// as is instead of marshalling the fields above. Hub.Broadcast encodes
	// once and hands every recipient (and the backplane) the same bytes, and
	// an envelope from a peer replica arrives as bytes with no fields set at
	// all. Shared by every recipient, so it is never modified.
	wire []byte
}

// inboundEnvelope is how readPump decodes a client frame: Envelope's fields,
// but with the payload kept as raw JSON, so a relayed payload (block.update
// and friends, up to the 64 KB frame limit) is re-encoded verbatim instead of
// built into a map[string]any and serialized back out of it.
type inboundEnvelope struct {
	Type      EventType       `json:"type"`
	FlowID    string          `json:"flowId"`
	UserID    string          `json:"userId,omitempty"`
	Timestamp time.Time       `json:"ts"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// PresencePayload carries information about a user's current state in a flow.
type PresencePayload struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName,omitempty"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
	// SelectedBlockID is the block currently focused by the user.
	SelectedBlockID string `json:"selectedBlockId,omitempty"`
}

// presenceUpdateOut is the payload of every presence.update the server sends —
// a selection change and each heartbeat. It differs from PresencePayload only
// in that selectedBlockId is always present: with omitempty a deselect went
// out with no selectedBlockId key at all, and a client that merges the payload
// into its entry kept the old block. "" clears it for that merge and the
// current client.
type presenceUpdateOut struct {
	UserID          string `json:"userId"`
	DisplayName     string `json:"displayName,omitempty"`
	SelectedBlockID string `json:"selectedBlockId"`
}

// maxSelectedBlockIDLen caps, in bytes, the selectedBlockId a client may set
// with presence.update. The server stores the id and repeats it to the whole
// room (and into Redis) on every heartbeat, so an uncapped id — anything up
// to the 64 KB frame limit — would be re-sent for as long as the sender stays
// connected. 128 matches the flowId ceiling (flowIDPattern).
const maxSelectedBlockIDLen = 128

// PresenceSyncPayload is the presence.sync snapshot: one entry per user
// currently in the room (deduplicated by userId, the joiner included).
type PresenceSyncPayload struct {
	Users []PresencePayload `json:"users"`
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
