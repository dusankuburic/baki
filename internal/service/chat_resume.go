package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"pad-core/logger"
	"pad-core/models"

	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/redisx"
)

// resumeRetention is how long a finished stream's buffer is kept so a client disconnected when the stream ended
// can still fetch the final text on reconnect.
const resumeRetention = 2 * time.Minute

// resumeKey names a stream's resume snapshot: a string, rewritten at most once a second while the buffer grows. A
// snapshot is v1 from the commands on (ADR 0033: {"v":1,"kind":"chat.resume"} and the owner "replica", all additive,
// so a release before them reads it as it read v0); while Redis is down the mirror is skipped and resuming from
// another replica answers "stream not found".
var resumeKey = redisx.Declare(redisx.Spec{
	Name:    "chat.resume",
	Kind:    redisx.KindString,
	Pattern: "chat:resume:{stream}",
	TTL:     maxChatStreamDuration + resumeRetention,
	Version: 1,
	OnDown:  redisx.OnDownOpen,
	Owner:   "internal/service",
})

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
	// ServedModel is the model that answered (alias providers).
	ServedModel string         `json:"servedModel,omitempty"`
	Events      []journalEvent `json:"events,omitempty"`
	// AnswerStored is false when the server-owned answer's write failed (the done event's answerStored): a client
	// that reattaches after the done saves the turn itself. Absent otherwise.
	AnswerStored *bool `json:"answerStored,omitempty"`
}

// resumeSnapshot is the cross-replica-persistable form of a stream's resumable state, plus the owner so a
// resume/authz request landing on a different replica can be authorized.
type resumeSnapshot struct {
	// V and Kind are the Redis envelope (redisx.Envelope), Replica the instance running the stream, where its
	// commands go. The store stamps all three on the way out; a v0 snapshot has none and cannot be commanded. Live says
	// the owner keeps a liveness key (redisx.ReplicaLiveKey), so its absence means the stream died with the replica;
	// without it (one replica configured, or the release before) the owner's silence proves nothing.
	V           int            `json:"v,omitempty"`
	Kind        string         `json:"kind,omitempty"`
	Replica     string         `json:"replica,omitempty"`
	Live        bool           `json:"live,omitempty"`
	Owner       string         `json:"owner"`
	Text        string         `json:"text"`
	Done        bool           `json:"done"`
	Error       string         `json:"error"`
	ErrorCode   string         `json:"errorCode,omitempty"`
	ErrorShared bool           `json:"errorShared,omitempty"`
	TokensIn    int            `json:"tokensIn"`
	TokensOut   int            `json:"tokensOut"`
	ServedModel string         `json:"servedModel,omitempty"`
	Events      []journalEvent `json:"events,omitempty"`
	// AnswerStored is ResumeResult.AnswerStored.
	AnswerStored *bool `json:"answerStored,omitempty"`
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

// redisResumeStore mirrors resumable stream state into Redis under a per-stream key with a TTL. instance names this
// replica in every snapshot it writes ("" in a test that does not care).
type redisResumeStore struct {
	c        *redis.Client
	instance string
	// live: this replica keeps its liveness key, which every snapshot it writes then says (resumeSnapshot.Live).
	live bool
}

func (redisResumeStore) enabled() bool { return true }

func (redisResumeStore) key(streamID string) string { return resumeKey.Key(streamID) }

// resumeStoreOpTimeout bounds a single backplane round-trip so it can't stall the stream or a resume request.
const resumeStoreOpTimeout = 2 * time.Second

func (r redisResumeStore) Save(ctx context.Context, streamID string, snap resumeSnapshot, ttl time.Duration) bool {
	snap.V, snap.Kind, snap.Replica, snap.Live = resumeKey.Spec().Version, resumeKey.Name(), r.instance, r.live
	b, err := json.Marshal(snap)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeStoreOpTimeout)
	defer cancel()
	if err := r.c.Set(ctx, r.key(streamID), b, ttl).Err(); err != nil {
		// Fail open: resume degrades to same-replica-only; warn, not error — expected during a Redis blip.
		redisx.LimitedWarn("chat_resume_mirror", "chat resume mirror failed", err)
		return false
	}
	return true
}

