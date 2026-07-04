package service

import (
	"context"
	"encoding/json"
	"time"

	"pad-core/logger"

	"github.com/redis/go-redis/v9"
)

// resumeSnapshot is the cross-replica-persistable form of a stream's resumable
// state — the same fields ResumeStream returns, plus the owner so a resume/authz
// request that lands on a different replica can be authorized. It is written to
// the backplane so a client that reconnects to any replica can fetch the buffer.
type resumeSnapshot struct {
	Owner     string `json:"owner"`
	Text      string `json:"text"`
	Done      bool   `json:"done"`
	Error     string `json:"error"`
	TokensIn  int    `json:"tokensIn"`
	TokensOut int    `json:"tokensOut"`
}

// resumeStore is the pluggable backing for chat-stream resume. The no-op impl
// keeps today's single-replica behavior (resume served purely from the local
// activeStreams/finishedStreams maps); the Redis impl mirrors state so resume
// works after the client reconnects to a different replica.
type resumeStore interface {
	// enabled reports whether a real backplane is present. When false the
	// caller skips the mirror goroutine entirely (zero overhead single-replica).
	enabled() bool
	// Save writes/refreshes the snapshot with the given TTL. It must fail open:
	// a backplane error degrades resume, it must never break the live stream.
	Save(ctx context.Context, streamID string, snap resumeSnapshot, ttl time.Duration)
	// Load returns the snapshot when present. ok is false on miss or error.
	Load(ctx context.Context, streamID string) (snap resumeSnapshot, ok bool)
}

// noopResumeStore is the single-replica default: resume is served from the local
// in-memory maps, so there is nothing to persist or fetch.
type noopResumeStore struct{}

func (noopResumeStore) enabled() bool                                               { return false }
func (noopResumeStore) Save(context.Context, string, resumeSnapshot, time.Duration) {}
func (noopResumeStore) Load(context.Context, string) (resumeSnapshot, bool) {
	return resumeSnapshot{}, false
}

// redisResumeStore mirrors resumable stream state into Redis under a per-stream
// key with a TTL, so any replica can serve ResumeStream/OwnerOf.
type redisResumeStore struct {
	c *redis.Client
}

func (redisResumeStore) enabled() bool { return true }

func (redisResumeStore) key(streamID string) string { return "chat:resume:" + streamID }

// resumeStoreOpTimeout bounds a single backplane round-trip so mirroring/lookup
// can't stall the stream goroutine or a resume request on a slow Redis.
const resumeStoreOpTimeout = 2 * time.Second

func (r redisResumeStore) Save(ctx context.Context, streamID string, snap resumeSnapshot, ttl time.Duration) {
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeStoreOpTimeout)
	defer cancel()
	if err := r.c.Set(ctx, r.key(streamID), b, ttl).Err(); err != nil {
		// Fail open: resume degrades to same-replica-only, live streaming is
		// unaffected. Warn (not error) — this is expected during a Redis blip.
		logger.Warn("chat resume mirror failed", "streamID", streamID, "err", err)
	}
}

func (r redisResumeStore) Load(ctx context.Context, streamID string) (resumeSnapshot, bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeStoreOpTimeout)
	defer cancel()
	b, err := r.c.Get(ctx, r.key(streamID)).Bytes()
	if err != nil {
		// redis.Nil (miss) or any transport error → treat as not found.
		return resumeSnapshot{}, false
	}
	var snap resumeSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return resumeSnapshot{}, false
	}
	return snap, true
}
