package websocket

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"pad-core/logger"
)

// presenceTTL is how long a presence entry survives without a heartbeat. It must
// exceed the client ping period (writePump pings every pingPeriod and refreshes
// presence on each tick), so a live client is never dropped while a crashed
// replica's entries expire within roughly one interval.
const presenceTTL = 2 * pingPeriod

const (
	// roomChannelPrefix + flowID is the pub/sub channel a room's broadcasts are
	// published on; every replica PSubscribes to the whole prefix.
	roomChannelPrefix = "ws:room:"
	// presenceKeyPrefix + flowID is the Redis hash holding a room's presence
	// (field = connID, value = presenceRecord JSON).
	presenceKeyPrefix = "ws:presence:"
)

// backplane fans WebSocket traffic across replicas so multi-replica deployments
// behave like one hub: room broadcasts travel over Redis pub/sub and presence
// lives in a shared per-room hash. A hub with a nil backplane skips all of this
// and stays purely in-memory (single replica / desktop) — unchanged behavior.
type backplane interface {
	// publish fans a room envelope out to peer replicas. The origin replica has
	// already delivered locally; peers deliver on receipt.
	publish(flowID string, envJSON []byte)
	// writePresence upserts a member's presence with a fresh expiry (join +
	// heartbeat + selection change all funnel here).
	writePresence(flowID, member string, record []byte)
	// removePresence drops a member on leave.
	removePresence(flowID, member string)
	// listPresence returns the non-expired presence records for a room across
	// all replicas, pruning expired members opportunistically.
	listPresence(flowID string) [][]byte
	// close stops the subscriber goroutine.
	close()
}

// presenceRecord is the cross-replica-persisted form of a client's presence:
// the wire PresencePayload fields plus an absolute expiry used for TTL sweeping
// (Redis hash fields have no native per-field TTL on the versions we target).
type presenceRecord struct {
	UserID          string `json:"userId"`
	DisplayName     string `json:"displayName,omitempty"`
	SelectedBlockID string `json:"selectedBlockId,omitempty"`
	Exp             int64  `json:"exp"` // unix nanoseconds
}

// pubMessage wraps a published envelope with its origin replica ID so a replica
// ignores the copy of its own broadcast that it receives back over pub/sub
// (it already delivered locally).
type pubMessage struct {
	Origin string          `json:"o"`
	Env    json.RawMessage `json:"e"`
}

// redisBackplane implements backplane over a single Redis client: one global
// PSubscribe for all room channels, and a hash per room for presence.
//
// Every replica receives every room's broadcasts (PSubscribe on the whole
// prefix) and no-ops when it has no local clients in that room. That keeps
// subscription management trivial (no per-room subscribe/unsubscribe races) at
// the cost of some cross-replica chatter — acceptable for a collaboration hub's
// room counts, and revisitable with per-room channels if it ever isn't.
type redisBackplane struct {
	c         *redis.Client
	replicaID string
	ctx       context.Context
	cancel    context.CancelFunc
	pubsub    *redis.PubSub
}

// newRedisBackplane starts the subscriber. onRoomMessage is invoked for each
// envelope published by a *peer* replica (self-origin messages are filtered),
// with the room's flowID and the raw envelope JSON.
func newRedisBackplane(c *redis.Client, replicaID string, onRoomMessage func(flowID string, envJSON []byte)) *redisBackplane {
	ctx, cancel := context.WithCancel(context.Background())
	b := &redisBackplane{c: c, replicaID: replicaID, ctx: ctx, cancel: cancel}
	b.pubsub = c.PSubscribe(ctx, roomChannelPrefix+"*")
	go b.consume(onRoomMessage)
	return b
}

func (b *redisBackplane) consume(onRoomMessage func(flowID string, envJSON []byte)) {
	ch := b.pubsub.Channel()
	for {
		select {
		case <-b.ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var pm pubMessage
			if err := json.Unmarshal([]byte(msg.Payload), &pm); err != nil {
				continue
			}
			if pm.Origin == b.replicaID {
				continue // our own broadcast, already delivered locally
			}
			flowID := strings.TrimPrefix(msg.Channel, roomChannelPrefix)
			onRoomMessage(flowID, pm.Env)
		}
	}
}

func (b *redisBackplane) publish(flowID string, envJSON []byte) {
	payload, err := json.Marshal(pubMessage{Origin: b.replicaID, Env: envJSON})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	if err := b.c.Publish(ctx, roomChannelPrefix+flowID, payload).Err(); err != nil {
		// Fail open: cross-replica fan-out degrades, local delivery is unaffected.
		logger.Warn("websocket backplane publish failed", "flowID", flowID, "err", err)
	}
}

func (b *redisBackplane) writePresence(flowID, member string, record []byte) {
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	key := presenceKeyPrefix + flowID
	if err := b.c.HSet(ctx, key, member, record).Err(); err != nil {
		logger.Warn("websocket backplane presence write failed", "flowID", flowID, "err", err)
		return
	}
	// Safety-net key TTL so an abandoned room hash (all members gone via crash)
	// eventually disappears even if removePresence never runs. Refreshed on
	// every write, so it never expires a room with live heartbeats.
	b.c.Expire(ctx, key, presenceTTL*2)
}

func (b *redisBackplane) removePresence(flowID, member string) {
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	if err := b.c.HDel(ctx, presenceKeyPrefix+flowID, member).Err(); err != nil {
		logger.Warn("websocket backplane presence delete failed", "flowID", flowID, "err", err)
	}
}

func (b *redisBackplane) listPresence(flowID string) [][]byte {
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	key := presenceKeyPrefix + flowID
	entries, err := b.c.HGetAll(ctx, key).Result()
	if err != nil {
		return nil
	}
	now := time.Now().UnixNano()
	var out [][]byte
	var expired []string
	for member, raw := range entries {
		var rec presenceRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Exp < now {
			expired = append(expired, member)
			continue
		}
		out = append(out, []byte(raw))
	}
	if len(expired) > 0 {
		// Best-effort prune of members whose replica died without a clean leave.
		b.c.HDel(ctx, key, expired...)
	}
	return out
}

func (b *redisBackplane) close() {
	b.cancel()
	if b.pubsub != nil {
		_ = b.pubsub.Close()
	}
}