func (r redisResumeStore) Load(ctx context.Context, streamID string) (resumeSnapshot, bool) {
	// redis.Nil (miss) or any transport error → treat as not found.
	snap, found, _ := r.Lookup(ctx, streamID)
	return snap, found
}

// Lookup is Load that tells a miss (found false, err nil) from a backplane that could not answer (err): for the
// commands an unreadable snapshot is "retry", not "no such stream". See lookupSnapshot.
func (r redisResumeStore) Lookup(ctx context.Context, streamID string) (resumeSnapshot, bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeStoreOpTimeout)
	defer cancel()
	b, err := r.c.Get(ctx, r.key(streamID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return resumeSnapshot{}, false, nil
	}
	if err != nil {
		return resumeSnapshot{}, false, err
	}
	// A snapshot another release wrote in a shape this one does not know is a miss, and stays in place.
	var snap resumeSnapshot
	if redisx.Decode(resumeKey.Name(), resumeKey.Spec().Version, b, &snap) != redisx.ResultOK {
		return resumeSnapshot{}, false, nil
	}
	return snap, true, nil
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
				// The phase it stalled in names the culprit: a setup stall is not the provider's (R-streaming-10).
				phase := ctl.currentPhase()
				logger.FromContext(ctx).Info("chat stream cancelled: idle", "idle", idle, "phase", phase.String())
				ctl.cancelWithCode(ChatErrIdleTimeout, idleCancelMessage(phase))
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
				logger.FromContext(ctx).Info("chat stream cancelled: client SSE connection gone")
				ctl.cancelWithCode(ChatErrClientGone, "response stopped: you were disconnected while it was generating")
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
	pointed := false // the conversation's active-stream pointer is written
	for {
		select {
		case <-finished:
			// Detached context: the stream ctx is done, but the final buffer must still be published.
			s.resumeBackplane().Save(context.WithoutCancel(ctx), streamID, ctl.snapshot(), resumeRetention)
			if s.pointActive(ctl, pointed) {
				s.cluster.clearActiveWhenSettled(ctl)
			}
			return
		case <-t.C:
			pointed = s.pointActive(ctl, pointed)
			snap, mark, changed := ctl.snapshotIfChanged(published)
			if !changed {
				continue
			}
			if s.resumeBackplane().Save(ctx, streamID, snap, maxChatStreamDuration+resumeRetention) {
				published = mark
				ctl.published.Store(true)
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

// CancelAll ends every running stream as the server stops (ChatErrServerRestarting) without waiting for them.
// The stop hook calls Shutdown, which also waits; this stays for callers that must not block.
func (s *ChatService) CancelAll() { s.cancelAll() }

// cancelAll cancels every active stream with ChatErrServerRestarting and returns how many it reached. A stream
// applying an approved fix ends once the apply returns (holdCancel).
func (s *ChatService) cancelAll() int {
	n := 0
	s.activeStreams.Range(func(_, value interface{}) bool {
		value.(*streamCtl).cancelWithCode(ChatErrServerRestarting, serverRestartingMsg)
		n++
		return true
	})
	return n
}

// chatShutdownBudget bounds Shutdown's wait for the workers it ended: a terminal emit, the final resume snapshot
// and a just-finished answer's persist take milliseconds when nothing is stuck, and the SSE connections, the pool
// and Redis close only after it.
const chatShutdownBudget = 2 * time.Second

// chatShutdownStopMargin is how long Shutdown waits, after its budget, for the workers whose answer write the stop
// cut to send their done.
const chatShutdownStopMargin = 500 * time.Millisecond

// Shutdown is the chat half of the server's stop (ADR 0024), called while the SSE connections are still open: it
// refuses new streams (ErrServerRestarting), cancels every running one with ChatErrServerRestarting, and waits,
// within ctx and chatShutdownBudget, for each worker to deliver that terminal event, publish its final resume
// snapshot and finish storing a just-completed answer. A stream whose worker is still running at the deadline
// gets its terminal state recorded, emitted and mirrored here, so its client hears of it before the connection
// closes; the error then says how many.
func (s *ChatService) Shutdown(ctx context.Context) error {
	s.streamCapMu.Lock()
	s.closing = true
	s.streamCapMu.Unlock()
	// The command subscriptions end last, so a stop another replica relays while the streams wind down is still answered.
	defer s.StopCommands()
	active := s.cancelAll()

	budget := chatShutdownBudget
	if s.shutdownBudget > 0 {
		budget = s.shutdownBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	idle := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(idle)
	}()
	select {
	case <-idle:
		if active > 0 {
			logger.FromContext(ctx).Info("chat streams ended for shutdown", "streams", active)
		}
		return nil
	case <-ctx.Done():
	}
	// The budget is spent: an approved apply still holding its stream's cancel is cut now.
	s.activeStreams.Range(func(_, value interface{}) bool {
		value.(*streamCtl).forceCancel()
		return true
	})
	forced := s.forceServerRestarting(ctx)
	// An answer write still under way (its done is claimed, so the stream was not ended above) is cut now; its
	// worker sends the done, answerStored false, within a short margin (R-streaming-c16).
	s.signalStop()
	select {
	case <-idle:
		if forced == 0 {
			logger.FromContext(ctx).Info("chat streams ended for shutdown after the stop cut their answer writes", "streams", active)
			return nil
		}
	case <-time.After(chatShutdownStopMargin):
	}
	logger.FromContext(ctx).Warn("chat workers still running at shutdown", "streams", active, "forced", forced, "budget", budget)
	return fmt.Errorf("chat shutdown: workers still running after %v (%d stream(s) ended in their place): %w", budget, forced, ctx.Err())
}

// forceServerRestarting ends, in their worker's place, the active streams that recorded no terminal state: it
// records ChatErrServerRestarting, emits it to a begun stream's owner and mirrors it. A stuck worker that finishes
// later sends no terminal event of its own (recordTerminal claims it), and its answer is not stored. Returns how
// many it ended.
func (s *ChatService) forceServerRestarting(ctx context.Context) int {
	n := 0
	s.activeStreams.Range(func(_, value interface{}) bool {
		ctl := value.(*streamCtl)
		if !ctl.recordTerminal(ChatErrServerRestarting, serverRestartingMsg) {
			return true
		}
		n++
		// Recorded now: the stuck worker may never end, and when it does it adds no second record.
		s.recordTurnEnded(ctl)
		select {
		case <-ctl.started:
			s.notifier.EmitTo(ctl.ownerID, "chat:event", map[string]interface{}{
				"streamId": ctl.streamID, "type": "error", "data": errorEventData(ChatErrServerRestarting, serverRestartingMsg, false)})
		default: // never begun: BeginStream and ResumeStream report the state
		}
		if rs := s.resumeBackplane(); rs.enabled() {
			rs.Save(context.WithoutCancel(ctx), ctl.streamID, ctl.snapshot(), resumeRetention)
		}
		return true
	})
	return n
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
// and resuming would commit it twice), nor is a stream whose client sent its own history. This is the local
// answer; ReadConversation (what GET conversation calls) also asks the shared pointer for a stream another replica
// runs (chat_commands.go).
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
	read, err := s.ReadConversation(ctx, scope, flowID, convKey)
	if err != nil {
		return nil, "", err
	}
	return read.Messages, read.ActiveStreamID, nil
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
			Done:        ctl.done,
			Error:       ctl.errMsg,
			ErrorCode:   ctl.errCode,
			ErrorShared: ctl.errShared,
			TokensIn:    ctl.tokensIn,
			TokensOut:   ctl.tokensOut,
			ServedModel: ctl.servedModel,
			Events:      ctl.snapshotEvents(),
		}
		snap.AnswerStored = ctl.answerStoredField()
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
		Text:         text,
		Done:         snap.Done,
		Error:        snap.Error,
		ErrorCode:    snap.ErrorCode,
		ErrorShared:  snap.ErrorShared,
		TokensIn:     snap.TokensIn,
		TokensOut:    snap.TokensOut,
		ServedModel:  snap.ServedModel,
		Events:       snap.Events,
		AnswerStored: snap.AnswerStored,
	}
}
