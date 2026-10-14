package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"pad-core/cache"
	"pad-core/logger"

	"pad-analyzer/internal/redisx"
)

// uploadIDKey names the flow id an upload minted, per caller and content (ADR 0032). It is what lets an identical
// re-upload that lands on another replica update the same flow instead of creating a second one: a replica's memo
// (flow.upload-ids) knows only its own uploads. The value is "<flow id> <claim time>" (payload version 0). A miss is
// harmless (a second flow, as after the memo's TTL), so the key fails open and every error falls back to the memo.
var uploadIDKey = redisx.Declare(redisx.Spec{
	Name:    "flow.upload-id",
	Kind:    redisx.KindString,
	Pattern: "baki:flow:upload-id:{user}:{digest}",
	TTL:     astCacheTTL,
	OnDown:  redisx.OnDownOpen,
	Owner:   "internal/service",
})

// uploadIDRedisWait bounds one Redis call on the upload path: an upload does not wait on a slow Redis.
const uploadIDRedisWait = 500 * time.Millisecond

// uploadClaimGrace is how long a claimed id is adopted before its flow exists: the replica that claimed it is still
// between parsing the upload and saving the flow. After it, an id whose flow is missing is a deleted flow's and is
// replaced. A var so tests can shorten it.
var uploadClaimGrace = 15 * time.Second

// sharedUploadIDs is the Redis side of the upload id memo.
type sharedUploadIDs struct{ client *redis.Client }

// WithUploadIDs shares the minted flow ids of uploads across replicas through client (nil: each replica remembers
// its own, as on one replica).
func WithUploadIDs(client *redis.Client) FlowOption {
	return func(s *FlowService) {
		if client != nil {
			s.uploadShared = &sharedUploadIDs{client: client}
		}
	}
}

// rememberedUploadID is the flow id an earlier identical upload minted: the shared one when there is a Redis (it
// sees every replica's), else this replica's memo. "" when none is known. inFlight says the id was claimed within
// uploadClaimGrace, so its flow may not be saved yet.
func (s *FlowService) rememberedUploadID(ctx context.Context, memo cache.Key, callerID, digest string) (id string, inFlight bool) {
	if s.uploadShared != nil {
		if id, claimed, ok := s.uploadShared.get(ctx, callerID, digest); ok {
			return id, time.Since(claimed) < uploadClaimGrace
		}
	}
	id, _ = s.uploadIDs.Get(memo)
	return id, false
}

// rememberUploadID records id as the flow id of the upload and returns the id to use. Without Redis it is the
// memo's Set. With one, a fresh upload claims the id (the first of two replicas parsing the same bytes wins, the
// other adopts the winner's id), and an id that was just reused, or that replaces a deleted flow's, overwrites.
func (s *FlowService) rememberUploadID(ctx context.Context, memo cache.Key, callerID, digest, id string, overwrite bool) string {
	if s.uploadShared != nil {
		id = s.uploadShared.claim(ctx, callerID, digest, id, overwrite)
	}
	s.uploadIDs.Set(memo, id, 1, 0)
	return id
}

// get reads the claim: the value is "<flow id> <claim time, unix seconds>".
func (u *sharedUploadIDs) get(ctx context.Context, callerID, digest string) (id string, claimed time.Time, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, uploadIDRedisWait)
	defer cancel()
	val, err := u.client.Get(ctx, uploadIDKey.Key(callerID, digest)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			logger.Debug("upload id lookup failed; using this replica's memo", "class", redisErrorClass(err))
		}
		return "", time.Time{}, false
	}
	id, stamp, _ := strings.Cut(val, " ")
	if secs, err := strconv.ParseInt(stamp, 10, 64); err == nil {
		claimed = time.Unix(secs, 0)
	} // a value without a stamp is long claimed
	return id, claimed, id != ""
}

func (u *sharedUploadIDs) claim(ctx context.Context, callerID, digest, id string, overwrite bool) string {
	ctx, cancel := context.WithTimeout(ctx, uploadIDRedisWait)
	defer cancel()
	key := uploadIDKey.Key(callerID, digest)
	val := id + " " + strconv.FormatInt(time.Now().Unix(), 10)
	if overwrite {
		if err := u.client.Set(ctx, key, val, astCacheTTL).Err(); err != nil {
			logger.Debug("upload id store failed; this replica's memo keeps it", "class", redisErrorClass(err))
		}
		return id
	}
	won, err := u.client.SetNX(ctx, key, val, astCacheTTL).Result()
	if err != nil {
		logger.Debug("upload id claim failed; this replica's memo keeps it", "class", redisErrorClass(err))
		return id
	}
	if won {
		return id
	}
	// Another replica claimed it between our lookup and now: use its id (it saves that flow next).
	if other, err := u.client.Get(ctx, key).Result(); err == nil {
		if otherID, _, _ := strings.Cut(other, " "); otherID != "" {
			return otherID
		}
	}
	return id
}

// redisErrorClass is a short class for a log line; the error text can carry the Redis address.
func redisErrorClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}
