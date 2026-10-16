package api

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/redisx"
)

// The per-user caps of this package across the replicas (redisx.SlotCap; operations.md, "Running more than one
// replica"): 10 event streams and 3 concurrent uploads per user. Each replica keeps its own cap too; with more than one
// replica configured (PAD_MAX_REPLICAS > 1, internal/di) the count across the replicas is checked as well, so N
// replicas no longer allow N times as many. A Redis that does not answer leaves the replica's own cap (fail open).

const (
	// maxSSEConnsPerClient is the cap of event streams per client key (a user, or an address without accounts).
	maxSSEConnsPerClient = 10
)

var (
	// sseConnSlotsKey holds a user's open event streams across the replicas.
	sseConnSlotsKey = redisx.Declare(redisx.Spec{
		Name:    "sse.conn-slots",
		Kind:    redisx.KindZSet,
		Pattern: "baki:sse:conns:{user}",
		TTL:     redisx.SlotKeyTTL,
		Version: 1,
		OnDown:  redisx.OnDownOpen,
		Owner:   "internal/api",
	})
	// uploadSlotsKey holds a user's uploads in progress across the replicas.
	uploadSlotsKey = redisx.Declare(redisx.Spec{
		Name:    "flow.upload-slots",
		Kind:    redisx.KindZSet,
		Pattern: "baki:flow:uploads:{user}",
		TTL:     redisx.SlotKeyTTL,
		Version: 1,
		OnDown:  redisx.OnDownOpen,
		Owner:   "internal/api",
	})
)

// WithConnCap counts each user's event streams across the replicas on client (nil: this replica's cap only).
func WithConnCap(client *redis.Client, health *redisx.Health) EventManagerOption {
	return func(m *EventManager) {
		m.connSlots = redisx.NewSlotCap(client, health, sseConnSlotsKey, maxSSEConnsPerClient)
	}
}

// ShareUploadCap counts each user's uploads in progress across the replicas on client (nil: this replica's cap only).
// Call it before the server serves (internal/di does, at boot).
func (h *FlowHandler) ShareUploadCap(client *redis.Client, health *redisx.Health) {
	h.uploadLimiter.shared = redisx.NewSlotCap(client, health, uploadSlotsKey, maxConcurrentUploadsPerUser)
}

// uploadSlotPoll is how often an upload waiting for a slot across the replicas asks again: the replica's own cap waits
// for a slot to free (it is a channel); the shared one can only be asked.
const uploadSlotPoll = 250 * time.Millisecond

// acquireShared waits, while ctx lives, for one of userID's upload slots across the replicas.
func (l *uploadLimiter) acquireShared(ctx context.Context, userID string) (release func(), ok bool) {
	return l.shared.AcquireWait(ctx, userID, uploadSlotPoll)
}
