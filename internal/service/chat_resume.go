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

// hostedStepIdleTimeout is the idle limit while a provider-hosted tool step is
// open (streamCtl.hostedOpen): a Foundry agent's MCP call (Azure caps those at
// 100 s) or code-interpreter run streams nothing until it finishes. Still
// bounded, and below maxChatStreamDuration, so a hung hosted step is caught.
const hostedStepIdleTimeout = 5 * time.Minute

// ResumeResult is the buffered state of a stream returned to a reconnecting client.
// Events replays the agentic journal (tool results, fix proposals/decisions)
// recorded since the stream began — the reconnecting client rebuilds its tool
// trail and any pending approval card from these instead of losing them.
type ResumeResult struct {
	Text  string `json:"text"`
	Done  bool   `json:"done"`
	Error string `json:"error"`
	// ErrorCode is Error's machine-readable class (see chat_errors.go), so a
	// client that missed the live error event still gets the same handling.
	ErrorCode string `json:"errorCode,omitempty"`
	// ErrorShared marks a failure on the organisation's shared credential
	// (F-42) — the reloaded client's error bubble offers the Organizations
	// section, not just the member's own Accounts.
	ErrorShared bool `json:"errorShared,omitempty"`
	TokensIn    int  `json:"tokensIn"`
	TokensOut   int  `json:"tokensOut"`
	// ServedModel is the model that answered (alias providers), so a client
	// that missed the live done event still learns it — and with it the USD
	// per 1M token rates the turn was billed at (omitted when not reported).
	ServedModel    string         `json:"servedModel,omitempty"`
	InputCostPerM  float64        `json:"inputCostPerM,omitempty"`
	OutputCostPerM float64        `json:"outputCostPerM,omitempty"`
	Events         []journalEvent `json:"events,omitempty"`
}

// resumeSnapshot is the cross-replica-persistable form of a stream's resumable
// state — the same fields ResumeStream returns, plus the owner so a resume/authz
// request that lands on a different replica can be authorized. It is written to
// the backplane so a client that reconnects to any replica can fetch the buffer.
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

// idleLimitFor is the idle limit that applies to ctl right now: while a
// hosted tool step is open, or a provider call has not produced its first
// output yet (a reasoning model thinking silently — streamCtl.awaitingOutput),
// it is raised to hostedStepIdleTimeout (both honour their test overrides).
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

// watchStream is the stream watchdog. Two independent guards share one ticker:
//
//   - Idle: the provider has emitted nothing for idleLimitFor — the upstream
//     is hung, and waiting out the wall-clock cap would just stall the user
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

