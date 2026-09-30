package service

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// journalEvent is one recorded agentic stream event (tool / tool_result /
// fix_proposal / fix_decision) kept for reconnect replay: the resume buffer
// only carries scrubbed text, so a client that drops mid-stream and resumes
// used to lose its tool trail entirely — and a fix_proposal emitted while
// disconnected orphaned the approval card into a guaranteed 60s timeout.
type journalEvent struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

// journaledEventTypes are the emit types recorded to the journal. chunk is
// excluded (the resume buffer already carries the text); done/error are
// excluded (ResumeResult carries them as fields).
var journaledEventTypes = map[string]bool{
	"tool":         true,
	"tool_result":  true,
	"fix_proposal": true,
	"fix_decision": true,
}

// maxJournalEvents bounds the journal. Tool loops produce a handful of events
// per iteration (cap 6 iterations); batch fixes add one event per batch, not
// per item. 100 covers every realistic stream; when exceeded, the OLDEST
// entries drop (recent events matter most for replay).
const maxJournalEvents = 100

// recordJournal appends an event when its type is journaled. Called on the
// stream's base emit path, under the ctl mutex (same lock as the buffer —
// emits and snapshots serialize). It also keeps the billed rates the done
// event carries (doneEventData) for resume/begin (noteDoneRates).
func (c *streamCtl) recordJournal(eventType string, data map[string]interface{}) {
	// done and error: the error path carries the billed usage of a failed
	// turn (failUsage, F-15), and a client that missed the live event must
	// get the same rates from ResumeStream / BeginStream.
	if eventType == "done" || eventType == "error" {
		c.noteDoneRates(data)
	}
	if !journaledEventTypes[eventType] {
		return
	}
	if len(c.journal) >= maxJournalEvents {
		// Drop half the oldest entries amortized-O(1) rather than one per hit.
		c.journal = c.journal[maxJournalEvents/2:]
	}
	c.journal = append(c.journal, journalEvent{Type: eventType, Data: data})
}

// noteDoneRates records the USD per 1M token rates a done event's payload
// carries (inputCostPerM / outputCostPerM, set by doneEventData when the
// provider can price the turn), so a client that missed the live done event
// gets them from ResumeStream / BeginStream like the served model. Absent
// or non-numeric values leave the rates at zero (= not reported). Caller
// holds c.mu.
func (c *streamCtl) noteDoneRates(data map[string]interface{}) {
	if v, ok := data["inputCostPerM"].(float64); ok {
		c.inputCostPerM = v
	}
	if v, ok := data["outputCostPerM"].(float64); ok {
		c.outputCostPerM = v
	}
}

// snapshotEvents returns a copy of the journal for inclusion in a snapshot or
// ResumeResult (callers must not alias the live slice).
func (c *streamCtl) snapshotEvents() []journalEvent {
	if len(c.journal) == 0 {
		return nil
	}
	out := make([]journalEvent, len(c.journal))
	copy(out, c.journal)
	return out
}

