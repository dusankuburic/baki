package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"pad-core/logger"
	"pad-core/models"

	"github.com/redis/go-redis/v9"
)

// resumeRetention is how long a finished stream's buffer is kept so a client disconnected when the stream ended
// can still fetch the final text on reconnect.
const resumeRetention = 2 * time.Minute

// resumeMirrorInterval is how often the growing buffer is snapshotted to the resume backplane: bounds Redis
// writes to ~1/sec/stream; a tick with nothing new writes nothing (mirrorStream). A var for tests.
var resumeMirrorInterval = time.Second

// subscriberCheckInterval is how often the watchdog verifies the owner still has a live SSE connection;
// subscriberMissLimit consecutive failures cancel. Two 15s ticks give a reopened tab ~30s of grace.
const (
	subscriberCheckInterval = 15 * time.Second
	subscriberMissLimit     = 2
)

// streamIdleTimeout cancels a stream whose provider has gone silent for this long: a healthy model emits at
// least metadata every few seconds, so 90s of nothing means the upstream hung.
const streamIdleTimeout = 90 * time.Second

// hostedStepIdleTimeout is the idle limit while a provider-hosted tool step is open (a Foundry agent's MCP call,
// capped at 100s by Azure, or code-interpreter run): it streams nothing until it finishes, but stays below
// maxChatStreamDuration so a hung step is caught.
const hostedStepIdleTimeout = 5 * time.Minute

// ResumeResult is the buffered state of a stream returned to a reconnecting client; Events replays the agentic
// journal (tool results, fix proposals/decisions) so the client rebuilds its tool trail and pending approvals.
type ResumeResult struct {
	Text  string `json:"text"`
	Done  bool   `json:"done"`
	Error string `json:"error"`
	// ErrorCode is Error's machine-readable class (see chat_errors.go).
	ErrorCode string `json:"errorCode,omitempty"`
	// ErrorShared marks a failure on the org's shared credential — the error bubble offers Organizations, not Accounts.
	ErrorShared bool `json:"errorShared,omitempty"`
	TokensIn    int  `json:"tokensIn"`
	TokensOut   int  `json:"tokensOut"`
	// ServedModel is the model that answered (alias providers) plus the USD/1M rates the turn was billed at.
	ServedModel    string         `json:"servedModel,omitempty"`
	InputCostPerM  float64        `json:"inputCostPerM,omitempty"`
	OutputCostPerM float64        `json:"outputCostPerM,omitempty"`
	Events         []journalEvent `json:"events,omitempty"`
}

// resumeSnapshot is the cross-replica-persistable form of a stream's resumable state, plus the owner so a
// resume/authz request landing on a different replica can be authorized.
type resumeSnapshot struct {
	Owner          string         `json:"owner"`
	Text           string         `json:"text"`
	Done           bool           `json:"done"`
	Error          string         `json:"error"`
	ErrorCode      string         `json:"errorCode,omitempty"`
	ErrorShared    bool           `json:"errorShared,omitempty"`
	TokensIn       int            `json:"tokensIn"`
	TokensOut      int            `json:"tokensOut"`
	ServedModel    string         `json:"servedModel,omitempty"`
	InputCostPerM  float64        `json:"inputCostPerM,omitempty"`
	OutputCostPerM float64        `json:"outputCostPerM,omitempty"`
	Events         []journalEvent `json:"events,omitempty"`
}

// resumeStore is the pluggable backing for chat-stream resume: the no-op keeps single-replica behavior (local
// maps only); the Redis impl mirrors state so resume works after reconnecting to a different replica.
type resumeStore interface {
	// enabled reports whether a real backplane is present (false skips the mirror goroutine entirely).
	enabled() bool
	// Save writes/refreshes the snapshot with the given TTL; it must fail open — a backplane error degrades
	// resume, it never breaks the live stream.
	Save(ctx context.Context, streamID string, snap resumeSnapshot, ttl time.Duration) bool
	// Load returns the snapshot when present. ok is false on miss or error.
	Load(ctx context.Context, streamID string) (snap resumeSnapshot, ok bool)
}