// mirrorStream periodically snapshots ctl to the resume backplane until
// finished is closed, then writes a final snapshot with the short
// resumeRetention TTL — matching the local finishedStreams grace window so
// both expire together. Only run when the backplane is enabled.
//
// finished — not ctx.Done() — is the exit signal on purpose: every deliberate
// cancel (user stop, the watchdogs, the wall-clock cap) cancels ctx BEFORE
// the worker records the terminal state, so a mirror that left on ctx.Done()
// published a "final" snapshot with Done=false, Error="" and a reconnect on
// another replica showed a live spinner for a stream that had ended. The
// worker closes finished from its cleanup, strictly after setFailure /
// markDone, and this goroutine is sequential, so no tick write can overtake
// the terminal one.
func (s *ChatService) mirrorStream(ctx context.Context, streamID string, ctl *streamCtl, finished <-chan struct{}) {
	t := time.NewTicker(resumeMirrorInterval)
	defer t.Stop()
	for {
		select {
		case <-finished:
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
//
// from is a BYTE offset supplied by the client over a public endpoint, so it
// is not trusted to land on a rune boundary. Slicing mid-rune yields invalid
// UTF-8, and encoding/json does not reject that — it substitutes U+FFFD, so
// the caller silently receives a replacement character welded to the front of
// the resumed tail instead of an error. The shipped frontend computes a real
// UTF-8 byte length (see lib/utf8.ts), but any other client, an older cached
// bundle, or a hand-rolled integration can pass an arbitrary int.
//
// The boundary is snapped DOWNWARD: the client re-receives the few bytes of
// the rune it straddled, which its append/replace handles, whereas snapping up
// would silently discard a character.
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

// BeginStream unblocks the stream goroutine so it may start emitting. When the
// stream has already finished — typically a pre-stream failure (bad provider,
// budget exceeded) whose error event was emitted before the client's SSE
// subscription existed — the final buffered state is returned instead so the
// caller can deliver it directly. nil means the stream is live (or unknown)
// and events arrive over SSE.
//
// A live stream that already recorded journal events (a tool loop that ran
// while this client was connecting/reconnecting) has them re-emitted over SSE
// to the owner: a fresh subscriber misses everything emitted before it
// subscribed, and without the replay a reconnected client's tool trail and
// any pending fix proposal would be gone for good.
func (s *ChatService) BeginStream(ctx context.Context, streamID string) *ResumeResult {
	if val, ok := s.activeStreams.Load(streamID); ok {
		ctl := val.(*streamCtl)
		ctl.startOnce.Do(func() { close(ctl.started) })
		// Replay the agentic journal to the owner's SSE connection(s); the
		// events are stream-addressed so non-matching clients drop them.
		s.replayJournal(ctl)
		// A failed-fast stream may still be in the active set for an instant;
		// its error was emitted before this call (pre-subscription, so lost) —
		// hand it back synchronously. A snapshot without done/error means any
		// later terminal event is emitted after this request arrived, i.e.
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

// replayJournal re-emits a stream's recorded journal events to its owner over
// SSE. Cheap no-op when the journal is empty (the common begin path).
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

// ActiveStreamFor returns the caller's stream on a conversation whose answer
// a reloading client cannot get from the stored history yet — what GET
// conversation reports as activeStreamId (F-21), so a client that reloads
// mid-turn re-attaches through /api/chat/resume instead of showing a thread
// whose server-side answer appears only after another reload. Reported:
//
//   - a live stream whose question is already stored (userTurnStored) — a
//     client that re-attached before then would save a history without it;
//   - failing that, a finished stream whose cleanup has not settled: its
//     answer is still being stored (persistAssistantTurn runs after the
//     stream left the active set), so the history does not carry it yet.
//
// A settled finished stream is not reported (its answer, if any, is in the
// history, and resuming it would commit the answer a second time), nor is a
// stream whose client sent its own history. "" when there is no such stream.
// GET conversation reads it through ConversationWithActiveStream, which also
// drops a stream whose answer the history already ends with.
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

// ConversationWithActiveStream is GET conversation's read: the stored
// messages plus the stream ActiveStreamFor reports, resolved so that a reload
// racing the stream's two writes neither loses nor duplicates a turn. The
// stream is looked up BEFORE the history is read: it is only reported once
// its question is stored, so a history read after the lookup carries the
// question. A stream whose answer the history read already ends with (it
// finished and persistAssistantTurn stored the answer between the two reads)
// is dropped — the client would otherwise show, and save, the answer twice.
func (s *ChatService) ConversationWithActiveStream(ctx context.Context, scope string, doc *models.FlowDocument, convKey string) ([]models.ChatMessage, string, error) {
	var ctl *streamCtl
	if doc != nil {
		ctl = s.activeStreamCtl(scope, doc.ID, convKey)
	}
	msgs, err := s.GetConversation(ctx, doc, convKey)
	if err != nil {
		return nil, "", err
	}
	if ctl == nil || answerStored(ctl, msgs) {
		return msgs, "", nil
	}
	return msgs, ctl.streamID, nil
}

// answerStored reports whether history already ends with ctl's finished
// answer — the same last-message test persistAssistantTurn de-dupes with.
func answerStored(ctl *streamCtl, history []models.ChatMessage) bool {
	n := len(history)
	if n == 0 || history[n-1].Role != "assistant" {
		return false
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	return ctl.done && history[n-1].Content == ctl.buffer.String()
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
		// B1.13: copy the buffer bytes under the lock (pre-sized), then build
		// the result OUTSIDE it — String()+sliceFrom under mu stalled chunk
		// emission for the duration of a full-buffer copy on long streams.
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

// result is the ResumeResult a snapshot answers with, its text replaced by
// text (the delta-resume tail, or the whole buffer).
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