// streamCtl holds the mutable, concurrency-safe state of one in-flight chat
// stream: the accumulated (scrubbed) output buffer, terminal status, and the
// cancellation plumbing shared between the stream worker goroutine, the
// watchdog, and any client-facing begin/cancel/resume call.
type streamCtl struct {
	cancel context.CancelFunc
	// streamID addresses this stream's events (journal replay, emit envelopes).
	streamID string
	// started is closed (once) by BeginStream so every waiter — the emit gate
	// and the subscriber watchdog — unblocks together.
	started   chan struct{}
	startOnce sync.Once
	// settled is closed by the worker's cleanup as its very last step — after
	// the stream left the active set, its terminal state was recorded AND the
	// detached answer persist (persistAssistantTurn) returned. The active set
	// alone says the stream is over, not that its answer is stored; callers
	// that need the store (tests) wait here.
	settled   chan struct{}
	mu        sync.Mutex
	buffer    strings.Builder
	done      bool   // stream finished successfully
	errMsg    string // non-empty if the stream ended with an error
	errCode   string // machine-readable class of errMsg ("" = unclassified); see chat_errors.go
	errShared bool   // the failure is one of the org-shared credential classes (F-42); see classifyFailureShared
	tokensIn  int
	tokensOut int
	// servedModel is the model that answered (alias providers: the deployment's
	// underlying model, from the provider's terminal chunk); "" when the
	// provider named none. Guarded by mu; carried on done/resume payloads.
	servedModel string
	// inputCostPerM / outputCostPerM are the rates the finished turn was
	// billed at, taken from the done event (noteDoneRates); 0 = not
	// reported. Guarded by mu; carried on resume/begin payloads.
	inputCostPerM  float64
	outputCostPerM float64
	// ownerID, orgID, streamID, flowID and convKey are IMMUTABLE after
	// publication: the control is stored in activeStreams (tryStartStream)
	// before the worker exists, and othersInFlight / ActiveStreamFor read them
	// from other goroutines without a lock, so a later write would be a data
	// race. orgID is the budget scope the stream is attributed to ("" =
	// personal), taken from the flow document at construction; flowID/convKey
	// identify the conversation the stream belongs to, so GET conversation can
	// report the in-flight stream to a client that reloaded mid-turn (F-21).
	ownerID string // caller identity (scope) that created this stream
	orgID   string
	flowID  string // flow document the stream chats over ("" without one)
	convKey string // conversation key (ChatRequest.ConversationKey)
	// userTurnStored is set once the turn's question is in the stored
	// conversation (persistInitialUserTurn stored it). ActiveStreamFor reports
	// only such streams: a client that reloads before then would re-attach
	// with a history lacking the question, and its full-replace save on done
	// would erase the question the server stored meanwhile (F-21). Never set
	// for a turn whose client sent its own history (it owns persistence), nor
	// when the history read failed or "Save conversation history" is off.
	userTurnStored atomic.Bool
	// sharedCredential records that the turn's provider runs on the org's
	// shared credential (ai.UsedOrgCredential), set by prepareTurn: an auth
	// or permission failure is then the org admin's to fix, and
	// classifyFailure says so.
	sharedCredential atomic.Bool
	// inflightCost is what THIS stream has spent so far plus one more maximum
	// response it may still produce, in dollars, for OTHER streams' budget
	// checks. The daily budget used to be read-then-act against the store
	// alone, and usage rows land asynchronously after each turn, so N streams
	// started together all passed the same read — measured 10× overspend on
	// a $1 budget. Guarded by mu.
	inflightCost float64
	// cancelReason is the client-facing explanation for a deliberate
	// cancellation (user stop, watchdog, shutdown); it replaces the provider's
	// raw "context canceled" wrapping when the failure is reported.
	cancelReason string
	// lastActivity is the UnixNano of the most recent provider chunk (any
	// kind, before scrub holdback), read by the idle check in watchStream.
	lastActivity atomic.Int64
	// lastEmit is the UnixNano of the most recent event delivered to the
	// client (any type). The heartbeat goroutine reads it to decide whether the
	// client has heard nothing for long enough to need a keepalive. Distinct
	// from lastActivity on purpose: a heartbeat must never reset the
	// provider-idle watchdog.
	lastEmit atomic.Int64
	// hostedOpen counts the provider's hosted tool steps (a Foundry agent's
	// MCP call, code interpreter run, …) that have started but not finished
	// in the current provider turn. A hosted step runs server-side without
	// streaming anything, so while one is open the idle watchdog allows
	// hostedStepIdleTimeout instead of the normal idle limit.
	hostedOpen atomic.Int32
	// awaitingOutput is set before each provider call and cleared by its first
	// Text / ToolCalls / Done chunk (and by endProviderTurn). A reasoning
	// deployment (o-series, gpt-5, DeepSeek-R1 on chat completions) streams
	// nothing while it thinks, and a Responses run emits argument and
	// reasoning deltas the parser may not surface as chunks, so while it is
	// set the idle watchdog allows hostedStepIdleTimeout instead of the normal
	// limit — a provider that hangs before its first token is caught at the
	// hosted limit, still under maxChatStreamDuration.
	awaitingOutput atomic.Bool
	// journal records agentic events (tool/tool_result/fix_proposal/
	// fix_decision) for replay on reconnect; guarded by mu. See journalEvent.
	journal []journalEvent
	// noticesShown holds the provider status lines (ai.Chunk.Notice names)
	// already shown this turn; guarded by mu. See firstNotice.
	noticesShown map[string]bool
}

// firstNotice reports whether a provider status line named name is new this
// turn, and records it: a retried provider call forwards the failed
// attempt's notice again (RetryingProvider), and every tool-loop iteration
// on the same deployment raises the same one — the trail shows it once.
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

// Client keepalive timing (see heartbeatStream). ChatService.heartbeatAfter /
// heartbeatTick override them in tests; 0 ⇒ these defaults.
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

// heartbeatStream emits a "heartbeat" chat event whenever the client has heard
// nothing from this stream for `after`. Several phases are silent by
// design — the fix-approval wait (up to ai.FixDecisionTimeout), apply +
// re-analysis, slow tools, reasoning models that think before their first
// token — and without a signal the client's stall probe declared a healthy
// stream dead after ~90s while the server went on to finish (and bill) it.
//
// The heartbeat is not a chunk (never counted in done.chunks), is not
// journaled, and deliberately does NOT touch() the provider-idle watchdog: a
// provider that has really stalled must still time out. It runs until stop is
// closed, ctx ends, or the stream records a terminal state.
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

// hostedStep accounts one provider ToolEvent: a start opens a hosted step, a
// completion closes one (never below zero — a completion whose start was not
// seen must not hide a later open step).
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