// noopResumeStore is the single-replica default: resume is served from the local in-memory maps.
type noopResumeStore struct{}

func (noopResumeStore) enabled() bool                                                    { return false }
func (noopResumeStore) Save(context.Context, string, resumeSnapshot, time.Duration) bool { return true }
func (noopResumeStore) Load(context.Context, string) (resumeSnapshot, bool) {
	return resumeSnapshot{}, false
}

// redisResumeStore mirrors resumable stream state into Redis under a per-stream key with a TTL.
type redisResumeStore struct {
	c *redis.Client
}

func (redisResumeStore) enabled() bool { return true }

func (redisResumeStore) key(streamID string) string { return "chat:resume:" + streamID }

// resumeStoreOpTimeout bounds a single backplane round-trip so it can't stall the stream or a resume request.
const resumeStoreOpTimeout = 2 * time.Second

func (r redisResumeStore) Save(ctx context.Context, streamID string, snap resumeSnapshot, ttl time.Duration) bool {
	b, err := json.Marshal(snap)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeStoreOpTimeout)
	defer cancel()
	if err := r.c.Set(ctx, r.key(streamID), b, ttl).Err(); err != nil {
		// Fail open: resume degrades to same-replica-only; warn, not error — expected during a Redis blip.
		logger.Warn("chat resume mirror failed", "streamID", streamID, "err", err)
		return false
	}
	return true
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

// watchdogTick returns the subscriber-check interval, honouring the test override.
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

// idleLimitFor is the idle limit that applies to ctl right now: while a hosted step is open, or a provider call
// has not produced first output yet (a reasoning model thinking — awaitingOutput), raised to the hosted limit.
func (s *ChatService) idleLimitFor(ctl *streamCtl) time.Duration {
	limit := s.idleLimit()
	if ctl.hostedOpen.Load() > 0 || ctl.awaitingOutput.Load() {
		hosted := hostedStepIdleTimeout
		if s.hostedIdleTimeout > 0 {
			hosted = s.hostedIdleTimeout
		}
		if limit < hosted {
			limit = hosted
		}
	}
	return limit
}

