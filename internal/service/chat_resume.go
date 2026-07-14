package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"pad-core/logger"

	"github.com/redis/go-redis/v9"
)

// resumeRetention is how long a finished stream's buffer is kept so a client
// that was disconnected when the stream ended can still fetch the final text
// (and done/error) on reconnect via ResumeStream.
const resumeRetention = 2 * time.Minute

// resumeMirrorInterval is how often the growing stream buffer is snapshotted to
// the resume backplane while streaming. It bounds Redis writes to ~1/sec/stream
// (the live delta stream is unaffected — this only backs cross-replica resume).
const resumeMirrorInterval = time.Second

// subscriberCheckInterval is how often the stream watchdog verifies the
// stream's owner still has a live SSE connection; subscriberMissLimit is how
// many consecutive failed checks cancel the stream. Two 15s ticks give a
// closed-then-reopened tab (and the SSE client's reconnect backoff) ~30s of
// grace before an abandoned stream stops billing provider tokens.
const (
	subscriberCheckInterval = 15 * time.Second
	subscriberMissLimit     = 2
)

// streamIdleTimeout cancels a stream whose provider has gone silent — no
// chunk of any kind — for this long. A healthy model emits at least metadata
// every few seconds; 90s of nothing means the upstream hung, and waiting out
// the full wall-clock cap would just stall the user silently.
const streamIdleTimeout = 90 * time.Second

// ResumeResult is the buffered state of a stream returned to a reconnecting client.
type ResumeResult struct {
	Text      string `json:"text"`
	Done      bool   `json:"done"`
	Error     string `json:"error"`
	TokensIn  int    `json:"tokensIn"`
	TokensOut int    `json:"tokensOut"`
}

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

// watchdogTick returns the subscriber-check interval, honouring the test
// override so watchdog tests don't sleep for real 15s ticks.
func (s *ChatService) watchdogTick() time.Duration {
	if s.watchdogInterval > 0 {
		return s.watchdogInterval
	}
	return subscriberCheckInterval
}

// idleLimit returns the provider-idle timeout, honouring the test override.
func (s *ChatService) idleLimit() time.Duration {
	if s.idleTimeout > 0 {
		return s.idleTimeout
	}
	return streamIdleTimeout
}

// watchStream is the stream watchdog. Two independent guards share one ticker:
//
//   - Idle: the provider has emitted nothing for idleLimit — the upstream is
//     hung, and waiting out the wall-clock cap would just stall the user
//     silently. Active from stream start (a provider can hang before /begin).
//   - Subscriber: a closed tab can't send /cancel, and the stream context is
//     deliberately detached from the request, so once the client has begun,
//     cancel when the owner has had no SSE connection for subscriberMissLimit
//     consecutive checks (~30s grace covers the SSE reconnect backoff).
//
// Exits with the stream via ctx, which the worker's deferred cleanup cancels.
func (s *ChatService) watchStream(ctx context.Context, streamID, scope string, ctl *streamCtl) {
	ticker := time.NewTicker(s.watchdogTick())
	defer ticker.Stop()
	begun := false
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if idle := time.Since(time.Unix(0, ctl.lastActivity.Load())); idle > s.idleLimit() {
				logger.Info("chat stream cancelled: provider idle", "streamID", streamID, "idle", idle)
				ctl.cancelWithReason("response stopped: the AI provider stopped responding")
				return
			}
			if !begun {
				select {
				case <-ctl.started:
					begun = true
				default:
					continue
				}
			}
			if s.notifier.HasSubscriber(scope) {
				misses = 0
				continue
			}
			misses++
			if misses >= subscriberMissLimit {
				logger.Info("chat stream cancelled: client SSE connection gone", "streamID", streamID)
				ctl.cancelWithReason("response stopped: you were disconnected while it was generating")
				return
			}
		}
	}
}

// mirrorStream periodically snapshots ctl to the resume backplane until the
// stream's context ends (completion, cancel, or timeout), then writes a final
// snapshot with the short resumeRetention TTL — matching the local
// finishedStreams grace window so both expire together. Only run when the
// backplane is enabled.
func (s *ChatService) mirrorStream(ctx context.Context, streamID string, ctl *streamCtl) {
	t := time.NewTicker(resumeMirrorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Detached context: the stream ctx is already done, but the final
			// buffer must still be published for reconnecting clients.
			s.resumeBackplane().Save(context.WithoutCancel(ctx), streamID, ctl.snapshot(), resumeRetention)
			return
		case <-t.C:
			s.resumeBackplane().Save(ctx, streamID, ctl.snapshot(), maxChatStreamDuration+resumeRetention)
		}
	}
}

