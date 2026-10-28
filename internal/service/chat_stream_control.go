package service

import (
	"context"
	"fmt"
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

// maxJournalEvents bounds the journal's droppable events (tool calls and notices, the truncation marker
// included): two per tool call, so it is reached only by a turn of about 50 calls. When it is reached the OLDEST half
// of them drops (recent events matter most for replay). Approval cards and their decisions are never dropped
// (journalKept): a decided proposal must stay in the stored record with its decision, so the journal holds at most
// maxJournalEvents plus the turn's approval events.
const maxJournalEvents = 100

// journalTruncatedName names the marker tool_result that stands for the tool calls the bound dropped.
const journalTruncatedName = "journal_truncated"

// journalKept reports whether a journaled event type survives the bound: the approval cards and their decisions.
func journalKept(eventType string) bool {
	return eventType == "fix_proposal" || eventType == "fix_decision"
}

// recordJournal appends an event when its type is journaled; called on the base emit path under the ctl mutex.
func (c *streamCtl) recordJournal(eventType string, data map[string]interface{}) {
	if !journaledEventTypes[eventType] {
		return
	}
	if !journalKept(eventType) {
		if c.journalDroppable >= maxJournalEvents {
			c.trimJournal()
		}
		c.journalDroppable++
	}
	c.journal = append(c.journal, journalEvent{Type: eventType, Data: data})
	c.journalSeq++
	c.turn.count(eventType, data)
	if eventType == "notice" && data["kind"] == noticeKnowledge {
		c.knowledgeStatus, _ = data["status"].(string)
	}
}

// trimJournal drops the oldest half of the droppable events and records one marker tool_result in their place,
// first in the journal, counting every tool call dropped so far ("N earlier tool calls omitted"): the stored trail
// (journalTurnRecord) and a reconnecting client show that calls are missing. The cut ends between whole calls: past
// the half, it goes on only until the result of every call start it took is taken too (a batch journals all its
// starts, then its results). Starts are paired with their results by name, oldest first (journalCallEnds), so a
// "tool" row that never gets a result — the context-block row, a provider status line, tools_unsupported,
// step_limit_reached, a hosted step whose completion never came — does not hold the cut open to the end of the
// journal. Approval events stay, in order. The journal is rebuilt in a new array, so the dropped events are not pinned
// by the old one. Caller holds c.mu.
func (c *streamCtl) trimJournal() {
	ends := journalCallEnds(c.journal)
	target := maxJournalEvents / 2
	kept := make([]journalEvent, 1, len(c.journal)+1)
	dropped, results := 0, 0
	// cutUntil is the index of the last result a dropped start is waiting for; the cut stays open up to it.
	cutUntil := -1
	for i, ev := range c.journal {
		if isJournalMarker(ev) {
			continue
		}
		if journalKept(ev.Type) || (dropped >= target && i > cutUntil) {
			kept = append(kept, ev)
			continue
		}
		dropped++
		switch ev.Type {
		case "tool":
			if end, paired := ends[i]; paired {
				cutUntil = max(cutUntil, end)
			}
		case "tool_result":
			results++
		}
	}
	c.journalOmitted += results
	kept[0] = journalMarker(c.journalOmitted)
	c.journal = kept
	c.journalDroppable = 0
	for _, ev := range kept {
		if !journalKept(ev.Type) {
			c.journalDroppable++
		}
	}
}

// journalCallEnds pairs each "tool" start of the journal with its tool_result: by name, the oldest open start of
// that name first (a batch emits its starts, then its results in call order). The map is start index -> result
// index; a start without a result (a notice-shaped row, a call still running) is absent.
func journalCallEnds(journal []journalEvent) map[int]int {
	open := map[string][]int{}
	ends := map[int]int{}
	for i, ev := range journal {
		if isJournalMarker(ev) {
			continue
		}
		name := journalString(ev.Data["name"])
		switch ev.Type {
		case "tool":
			open[name] = append(open[name], i)
		case "tool_result":
			if starts := open[name]; len(starts) > 0 {
				ends[starts[0]] = i
				open[name] = starts[1:]
			}
		}
	}
	return ends
}

// journalMarker is the tool_result that stands for omitted dropped tool calls. It is not a tool the model ran: it is
// journaled directly (never emitted live, never counted by the turn's record) and is ok, so no card shows a failure.
func journalMarker(omitted int) journalEvent {
	noun := "calls"
	if omitted == 1 {
		noun = "call"
	}
	return journalEvent{Type: "tool_result", Data: map[string]interface{}{
		"name":       journalTruncatedName,
		"label":      "Earlier tool calls omitted",
		"ok":         true,
		"durationMs": 0,
		"summary":    fmt.Sprintf("%d earlier tool %s omitted", omitted, noun),
	}}
}

// isJournalMarker reports whether ev is the truncation marker.
func isJournalMarker(ev journalEvent) bool {
	return ev.Type == "tool_result" && ev.Data["name"] == journalTruncatedName
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
	// answerUnstored: the server-owned answer's write failed; the done event says answerStored false.
	answerUnstored atomic.Bool
	// conversationCleared: the conversation was deleted while the stream ran (clearingConversation); its answer is
	// not written.
	conversationCleared atomic.Bool
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
	// "context canceled" wrapping when the failure is reported. cancelCode is its class ("" = ChatErrStopped; the
	// watchdog's are ChatErrIdleTimeout and ChatErrClientGone).
	cancelReason string
	cancelCode   string
	// knowledgeStatus is the status of the turn's knowledge notice ("" = none), under mu: the stored answer reads
	// it here, since the journal that carries the notice may have been trimmed.
	knowledgeStatus string
	// turn is what the turn's terminal record says beyond the stream's own state (chat_turn_record.go).
	turn turnRecord
	// applyHolds counts the approved applies running (holdCancel); a cancel asked for meanwhile sets cancelDeferred
	// and fires when the last one releases. Both under mu.
	applyHolds     int
	cancelDeferred bool
	// phaseBeforeApply is the phase an apply interrupted, restored when the last hold releases; under mu.
	phaseBeforeApply streamPhase
	// phase is the streamPhase the stream is in (setPhase), read by the idle check in watchStream.
	phase atomic.Int32
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
	// journalDroppable counts the journal's events the bound may drop (all but the approval events), the marker
	// included; guarded by mu. See recordJournal.
	journalDroppable int
	// journalOmitted counts the tool calls the bound dropped so far, the number the marker states; guarded by mu.
	journalOmitted int
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

// streamPhase is what a stream is doing, for the idle watchdog's message and log line (R-streaming-10).
type streamPhase int32

const (
	// phaseSetup: everything before the first provider call (the provider build, the history, the context build).
	phaseSetup streamPhase = iota
	// phaseProvider: a provider call, from its start to its return.
	phaseProvider
	// phaseTool: the tools of a tool-loop iteration.
	phaseTool
	// phaseApproval: an approval card waiting for the user's decision.
	phaseApproval
	// phaseApply: an approved fix being applied.
	phaseApply
)

func (p streamPhase) String() string {
	switch p {
	case phaseProvider:
		return "provider"
	case phaseTool:
		return "tool"
	case phaseApproval:
		return "approval"
	case phaseApply:
		return "apply"
	}
	return "setup"
}

// setPhase records what the stream is doing now.
func (c *streamCtl) setPhase(p streamPhase) { c.phase.Store(int32(p)) }

// currentPhase is what the stream is doing now.
func (c *streamCtl) currentPhase() streamPhase { return streamPhase(c.phase.Load()) }

// idleCancelMessage is the ChatErrIdleTimeout copy for a stream that stalled in phase p: only a provider-phase stall
// blames the provider (a stall in setup was reported as the provider's, which was never called). The approval and
// apply copies are for completeness: the approval wait touches the clock every fixDecisionKeepalive and the apply
// through its applier's touch, and an apply holds the cancel until it returns, so neither is reached in practice.
func idleCancelMessage(p streamPhase) string {
	switch p {
	case phaseProvider:
		return "response stopped: the AI provider stopped responding"
	case phaseTool:
		return "response stopped: a tool the assistant ran stopped responding"
	case phaseApproval:
		return "response stopped: nothing happened while it waited for your approval"
	case phaseApply:
		return "response stopped: applying the fix stopped responding"
	}
	return "response stopped: setting up the response took too long — the AI provider was not called yet; try again"
}

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
func (c *streamCtl) beginProviderTurn() {
	c.setPhase(phaseProvider)
	c.awaitingOutput.Store(true)
}

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
//
// While an approved fix is being applied (holdCancel) the cancel itself waits for it: the write and its undo
// snapshot cannot be cut in half, and the fix's decision reaches the client before the stream's terminal event.
func (c *streamCtl) cancelWithCode(code, reason string) {
	c.mu.Lock()
	if c.cancelReason == "" {
		c.cancelReason, c.cancelCode = reason, code
	}
	if c.applyHolds > 0 {
		c.cancelDeferred = true
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	c.cancel()
}

// holdCancel makes the stream's cancellation wait until release is called: an approved fix's apply and its
// verification run as one critical section (R-approvals-c17). false when the stream is already ending — the fix
// must not start then. Shutdown bounds the wait (forceCancel).
func (c *streamCtl) holdCancel() (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelReason != "" && c.applyHolds == 0 {
		return func() {}, false
	}
	if c.applyHolds == 0 {
		c.phaseBeforeApply = c.currentPhase()
		c.setPhase(phaseApply)
	}
	c.applyHolds++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.applyHolds--
			if c.applyHolds == 0 {
				c.setPhase(c.phaseBeforeApply)
			}
			fire := c.applyHolds == 0 && c.cancelDeferred
			if fire {
				c.cancelDeferred = false
			}
			c.mu.Unlock()
			if fire {
				c.cancel()
			}
		})
	}, true
}

