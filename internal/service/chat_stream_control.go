package service

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// journalEvent is one recorded agentic stream event (tool / tool_result / fix_proposal / fix_decision) kept for
// reconnect replay: the resume buffer only carries scrubbed text, so a disconnected client would otherwise lose
// its tool trail — and an orphaned fix_proposal a guaranteed timeout.
type journalEvent struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

// journaledEventTypes are the emit types recorded to the journal; chunk (buffer carries the text) and
// done/error (ResumeResult fields) are excluded.
var journaledEventTypes = map[string]bool{
	"tool":         true,
	"tool_result":  true,
	"fix_proposal": true,
	"fix_decision": true,
	"notice":       true,
}

// maxJournalEvents bounds the journal: a handful of events per tool iteration, one per batch fix — 100 covers
// every realistic stream; when exceeded the OLDEST entries drop (recent events matter most for replay).
const maxJournalEvents = 100

// recordJournal appends an event when its type is journaled; called on the base emit path under the ctl mutex.
func (c *streamCtl) recordJournal(eventType string, data map[string]interface{}) {
	if !journaledEventTypes[eventType] {
		return
	}
	if len(c.journal) >= maxJournalEvents {
		// Drop half the oldest entries in place, amortized-O(1); the vacated tail is cleared so dropped
		// events are not pinned in the backing array.
		kept := copy(c.journal, c.journal[maxJournalEvents/2:])
		clear(c.journal[kept:])
		c.journal = c.journal[:kept]
	}
	c.journal = append(c.journal, journalEvent{Type: eventType, Data: data})
	c.journalSeq++
}

// setDroppedHistory records the turn's cumulative history_truncated count.
func (c *streamCtl) setDroppedHistory(n int) {
	c.mu.Lock()
	c.droppedHistory = n
	c.mu.Unlock()
}

// droppedHistoryCount is the turn's cumulative history_truncated count so far.
func (c *streamCtl) droppedHistoryCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.droppedHistory
}

// snapshotEvents returns a copy of the journal (callers must not alias the live slice).
func (c *streamCtl) snapshotEvents() []journalEvent {
	if len(c.journal) == 0 {
		return nil
	}
	out := make([]journalEvent, len(c.journal))
	copy(out, c.journal)
	return out
}

