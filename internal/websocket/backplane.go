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
// exceed the heartbeat period (each room's heartbeat re-writes every local
// connection's record once per pingPeriod), so a live client is never dropped
// while a crashed replica's entries expire within roughly one interval.
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
// lives in a shared per-room hash. A hub with a nil backplane skips all of
// this and stays purely in-memory (single replica / desktop).
type backplane interface {
	// publish fans a room envelope out to peer replicas; the origin replica has
	// already delivered locally. May block for a Redis round trip: called from
	// the hub's publisher goroutine only (Hub.publisher), never from a pump.
	publish(flowID string, envJSON []byte)
	// writePresence upserts the presence of every member in records (member →
	// record JSON) with a fresh expiry, in one round trip: a join or selection
	// change writes one record, a room heartbeat writes all local connections.
	writePresence(flowID string, records map[string][]byte)
	// removePresence drops a member on leave.
	removePresence(flowID, member string)
	// listPresence returns the non-expired presence records for a room across
	// all replicas, decoded, pruning expired members opportunistically; nil
	// when the store cannot be read.
	listPresence(flowID string) []presenceRecord
	// close stops the subscriber goroutine.
	close()
}

// presenceRecord is the cross-replica-persisted form of a client's presence:
// the wire PresencePayload fields plus an absolute expiry used for TTL sweeping
// (Redis hash fields have no native per-field TTL on the versions we target).
// ConnID and SelectedAt never reach clients — Hub.Presence copies the wire
// fields one by one. A record from a replica that predates them has both zero,
// which the readers treat as before: an empty ConnID matches no live
// connection, a zero SelectedAt orders no selection before another.
type presenceRecord struct {
	UserID          string `json:"userId"`
	DisplayName     string `json:"displayName,omitempty"`
	SelectedBlockID string `json:"selectedBlockId,omitempty"`
	// SelectedAt is when this connection last set its selection (unix
	// nanoseconds, 0 = never), so a user's connections on different replicas
	// agree on which selection is the user's (see selectionNewer).
	SelectedAt int64 `json:"selAt,omitempty"`
	// ConnID is the hash field this record is stored under, repeated in the
	// value because listPresence returns values only: Leave must tell its own
	// record — still there when the delete failed — from another's.
	ConnID string `json:"connId,omitempty"`
	Exp    int64  `json:"exp"` // unix nanoseconds
}

// pubMessage wraps a published envelope with its origin replica ID so a replica
// ignores its own broadcast echoed back over pub/sub (already delivered locally).
type pubMessage struct {
	Origin string          `json:"o"`
	Env    json.RawMessage `json:"e"`
}

// redisBackplane implements backplane over a single Redis client: one global
// PSubscribe for all room channels, and a hash per room for presence.
//
// Every replica receives every room's broadcasts (PSubscribe on the whole
// prefix) and no-ops when it has no local clients in that room — subscription
// management stays trivial (no per-room subscribe/unsubscribe races) at the
// cost of some cross-replica chatter, acceptable for a collaboration hub's
// room counts.
type redisBackplane struct {
	c         *redis.Client
	replicaID string
	ctx       context.Context
	cancel    context.CancelFunc
	pubsub    *redis.PubSub
}

// presencePipeliner is the optional capability of a backplane that can write
// presence records and read the room back in ONE round trip — the joiner's own
// record is then in the snapshot it reads (the pipeline's HGETALL runs after
// its HSET) at half the two-call latency. Backplanes without it keep the
// two-call form.
type presencePipeliner interface {
	writeAndListPresence(flowID string, records map[string][]byte) []presenceRecord
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

func (b *redisBackplane) writePresence(flowID string, records map[string][]byte) {
	if len(records) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	key := presenceKeyPrefix + flowID
	fields := make([]any, 0, 2*len(records))
	for member, record := range records {
		fields = append(fields, member, record)
	}
	// One round trip for everything: a room heartbeat writes every local
	// connection's record in a single HSET, and HSET and EXPIRE used to wait
	// out a round trip each. The write still completes before this returns,
	// which the callers' presenceMu ordering relies on. EXPIRE goes out even
	// when HSET fails; refreshing the TTL of whatever the hash holds is
	// harmless.
	if _, err := b.c.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, key, fields...)
		// Safety-net key TTL so an abandoned room hash (all members gone via
		// crash) eventually disappears even if removePresence never runs;
		// refreshed on every write, so it never expires a room with live
		// heartbeats.
		p.Expire(ctx, key, presenceTTL*2)
		return nil
	}); err != nil {
		logger.Warn("websocket backplane presence write failed", "flowID", flowID, "err", err)
	}
}

func (b *redisBackplane) removePresence(flowID, member string) {
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	if err := b.c.HDel(ctx, presenceKeyPrefix+flowID, member).Err(); err != nil {
		logger.Warn("websocket backplane presence delete failed", "flowID", flowID, "err", err)
	}
}

func (b *redisBackplane) listPresence(flowID string) []presenceRecord {
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	key := presenceKeyPrefix + flowID
	entries, err := b.c.HGetAll(ctx, key).Result()
	if err != nil {
		return nil
	}
	return b.decodePresence(ctx, key, entries)
}

// writeAndListPresence pipelines the HSET+EXPIRE of writePresence with the
// HGETALL of listPresence: one round trip for the join path's
// write-then-read.
func (b *redisBackplane) writeAndListPresence(flowID string, records map[string][]byte) []presenceRecord {
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	key := presenceKeyPrefix + flowID
	fields := make([]any, 0, 2*len(records))
	for member, record := range records {
		fields = append(fields, member, record)
	}
	var hgetall *redis.MapStringStringCmd
	_, err := b.c.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, key, fields...)
		p.Expire(ctx, key, presenceTTL*2)
		hgetall = p.HGetAll(ctx, key)
		return nil
	})
	if err != nil {
		return nil
	}
	return b.decodePresence(ctx, key, hgetall.Val())
}

// decodePresence converts an HGETALL payload to its live records, pruning
// members whose record is corrupt or whose replica died without a clean
// leave (expired past the record TTL).
func (b *redisBackplane) decodePresence(ctx context.Context, key string, entries map[string]string) []presenceRecord {
	now := time.Now().UnixNano()
	out := make([]presenceRecord, 0, len(entries))
	var expired []string
	for member, raw := range entries {
		var rec presenceRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Exp < now {
			expired = append(expired, member)
			continue
		}
		out = append(out, rec)
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
