package interfaces

import (
	"context"
	"errors"
	"time"
)

// FlowSnapshotTTL is how long a flow's undo snapshot is offered: the ring is a session affordance ("that fix made it
// worse"), and explicit history is flow_versions. Reads hide older rows at once; the retention sweep deletes them.
const FlowSnapshotTTL = 24 * time.Hour

var (
	// ErrFlowSnapshotTooLarge: the snapshot alone exceeds FlowSnapshotLimits.MaxBytes (or UserMaxBytes), so keeping it
	// would evict every older one and then itself. Nothing was stored; the older snapshots stay valid restore points.
	ErrFlowSnapshotTooLarge = errors.New("flow snapshot exceeds the per-flow byte bound")
	// ErrFlowSnapshotUnstorable: the text holds a NUL byte, which neither a TEXT nor a JSONB value can carry.
	ErrFlowSnapshotUnstorable = errors.New("flow snapshot holds a NUL byte")
)

// FlowSnapshot is one pre-mutation state of a cloud flow's source: the text of a single-file flow in Source, the
// per-file texts of a folder flow in Files.
type FlowSnapshot struct {
	ID     string
	FlowID string
	Label  string
	// UserID is who made the mutation the snapshot precedes, the identity the per-user bound counts against; "" for
	// none (a background job, local mode), which only the per-flow bound covers. Written, never read back.
	UserID string
	// CreatedAt is kept as given; the zero value is stamped with the database clock.
	CreatedAt time.Time
	// Bytes is the display size (the member texts); the bound counts the stored size, member names included.
	Bytes int64
	// Source and Files are empty in a list: ListFlowSnapshots returns the metadata only.
	Source string
	Files  map[string]string
}

// FlowSnapshotLimits are the bounds one push enforces on its flow's ring.
type FlowSnapshotLimits struct {
	// Keep is the newest snapshots kept; older ones are deleted by the push.
	Keep int
	// MaxBytes bounds the stored size of the flow's whole ring (newest first, older ones deleted until it holds) and
	// of any one snapshot.
	MaxBytes int64
	// UserKeep and UserMaxBytes bound the snapshots of FlowSnapshot.UserID across every flow (newest first by push
	// order, older ones deleted by the same push), so editing many flows cannot multiply the per-flow bound. A
	// snapshot alone over UserMaxBytes is refused like one over MaxBytes. Zero: no per-user bound.
	UserKeep     int
	UserMaxBytes int64
}

// FlowSnapshotStore is the cloud undo ring (migration 0045): rows keyed by the flow, visible to whoever may open it,
// gone with it. The in-memory ring of the desktop is the service's own.
type FlowSnapshotStore interface {
	// PushFlowSnapshot stores s as the flow's newest snapshot, then deletes the flow's and s.UserID's older ones
	// beyond lim (the user's only among the snapshots the caller can see), in the
	// caller's transaction when the request has one, so it never outlives a rolled-back mutation. Failing alone it
	// leaves that transaction usable. ErrNotFound when the flow does not exist or is not visible.
	PushFlowSnapshot(ctx context.Context, s *FlowSnapshot, lim FlowSnapshotLimits) error
	// ListFlowSnapshots returns the flow's snapshots within FlowSnapshotTTL, oldest first, without their text; an
	// empty non-nil slice for none (also for a flow the caller cannot see).
	ListFlowSnapshots(ctx context.Context, flowID string) ([]*FlowSnapshot, error)
	// LoadFlowSnapshot returns the snapshot with its text. ErrNotFound unless id is a live snapshot of flowID, so an
	// id taken from another flow restores nothing.
	LoadFlowSnapshot(ctx context.Context, flowID, id string) (*FlowSnapshot, error)
}

// StoredBytes is what the byte bound counts for s: the source and every member's name and text.
func (s *FlowSnapshot) StoredBytes() int64 {
	n := int64(len(s.Source))
	for name, text := range s.Files {
		n += int64(len(name) + len(text))
	}
	return n
}