// streamCtl holds the mutable, concurrency-safe state of one in-flight chat stream: the scrubbed output buffer,
// terminal status, and cancellation plumbing shared between the worker goroutine, the watchdog and any
// client-facing begin/cancel/resume call.
type streamCtl struct {
	cancel context.CancelFunc
	// streamID addresses this stream's events (journal replay, emit envelopes).
	streamID string
	// droppedHistory is how many history messages the turn has left out to fit the window in all (the request
	// build's, then each loop iteration's), under mu: the stored answer reads it here, since the journal that
	// carries the notices may have been trimmed.
	droppedHistory int
	// started is closed (once) by BeginStream so every waiter unblocks together.
	started   chan struct{}
	startOnce sync.Once
	// settled is closed by the worker's cleanup as its very last step — after the stream left the active set,
	// its terminal state was recorded AND the detached answer persist returned. Store waiters wait here.
	settled   chan struct{}
	mu        sync.Mutex
	buffer    strings.Builder
	done      bool   // stream finished successfully
	errMsg    string // non-empty if the stream ended with an error
	errCode   string // machine-readable class of errMsg ("" = unclassified); see chat_errors.go
	errShared bool   // the failure is one of the org-shared credential classes; see classifyFailureShared
	tokensIn  int
	tokensOut int
	// servedModel is the model that answered (alias providers: the deployment's underlying model); guarded by mu.
	servedModel string
	// ownerID, streamID, flowID and convKey are IMMUTABLE after publication: ActiveStreamFor reads them from other
	// goroutines without a lock, so a later write would be a data race. flowID/convKey identify the conversation,
	// so GET conversation can report the in-flight stream to a client that reloaded mid-turn.
	ownerID string // caller identity (scope) that created this stream
	flowID  string // flow document the stream chats over ("" without one)
	convKey string // conversation key (ChatRequest.ConversationKey)
	// userTurnStored is set once the turn's question is in the stored conversation; ActiveStreamFor reports only
	// such streams — a client re-attaching earlier would save over (and erase) the question the server stored.
	userTurnStored atomic.Bool
	// convWrite is where this turn's own conversation writes left the stored version (the question at stream start,
	// then the answer), kept only while no other writer got in between; the terminal event carries it. nil: unknown.
	convWrite atomic.Pointer[convWrite]
	// sharedCredential records that the turn runs on the org's shared credential (ai.UsedOrgCredential): an auth
	// or permission failure is then the org admin's to fix, and classifyFailure says so.
	sharedCredential atomic.Bool
	// shareHold is what the stream holds across the replicas, its id claim and its cap slot (chat_replicas.go): written
	// by the start before the worker runs, given back by the worker's cleanup. nil with one replica.
	shareHold *streamHold
	// published is set once another replica can know of the stream (its snapshot or an approval of it is in Redis): a
	// replica that cannot keep its liveness key stops such streams (fencePublishedStreams).
	published atomic.Bool
	// cancelReason is the client-facing explanation for a deliberate cancellation; it replaces the raw
	// "context canceled" wrapping when the failure is reported. cancelCode is its class ("" = ChatErrStopped).
	cancelReason string
	cancelCode   string
	// lastActivity is the UnixNano of the most recent provider chunk, read by the idle check in watchStream.
	lastActivity atomic.Int64
	// lastEmit is the UnixNano of the most recent event delivered to the client; distinct from lastActivity on
	// purpose: a heartbeat must never reset the provider-idle watchdog.
	lastEmit atomic.Int64
	// hostedOpen counts the provider's hosted tool steps (agent MCP call, code interpreter, …) started but not
	// finished this turn: a hosted step streams nothing, so while one is open the watchdog allows
	// hostedStepIdleTimeout instead of the normal idle limit.
	hostedOpen atomic.Int32
	// awaitingOutput is set before each provider call and cleared by its first Text/ToolCalls/Done chunk: a
	// reasoning deployment streams nothing while it thinks, so while set the watchdog allows
	// hostedStepIdleTimeout; a provider hung before its first token is caught under maxChatStreamDuration.
	awaitingOutput atomic.Bool
	// journal records agentic events for replay on reconnect; guarded by mu. See journalEvent.
	journal []journalEvent
	// journalSeq counts journal appends (length alone repeats once trimmed); guarded by mu. See mirrorMark.
	journalSeq uint64
	// noticesShown holds provider status lines already shown this turn; guarded by mu. See firstNotice.
	noticesShown map[string]bool
	// terminalSent: the stream's one done or error event has gone out (claimTerminalEvent, recordTerminal); guarded
	// by mu.
	terminalSent bool
}

// firstNotice reports whether a provider status line named name is new this turn, and records it: a retried
// provider call forwards the failed attempt's notice again, and every tool-loop iteration raises the same one.
func (c *streamCtl) firstNotice(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.noticesShown[name] {
		return false
	}
	if c.noticesShown == nil {
		c.noticesShown = make(map[string]bool)
	}
	c.noticesShown[name] = true
	return true
}

// Client keepalive timing (see heartbeatStream); ChatService overrides them in tests, 0 ⇒ these defaults.
const (
	defaultHeartbeatAfter = 20 * time.Second
	defaultHeartbeatTick  = 5 * time.Second
)

func (s *ChatService) heartbeatTiming() (after, tick time.Duration) {
	after, tick = defaultHeartbeatAfter, defaultHeartbeatTick
	if s.heartbeatAfter > 0 {
		after = s.heartbeatAfter
	}
	if s.heartbeatTick > 0 {
		tick = s.heartbeatTick
	}
	return after, tick
}

// heartbeatStream emits a "heartbeat" chat event whenever the client has heard nothing for `after` — several
// phases are silent by design (fix-approval wait, apply + re-analysis, slow tools, reasoning before the first
// token), and without a signal the client's stall probe declared a healthy stream dead while the server went on
// to finish (and bill) it. The heartbeat is not a chunk, is not journaled, and does NOT touch() the idle
// watchdog. Runs until stop closes, ctx ends, or the stream records a terminal state.
func heartbeatStream(ctx context.Context, ctl *streamCtl, emit func(string, map[string]interface{}), stop <-chan struct{}, after, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		}
		select {
		case <-ctl.started:
		default:
			continue // legacy begin handshake pending: nobody is subscribed yet
		}
		ctl.mu.Lock()
		finished := ctl.done || ctl.errMsg != ""
		ctl.mu.Unlock()
		if finished {
			return
		}
		if time.Since(time.Unix(0, ctl.lastEmit.Load())) < after {
			continue
		}
		emit("heartbeat", map[string]interface{}{})
	}
}

