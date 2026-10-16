package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/redis/go-redis/v9"
	"pad-core/logger"

	"pad-analyzer/internal/redisx"
)

// The stream start across replicas (ADR 0033, addendum (close-out)). With more than one replica configured
// (PAD_MAX_REPLICAS > 1) the two checks tryStartStream makes per replica, the caller's concurrent-stream cap and the
// clientStreamId's uniqueness, are also made against Redis, so N replicas no longer allow N times as many streams nor
// two streams under one id. Each replica keeps its own checks too. Redis that does not answer leaves the local checks
// as the only ones (fail open): a single replica and a deployment without Redis never get here.

var (
	// chatStreamSlotsKey holds a caller's running streams across the replicas, one lease each (redisx.SlotCap).
	chatStreamSlotsKey = redisx.Declare(redisx.Spec{
		Name:    "chat.stream-slots",
		Kind:    redisx.KindZSet,
		Pattern: "baki:chat:streams:{scope}",
		TTL:     redisx.SlotKeyTTL,
		Version: 1,
		OnDown:  redisx.OnDownOpen,
		Owner:   "internal/service",
	})
	// chatStreamIDKey claims a client-chosen stream id for as long as a replica keeps it in its local maps: while the
	// stream runs and resumeRetention after (SET NX, shortened when the stream ends).
	chatStreamIDKey = redisx.Declare(redisx.Spec{
		Name:    "chat.stream-id",
		Kind:    redisx.KindString,
		Pattern: "baki:chat:stream-id:{stream}",
		TTL:     maxChatStreamDuration + resumeRetention,
		Version: 1,
		OnDown:  redisx.OnDownOpen,
		Owner:   "internal/service",
	})
)

// streamIDClaim is the value of chatStreamIDKey: the replica that runs the stream.
type streamIDClaim struct {
	redisx.Envelope
	Replica string `json:"replica"`
}

// streamShare is a ChatService's part of the cross-replica start; nil with one replica or without Redis.
type streamShare struct {
	c      *redis.Client
	health *redisx.Health
	origin string
	slots  *redisx.SlotCap
}

func newStreamShare(c *redis.Client, health *redisx.Health, origin string) *streamShare {
	return &streamShare{c: c, health: health, origin: origin, slots: redisx.NewSlotCap(c, health, chatStreamSlotsKey, maxConcurrentStreamsPerScope)}
}

// streamHold is what a started stream holds across the replicas, given back by abort or finish.
type streamHold struct {
	claim   string // the id claim's value; "" when none was taken (Redis did not answer)
	release func()
}