// watchStream is the stream watchdog; two independent guards share one ticker:
//
//   - Idle: the provider has emitted nothing for idleLimitFor — the upstream is hung; waiting out the
//     wall-clock cap would stall the user silently. Active from stream start (a provider can hang before /begin).
//   - Subscriber: a closed tab can't send /cancel and the stream context is detached from the request, so once
//     begun, cancel after subscriberMissLimit checks with no SSE connection (~30s grace covers reconnect backoff).
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
			if idle := time.Since(time.Unix(0, ctl.lastActivity.Load())); idle > s.idleLimitFor(ctl) {
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

// mirrorStream periodically snapshots ctl to the resume backplane until finished is closed, then writes a final
// snapshot with the short resumeRetention TTL (matching the local finishedStreams grace window).
//
// finished — not ctx.Done() — is the exit signal on purpose: a deliberate cancel cancels ctx BEFORE the worker
// records the terminal state, so a mirror leaving on ctx.Done() published a "final" snapshot that still looked
// live to a reconnect on another replica. The worker closes finished strictly after setFailure / markDone.
//
// published is the mark of the snapshot already written: an unchanged state skips the write; a FAILED write
// leaves the mark unpublished so the next tick retries. The final write is unconditional and shortens the TTL.
func (s *ChatService) mirrorStream(ctx context.Context, streamID string, ctl *streamCtl, finished <-chan struct{}, published mirrorMark) {
	t := time.NewTicker(resumeMirrorInterval)
	defer t.Stop()
	for {
		select {
		case <-finished:
			// Detached context: the stream ctx is done, but the final buffer must still be published.
			s.resumeBackplane().Save(context.WithoutCancel(ctx), streamID, ctl.snapshot(), resumeRetention)
			return
		case <-t.C:
			snap, mark, changed := ctl.snapshotIfChanged(published)
			if !changed {
				continue
			}
			if s.resumeBackplane().Save(ctx, streamID, snap, maxChatStreamDuration+resumeRetention) {
				published = mark
			} else {
				published = unpublishedMark
			}
		}
	}
}

// sliceFrom returns s[from:] clamped to the string bounds; from beyond the end returns "".
// from is a client-supplied BYTE offset on a public endpoint, so it is not trusted to land on a rune boundary:
// the boundary is snapped DOWNWARD — the client re-receives the straddled rune's bytes, whereas snapping up
// would silently discard a character (encoding/json substitutes invalid UTF-8 instead of rejecting it).
func sliceFrom(s string, from int) string {
	if from <= 0 {
		return s
	}
	if from >= len(s) {
		return ""
	}
	for from > 0 && !utf8.RuneStart(s[from]) {
		from--
	}
	return s[from:]
}

// BeginStream unblocks the stream goroutine so it may start emitting. When the stream has already finished —
// typically a pre-stream failure emitted before the client's SSE subscription existed — the final buffered
// state is returned so the caller can deliver it directly; nil means live (or unknown), events arrive over SSE.
// A live stream that already recorded journal events has them re-emitted to the owner: a fresh subscriber
// misses everything emitted before it subscribed.
func (s *ChatService) BeginStream(ctx context.Context, streamID string) *ResumeResult {
	if val, ok := s.activeStreams.Load(streamID); ok {
		ctl := val.(*streamCtl)
		ctl.startOnce.Do(func() { close(ctl.started) })
		// Replay the journal to the owner's SSE connection(s); the events are stream-addressed.
		s.replayJournal(ctl)
		// A failed-fast stream's error was emitted pre-subscription (lost) — hand it back synchronously. A
		// snapshot without done/error means any later terminal event is emitted after this request arrived,
		// after the client subscribed, so SSE delivery covers it.
		if snap := ctl.snapshot(); snap.Done || snap.Error != "" {
			return snap.result(snap.Text)
		}
		return nil
	}
	if res, err := s.ResumeStream(ctx, streamID, 0); err == nil && (res.Done || res.Error != "") {
		return res
	}
	return nil
}

// replayJournal re-emits a stream's recorded journal events to its owner over SSE; no-op when empty.
func (s *ChatService) replayJournal(ctl *streamCtl) {
	ctl.mu.Lock()
	events := ctl.snapshotEvents()
	ctl.mu.Unlock()
	for _, ev := range events {
		s.notifier.EmitTo(ctl.ownerID, "chat:event",
			map[string]interface{}{"streamId": ctl.streamID, "type": ev.Type, "data": ev.Data})
	}
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

// OwnerOf returns the scope that created the given stream, or "" if unknown: active and recently-finished
// streams locally, then the backplane so an authz check on another replica still resolves the owner.
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

// ActiveStreamFor returns the caller's stream on a conversation whose answer a reloading client cannot get from
// the stored history yet — what GET conversation reports as activeStreamId. Reported: a live stream whose
// question is already stored (userTurnStored); failing that, a finished stream whose cleanup has not settled
// (its answer is still being stored). A settled finished stream is not reported (its answer is in the history,
// and resuming would commit it twice), nor is a stream whose client sent its own history.
func (s *ChatService) ActiveStreamFor(scope, flowID, convKey string) string {
	if c := s.activeStreamCtl(scope, flowID, convKey); c != nil {
		return c.streamID
	}
	return ""
}

// activeStreamCtl is ActiveStreamFor's control (nil = none reported).
func (s *ChatService) activeStreamCtl(scope, flowID, convKey string) *streamCtl {
	mine := func(v interface{}) *streamCtl {
		c, ok := v.(*streamCtl)
		if !ok || c.ownerID != scope || c.flowID != flowID || c.convKey != convKey || !c.userTurnStored.Load() {
			return nil
		}
		return c
	}
	var found *streamCtl
	s.activeStreams.Range(func(_, v interface{}) bool {
		found = mine(v)
		return found == nil
	})
	if found != nil {
		return found
	}
	s.finishedStreams.Range(func(_, v interface{}) bool {
		c := mine(v)
		if c == nil || c.settled == nil {
			return true
		}
		select {
		case <-c.settled:
			return true // the answer (if any) is stored and arrives with the history
		default:
			found = c
			return false
		}
	})
	return found
}

// ConversationWithActiveStream is GET conversation's read: stored messages plus the reported stream, resolved so
// a reload racing the stream's two writes neither loses nor duplicates a turn. The stream is looked up BEFORE the
// history; a stream whose answer the history already ends with is dropped (the client would show it twice).
func (s *ChatService) ConversationWithActiveStream(ctx context.Context, scope, flowID, convKey string) ([]models.ChatMessage, string, error) {
	var ctl *streamCtl
	if flowID != "" {
		ctl = s.activeStreamCtl(scope, flowID, convKey)
	}
	msgs, err := s.GetConversation(ctx, flowID, convKey)
	if err != nil {
		return nil, "", err
	}
	if ctl == nil || answerStored(ctl, msgs) {
		return msgs, "", nil
	}
	return msgs, ctl.streamID, nil
}

// answerStored reports whether history already ends with ctl's finished answer.
func answerStored(ctl *streamCtl, history []models.ChatMessage) bool {
	n := len(history)
	if n == 0 || history[n-1].Role != "assistant" {
		return false
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	return ctl.done && history[n-1].Content == ctl.buffer.String()
}

// ResumeStream returns a stream's buffered state, preferring the live local stream and falling back to the
// shared backplane when the stream ran on a different replica.
// The `from` offset (bytes) enables delta resume: a client whose accumulated text is a clean prefix of the buffer
// sends its length and receives only the tail. from is clamped to [0, len(buffer)]; on a chunk-count mismatch
// pass from=0 for a full authoritative replace.
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
		// Copy the buffer under the lock, build the result OUTSIDE it — String()+sliceFrom under mu stalled
		// chunk emission for the duration of a full-buffer copy on long streams.
		buf := ctl.buffer.String()
		snap := &ResumeResult{
			Done:           ctl.done,
			Error:          ctl.errMsg,
			ErrorCode:      ctl.errCode,
			ErrorShared:    ctl.errShared,
			TokensIn:       ctl.tokensIn,
			TokensOut:      ctl.tokensOut,
			ServedModel:    ctl.servedModel,
			InputCostPerM:  ctl.inputCostPerM,
			OutputCostPerM: ctl.outputCostPerM,
			Events:         ctl.snapshotEvents(),
		}
		ctl.mu.Unlock()
		snap.Text = sliceFrom(buf, from)
		return snap, nil
	}
	if snap, ok := s.resumeBackplane().Load(ctx, streamID); ok {
		return snap.result(sliceFrom(snap.Text, from)), nil
	}
	return nil, fmt.Errorf("stream not found or already completed")
}

// result is the ResumeResult a snapshot answers with, its text replaced by text (the delta tail or whole buffer).
func (snap resumeSnapshot) result(text string) *ResumeResult {
	return &ResumeResult{
		Text:           text,
		Done:           snap.Done,
		Error:          snap.Error,
		ErrorCode:      snap.ErrorCode,
		ErrorShared:    snap.ErrorShared,
		TokensIn:       snap.TokensIn,
		TokensOut:      snap.TokensOut,
		ServedModel:    snap.ServedModel,
		InputCostPerM:  snap.InputCostPerM,
		OutputCostPerM: snap.OutputCostPerM,
		Events:         snap.Events,
	}
}