// stopRequested reports a cancellation asked for while the cancel is held, with its code ("" = the user stopped).
func (c *streamCtl) stopRequested() (code string, asked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelCode, c.cancelReason != ""
}

// forceCancel cancels the stream now, held or not: Shutdown's budget is spent.
func (c *streamCtl) forceCancel() { c.cancel() }

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

// stampAnswerStored adds answerStored false to a done event when the stream's server-side answer write failed
// (R-tool-loop-20); absent otherwise, so older clients see the event unchanged.
func (c *streamCtl) stampAnswerStored(data map[string]interface{}) {
	if c.answerUnstored.Load() {
		data["answerStored"] = false
	}
}

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
		Owner:        c.ownerID,
		Text:         c.buffer.String(),
		Done:         c.done,
		Error:        c.errMsg,
		ErrorCode:    c.errCode,
		ErrorShared:  c.errShared,
		TokensIn:     c.tokensIn,
		TokensOut:    c.tokensOut,
		ServedModel:  c.servedModel,
		Events:       c.snapshotEvents(),
		AnswerStored: c.answerStoredField(),
	}
}

// answerStoredField is the resume form of stampAnswerStored: false when the answer's write failed, else absent.
func (c *streamCtl) answerStoredField() *bool {
	if !c.answerUnstored.Load() {
		return nil
	}
	stored := false
	return &stored
}