// sliceFrom returns s[from:] clamped to the string bounds. from beyond the end
// returns "" (a delta resume for an offset the client already has).
func sliceFrom(s string, from int) string {
	if from <= 0 {
		return s
	}
	if from >= len(s) {
		return ""
	}
	return s[from:]
}

// BeginStream unblocks the stream goroutine so it may start emitting. When the
// stream has already finished — typically a pre-stream failure (bad provider,
// budget exceeded) whose error event was emitted before the client's SSE
// subscription existed — the final buffered state is returned instead so the
// caller can deliver it directly. nil means the stream is live (or unknown)
// and events arrive over SSE.
func (s *ChatService) BeginStream(ctx context.Context, streamID string) *ResumeResult {
	if val, ok := s.activeStreams.Load(streamID); ok {
		ctl := val.(*streamCtl)
		ctl.startOnce.Do(func() { close(ctl.started) })
		// A failed-fast stream may still be in the active set for an instant;
		// its error was emitted before this call (pre-subscription, so lost) —
		// hand it back synchronously. A snapshot without done/error means any
		// later terminal event is emitted after this request arrived, i.e.
		// after the client subscribed, so SSE delivery covers it.
		if snap := ctl.snapshot(); snap.Done || snap.Error != "" {
			return &ResumeResult{Text: snap.Text, Done: snap.Done, Error: snap.Error, TokensIn: snap.TokensIn, TokensOut: snap.TokensOut}
		}
		return nil
	}
	if res, err := s.ResumeStream(ctx, streamID, 0); err == nil && (res.Done || res.Error != "") {
		return res
	}
	return nil
}

func (s *ChatService) CancelStream(streamID string) {
	if val, ok := s.activeStreams.Load(streamID); ok {
		ctl := val.(*streamCtl)
		ctl.cancelWithReason("response stopped")
	}
}

func (s *ChatService) CancelAll() {
	s.activeStreams.Range(func(key, value interface{}) bool {
		ctl := value.(*streamCtl)
		ctl.cancelWithReason("response stopped: the server is shutting down")
		return true
	})
}

// OwnerOf returns the scope/caller that created the given stream, or "" if the
// stream is unknown. It checks active and recently-finished streams locally,
// then falls back to the resume backplane so an authz check on a replica that
// did not create the stream still resolves the owner (multi-replica).
func (s *ChatService) OwnerOf(ctx context.Context, streamID string) string {
	if val, ok := s.activeStreams.Load(streamID); ok {
		return val.(*streamCtl).ownerID
	}
	if val, ok := s.finishedStreams.Load(streamID); ok {
		return val.(*streamCtl).ownerID
	}
	if snap, ok := s.resumeBackplane().Load(ctx, streamID); ok {
		return snap.Owner
	}
	return ""
}

// ResumeStream returns a stream's buffered state. It prefers the live local
// stream; when the stream ran on a different replica it falls back to the shared
// backplane so the client can still resume after reconnecting elsewhere.
//
// The `from` offset (bytes) enables delta resume for reconnects: a client whose
// accumulated text is a clean prefix of the buffer sends its length and receives
// only the tail, avoiding a full-buffer re-fetch + re-parse over flaky links.
// from is clamped to [0, len(buffer)]; a chunk-count-mismatch (possible mid-
// stream gaps) should pass from=0 for a full authoritative replace.
func (s *ChatService) ResumeStream(ctx context.Context, streamID string, from int) (*ResumeResult, error) {
	if from < 0 {
		from = 0
	}
	val, ok := s.activeStreams.Load(streamID)
	if !ok {
		val, ok = s.finishedStreams.Load(streamID)
	}
	if ok {
		ctl := val.(*streamCtl)
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		return &ResumeResult{
			Text:      sliceFrom(ctl.buffer.String(), from),
			Done:      ctl.done,
			Error:     ctl.errMsg,
			TokensIn:  ctl.tokensIn,
			TokensOut: ctl.tokensOut,
		}, nil
	}
	if snap, ok := s.resumeBackplane().Load(ctx, streamID); ok {
		return &ResumeResult{
			Text:      sliceFrom(snap.Text, from),
			Done:      snap.Done,
			Error:     snap.Error,
			TokensIn:  snap.TokensIn,
			TokensOut: snap.TokensOut,
		}, nil
	}
	return nil, fmt.Errorf("stream not found or already completed")
}