// shortenClaimScript sets a new TTL on the claim only while it still holds the value this replica wrote.
var shortenClaimScript = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("pexpire", KEYS[1], ARGV[2]) else return 0 end`)

// reserve claims ctl's stream id and one of its caller's stream slots. dup: another replica runs (or just ran) a stream
// under the id; full: the caller runs the cap's streams across the replicas. Neither leaves anything held.
func (sh *streamShare) reserve(ctl *streamCtl) (hold *streamHold, dup, full bool) {
	if sh == nil {
		return nil, false, false
	}
	hold = &streamHold{release: func() {}}
	if !sh.health.Degraded() {
		val, err := json.Marshal(streamIDClaim{Envelope: redisx.Envelope{V: 1, Kind: chatStreamIDKey.Name()}, Replica: sh.origin})
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), clusterOpTimeout)
			won, err := sh.c.SetNX(ctx, chatStreamIDKey.Key(ctl.streamID), val, chatStreamIDKey.Spec().TTL).Result()
			cancel()
			switch {
			case err != nil:
				redisx.LimitedWarn("chat_stream_id_claim", "chat: the stream id could not be claimed across the replicas; this replica's check applies", err)
			case !won:
				return nil, true, false
			default:
				hold.claim = string(val)
			}
		}
	}
	release, ok := sh.slots.Acquire(context.Background(), ctl.ownerID)
	if !ok {
		sh.dropClaim(ctl.streamID, hold.claim)
		return nil, false, true
	}
	hold.release = release
	return hold, false, false
}

// abort gives back what a start that did not happen took: the slot, and the id, which the client may send again.
func (sh *streamShare) abort(ctl *streamCtl, hold *streamHold) {
	if sh == nil || hold == nil {
		return
	}
	hold.release()
	sh.dropClaim(ctl.streamID, hold.claim)
}

// finish gives back the slot of a stream that ended and keeps its id claimed for resumeRetention, as long as the
// replica keeps it in finishedStreams.
func (sh *streamShare) finish(ctl *streamCtl, hold *streamHold) {
	if sh == nil || hold == nil {
		return
	}
	hold.release()
	if hold.claim == "" || sh.health.Degraded() {
		return // the claim expires with its TTL
	}
	ctx, cancel := context.WithTimeout(context.Background(), clusterOpTimeout)
	defer cancel()
	_ = shortenClaimScript.Run(ctx, sh.c, []string{chatStreamIDKey.Key(ctl.streamID)}, hold.claim, resumeRetention.Milliseconds()).Err()
}

func (sh *streamShare) dropClaim(streamID, claim string) {
	if claim == "" || sh.health.Degraded() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), clusterOpTimeout)
	defer cancel()
	_ = releaseScript.Run(ctx, sh.c, []string{chatStreamIDKey.Key(streamID)}, claim).Err()
}

// shareAcrossReplicas turns on the cross-replica start and the liveness key (MultiReplica). After attachCluster.
func (s *ChatService) shareAcrossReplicas(c *redis.Client, health *redisx.Health) {
	s.share = newStreamShare(c, health, s.cluster.instance)
	s.cluster.health = health
	if s.cluster.live = redisx.NewLiveness(c, s.cluster.instance); s.cluster.live != nil {
		if rs, ok := s.resume.(redisResumeStore); ok {
			rs.live = true
			s.resume = rs
		}
		// The key says "a command sent now is received", is written again the moment Redis or the subscription is
		// back, and the streams others know of stop before it can lapse while this replica runs on.
		live := s.cluster.live
		live.WhileListening(s.cluster.bus.Listening)
		live.OnStale(s.fencePublishedStreams)
		s.cluster.bus.OnSubscribed(live.Kick)
		health.OnChange(func(up bool) {
			if up {
				live.Kick()
			}
		})
	}
}

// replicaFencedMsg is the error a stream stopped by fencePublishedStreams ends with.
const replicaFencedMsg = "response stopped: this server lost its connection to the others — try again"

// fencePublishedStreams stops every stream of this replica that the others know of (ctl.published): this replica has
// not written its liveness key for so long that it may lapse, after which another replica answers a stop for such a
// stream as for a dead replica's (ok, snapshot removed). Stopping them first keeps that answer true. A stream nobody
// else knows of runs on: nobody can take it for gone. Its approvals end with it.
func (s *ChatService) fencePublishedStreams() {
	n := 0
	s.activeStreams.Range(func(_, v interface{}) bool {
		if ctl := v.(*streamCtl); ctl.published.Load() {
			ctl.cancelWithCode(ChatErrInterrupted, replicaFencedMsg)
			n++
		}
		return true
	})
	if n > 0 {
		logger.Warn("chat streams stopped: this replica could not keep its liveness key in Redis", "streams", n)
	}
}

// markPublished records that another replica can know of the stream (an approval of it is published).
func (s *ChatService) markPublished(streamID string) {
	if v, ok := s.activeStreams.Load(streamID); ok {
		v.(*streamCtl).published.Store(true)
	}
}

// --- a dead owner (the liveness key) -----------------------------------------------------------------------------

// errOwnerGone: the stream's replica did not answer and its liveness key is gone, so the stream died with it.
var errOwnerGone = errors.New("the replica running the stream is gone")

// ownerGone reports whether the replica named owner is known to be gone: it publishes its liveness (live, stamped in
// the snapshot or approval record by a replica that keeps the key) and the key has lapsed. Unknown (an older replica
// that keeps no key, Redis not answering, this replica's own view of Redis interrupted lately) is not gone.
func (cl *chatCluster) ownerGone(ctx context.Context, owner string, live bool) bool {
	if !live || cl == nil || !cl.steadyView() {
		return false
	}
	alive, err := redisx.ReplicaAlive(ctx, cl.c, owner)
	return err == nil && !alive
}

// steadyView reports whether this replica's own link to Redis has been up for a whole liveness TTL: its command
// subscriptions unbroken and Redis not degraded. Right after a Redis restart or an outage the owner's key may be missing
// only because the owner has not written it again yet (its subscription reconnects on a backoff of up to 5 s, its
// writer ticks every 5 s); within a TTL of a steady view, a live owner has.
func (cl *chatCluster) steadyView() bool {
	now := cl.now()
	if since, up := cl.bus.UpSince(); !up || now.Sub(since) < redisx.LivenessTTL {
		return false
	}
	if cl.health.Configured() {
		if since, up := cl.health.UpSince(); !up || now.Sub(since) < redisx.LivenessTTL {
			return false
		}
	}
	return true
}

// dropDeadSnapshot removes the snapshot of a stream whose replica is gone, while it still names that replica and is not
// final: a resume then answers STREAM_NOT_FOUND (the client shows the answer as interrupted) instead of a stream that
// never ends, and the next stop is refused as an unknown stream. A WATCH transaction, so a snapshot rewritten in
// between (the replica was not gone after all) is kept.
func (cl *chatCluster) dropDeadSnapshot(streamID, owner string) {
	key := resumeKey.Key(streamID)
	ctx, cancel := cl.opCtx(context.Background())
	defer cancel()
	_ = cl.c.Watch(ctx, func(tx *redis.Tx) error {
		b, err := tx.Get(ctx, key).Bytes()
		if err != nil {
			return err
		}
		var snap resumeSnapshot
		if redisx.Decode(resumeKey.Name(), resumeKey.Spec().Version, b, &snap) != redisx.ResultOK ||
			snap.Replica != owner || snap.Done || snap.Error != "" {
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Del(ctx, key)
			return nil
		})
		return err
	}, key)
}

// dropDeadApproval removes the pending record of an approval whose replica is gone, while it still names that replica.
func (cl *chatCluster) dropDeadApproval(proposalID, owner string) {
	key := chatApprovalKey.Key(proposalID)
	ctx, cancel := cl.opCtx(context.Background())
	defer cancel()
	_ = cl.c.Watch(ctx, func(tx *redis.Tx) error {
		b, err := tx.Get(ctx, key).Bytes()
		if err != nil {
			return err
		}
		var rec approvalRecord
		if redisx.Decode(chatApprovalKey.Name(), chatApprovalKey.Spec().Version, b, &rec) != redisx.ResultOK || rec.Replica != owner {
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Del(ctx, key)
			return nil
		})
		return err
	}, key)
}
