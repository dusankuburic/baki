package websocket

import (
	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/redisx"
)

// connSlotsKey holds a user's open sockets across the replicas (redisx.SlotCap): with more than one replica configured
// (PAD_MAX_REPLICAS > 1, internal/di) the per-user cap counts every replica's sockets, so N replicas no longer allow
// N times maxConnsPerUser. The hub keeps its own count too; a Redis that does not answer leaves only that (fail open).
var connSlotsKey = redisx.Declare(redisx.Spec{
	Name:    "ws.conn-slots",
	Kind:    redisx.KindZSet,
	Pattern: "baki:ws:conns:{user}",
	TTL:     redisx.SlotKeyTTL,
	Version: 1,
	OnDown:  redisx.OnDownOpen,
	Owner:   "internal/websocket",
})

// ShareConnCap counts each user's sockets across the replicas on client (nil: this hub's cap only). Call it before the
// server serves (internal/di does, at boot).
func (h *Hub) ShareConnCap(client *redis.Client, health *redisx.Health) {
	h.connSlots = redisx.NewSlotCap(client, health, connSlotsKey, maxConnsPerUser)
}