// endProviderTurn clears the hosted-step count once a provider call returns:
// a step whose completion never arrived (failed or truncated stream) must
// not keep the longer idle limit for the rest of the stream. The
// awaiting-output flag is cleared for the same reason.
func (c *streamCtl) endProviderTurn() {
	c.hostedOpen.Store(0)
	c.awaitingOutput.Store(false)
}

// beginProviderTurn marks the start of a provider call: until its first
// output chunk arrives the idle watchdog applies the hosted-step limit (see
// awaitingOutput).
func (c *streamCtl) beginProviderTurn() { c.awaitingOutput.Store(true) }

// sawOutput records a chunk that proves the provider is producing output
// (text, tool calls or the terminal chunk): the normal idle limit applies
// from here on.
func (c *streamCtl) sawOutput(text, toolCalls, done bool) {
	if text || toolCalls || done {
		c.awaitingOutput.Store(false)
	}
}

// setInflight publishes this stream's spend-so-far plus reservation for
// concurrent streams' budget checks (see inflightCost).
func (c *streamCtl) setInflight(cost float64) {
	c.mu.Lock()
	c.inflightCost = cost
	c.mu.Unlock()
}

func (c *streamCtl) inflight() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflightCost
}

// cancelWithReason records why the stream is being deliberately cancelled,
// then cancels it. The first reason wins.
func (c *streamCtl) cancelWithReason(reason string) {
	c.mu.Lock()
	if c.cancelReason == "" {
		c.cancelReason = reason
	}
	c.mu.Unlock()
	c.cancel()
}

// setFailure records the stream's terminal error message under lock, together
// with its machine-readable code and shared-credential flag (see
// chat_errors.go) so resume/begin can hand them to a client that missed the
// live error event. Shared by every failure path (pre-stream, mid-stream,
// tool-loop) so the buffer/errMsg invariant (mutex-guarded, single writer at
// a time) has one implementation.
func (c *streamCtl) setFailure(code, msg string, shared bool) {
	c.mu.Lock()
	c.errMsg = msg
	c.errCode = code
	c.errShared = shared
	c.mu.Unlock()
}

// setFailureUsage is setFailure for a turn the provider BILLED (an
// output-limit failure whose Done usage is in hand, F-15): the token counts
// and the model that answered are stored alongside the failure, so
// ResumeStream and BeginStream snapshots stop reporting 0/0 for the most
// expensive failures — up to 16384 reasoning tokens per failed turn. done
// stays false: the stream failed, there is no answer to persist.
func (c *streamCtl) setFailureUsage(code, msg string, shared bool, tokensIn, tokensOut int, servedModel string) {
	c.mu.Lock()
	c.errMsg = msg
	c.errCode = code
	c.errShared = shared
	c.tokensIn = tokensIn
	c.tokensOut = tokensOut
	c.servedModel = servedModel
	c.mu.Unlock()
}

// markDone records a successful terminal chunk's token counts and the model
// that answered under lock. Shared by the single-turn stream path and the
// tool loop's final-answer turn.
func (c *streamCtl) markDone(tokensIn, tokensOut int, servedModel string) {
	c.mu.Lock()
	c.done = true
	c.tokensIn = tokensIn
	c.tokensOut = tokensOut
	c.servedModel = servedModel
	c.mu.Unlock()
}

// ensureTerminal records a failure when the worker exits without done/error
// recorded: a Done or Error chunk dropped at the begin gate (the legacy
// handshake cancelled before /chat/begin), a provider that returned nil after
// its chunks were dropped, a panic. Without it the stream moved to
// finishedStreams as neither done nor failed, BeginStream reported it live and
// ResumeStream handed back a non-terminal snapshot for good. A cancelled
// stream gets its stored reason (or the timeout copy); anything else is an
// interruption. Returns set=true when it recorded something, so the caller
// can emit the matching error event to a subscribed client.
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

// failureMessage returns the client-facing message for a stream error (the
// message half of classifyFailure, for callers that don't carry a code).
func (c *streamCtl) failureMessage(ctx context.Context, err error) string {
	_, msg := c.classifyFailure(ctx, err)
	return msg
}

// isStreamTruncation matches the stream-helpers' truncation/malformed errors
// (they're fmt-wrapped per provider, so match on the message shape).
func isStreamTruncation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "truncated before terminal marker") ||
		strings.Contains(msg, "undecodable event(s) and no terminal marker")
}

// snapshot captures the stream's resumable state under lock, for mirroring to
// the resume backplane and for building a ResumeResult.
func (c *streamCtl) snapshot() resumeSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return resumeSnapshot{
		Owner:          c.ownerID,
		Text:           c.buffer.String(),
		Done:           c.done,
		Error:          c.errMsg,
		ErrorCode:      c.errCode,
		ErrorShared:    c.errShared,
		TokensIn:       c.tokensIn,
		TokensOut:      c.tokensOut,
		ServedModel:    c.servedModel,
		InputCostPerM:  c.inputCostPerM,
		OutputCostPerM: c.outputCostPerM,
		Events:         c.snapshotEvents(),
	}
}