// touch records provider activity for the idle timeout.
func (c *streamCtl) touch() { c.lastActivity.Store(time.Now().UnixNano()) }

// hostedStep accounts one provider ToolEvent: a start opens a hosted step, a completion closes one (never below
// zero — an unseen completion must not hide a later open step).
func (c *streamCtl) hostedStep(start bool) {
	if start {
		c.hostedOpen.Add(1)
		return
	}
	for {
		n := c.hostedOpen.Load()
		if n <= 0 || c.hostedOpen.CompareAndSwap(n, n-1) {
			return
		}
	}
}

// endProviderTurn clears the hosted-step count once a provider call returns: a step whose completion never
// arrived must not keep the longer idle limit; awaiting-output is cleared for the same reason.
func (c *streamCtl) endProviderTurn() {
	c.hostedOpen.Store(0)
	c.awaitingOutput.Store(false)
}

// beginProviderTurn marks the start of a provider call: until first output the watchdog applies the hosted-step limit.
func (c *streamCtl) beginProviderTurn() { c.awaitingOutput.Store(true) }

// sawOutput records a chunk that proves the provider is producing output; the normal idle limit applies from here.
func (c *streamCtl) sawOutput(text, toolCalls, done bool) {
	if text || toolCalls || done {
		c.awaitingOutput.Store(false)
	}
}

// cancelWithReason records why the stream is being deliberately cancelled, then cancels it; first reason wins.
func (c *streamCtl) cancelWithReason(reason string) { c.cancelWithCode("", reason) }

// cancelWithCode is cancelWithReason with the failure code the cancellation reports ("" = ChatErrStopped); the
// first reason wins with its code, so a stream the user stopped stays stopped when the server then shuts down.
func (c *streamCtl) cancelWithCode(code, reason string) {
	c.mu.Lock()
	if c.cancelReason == "" {
		c.cancelReason, c.cancelCode = reason, code
	}
	c.mu.Unlock()
	c.cancel()
}

// setFailure records the stream's terminal error message under lock with its code and shared-credential flag
// (see chat_errors.go), so resume/begin can hand them to a client that missed the live error event. Shared by
// every failure path so the buffer/errMsg invariant has one implementation. The first terminal state recorded
// stays (terminalLocked): it is the one the stream's terminal event reports.
func (c *streamCtl) setFailure(code, msg string, shared bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminalLocked() {
		return
	}
	c.errMsg = msg
	c.errCode = code
	c.errShared = shared
}

// setFailureUsage is setFailure for a turn the provider BILLED (an output-limit failure with Done usage in
// hand): tokens and the answering model are stored so resume/begin stop reporting 0/0 for the most expensive
// failures. done stays false: there is no answer to persist.
func (c *streamCtl) setFailureUsage(code, msg string, shared bool, tokensIn, tokensOut int, servedModel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminalLocked() {
		return
	}
	c.errMsg = msg
	c.errCode = code
	c.errShared = shared
	c.tokensIn = tokensIn
	c.tokensOut = tokensOut
	c.servedModel = servedModel
}

// markDone records a successful terminal chunk's tokens and answering model under lock, unless a terminal state
// is recorded already (a worker that outlived Shutdown, which ended the stream in its place).
func (c *streamCtl) markDone(tokensIn, tokensOut int, servedModel string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminalLocked() {
		return
	}
	c.done = true
	c.tokensIn = tokensIn
	c.tokensOut = tokensOut
	c.servedModel = servedModel
}

// terminalLocked reports whether the stream recorded its terminal state; caller holds c.mu.
func (c *streamCtl) terminalLocked() bool { return c.done || c.errMsg != "" }

// claimTerminalEvent reports whether the stream's terminal event (done or error) may go out now, and marks it sent:
// one per stream, whoever sends it. A done is refused once a failure is recorded, so the event the client acts on
// is the state a resume reports. A worker stuck past Shutdown's budget, which ended the stream in its place, used to
// send its own done or error when it finally returned.
func (c *streamCtl) claimTerminalEvent(eventType string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminalSent || (eventType == "done" && c.errMsg != "") {
		return false
	}
	c.terminalSent = true
	return true
}

// ensureTerminal records a failure when the worker exits without done/error recorded (a terminal chunk dropped
// at the begin gate, a provider that returned nothing, a panic) — without it BeginStream reported such a stream
// live forever. A cancelled stream gets its stored reason; anything else is an interruption. Returns set=true
// when it recorded something, so the caller can emit the matching error event.
func (c *streamCtl) ensureTerminal(ctx context.Context) (code, msg string, set bool) {
	code, msg = ChatErrInterrupted, "the response ended before it finished — try again"
	if ctx.Err() != nil {
		if cc, cm := c.classifyFailure(ctx, ctx.Err()); cc != "" {
			code, msg = cc, cm
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done || c.errMsg != "" {
		return "", "", false
	}
	c.errCode, c.errMsg = code, msg
	return code, msg, true
}

// recordTerminal records a failure with code and msg unless the stream already recorded a terminal state or sent
// its terminal event, and reports whether it did: Shutdown's way of ending a stream whose worker is stuck past its
// cancellation. It claims the terminal event in the same step, so the stuck worker sends none when it returns.
func (c *streamCtl) recordTerminal(code, msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminalLocked() || c.terminalSent {
		return false
	}
	c.errCode, c.errMsg = code, msg
	c.terminalSent = true
	return true
}

// failureMessage returns the client-facing message for a stream error (the message half of classifyFailure).
func (c *streamCtl) failureMessage(ctx context.Context, err error) string {
	_, msg := c.classifyFailure(ctx, err)
	return msg
}

// isStreamTruncation matches the stream-helpers' truncation/malformed errors (fmt-wrapped per provider).
func isStreamTruncation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "truncated before terminal marker") ||
		strings.Contains(msg, "undecodable event(s) and no terminal marker")
}

// mirrorMark identifies a version of the stream's resumable state: every changeable field of resumeSnapshot,
// with the buffer and journal standing in by length and append count. Equal marks mean equal snapshots, so the
// resume mirror skips the write (mirrorStream).
type mirrorMark struct {
	textLen             int
	done                bool
	errMsg, errCode     string
	errShared           bool
	tokensIn, tokensOut int
	servedModel         string
	journalSeq          uint64
	// unpublished is never set by markLocked, so a mark carrying it forces the next mirror tick to write.
	unpublished bool
}

// unpublishedMark stands for "nothing stored yet" (a failed backplane write).
var unpublishedMark = mirrorMark{unpublished: true}

// markLocked is the state's current mirrorMark; caller holds c.mu.
func (c *streamCtl) markLocked() mirrorMark {
	return mirrorMark{
		textLen: c.buffer.Len(), done: c.done, errMsg: c.errMsg, errCode: c.errCode, errShared: c.errShared,
		tokensIn: c.tokensIn, tokensOut: c.tokensOut, servedModel: c.servedModel, journalSeq: c.journalSeq,
	}
}

// snapshotMarked is snapshot together with its mirrorMark, taken under one lock.
func (c *streamCtl) snapshotMarked() (resumeSnapshot, mirrorMark) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked(), c.markLocked()
}

// snapshotIfChanged returns the snapshot and its mark when the state's mark differs from last; when nothing
// changed no snapshot is built (the buffer and journal are not copied).
func (c *streamCtl) snapshotIfChanged(last mirrorMark) (snap resumeSnapshot, mark mirrorMark, changed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if mark = c.markLocked(); mark == last {
		return resumeSnapshot{}, mark, false
	}
	return c.snapshotLocked(), mark, true
}

// snapshot captures the stream's resumable state under lock, for mirroring to
// the resume backplane and for building a ResumeResult.
func (c *streamCtl) snapshot() resumeSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

// snapshotLocked is snapshot with c.mu held.
func (c *streamCtl) snapshotLocked() resumeSnapshot {
	return resumeSnapshot{
		Owner:       c.ownerID,
		Text:        c.buffer.String(),
		Done:        c.done,
		Error:       c.errMsg,
		ErrorCode:   c.errCode,
		ErrorShared: c.errShared,
		TokensIn:    c.tokensIn,
		TokensOut:   c.tokensOut,
		ServedModel: c.servedModel,
		Events:      c.snapshotEvents(),
	}
}
