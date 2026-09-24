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
// emits and snapshots serialize).
func (c *streamCtl) recordJournal(eventType string, data map[string]interface{}) {
	if !journaledEventTypes[eventType] {
		return
	}
	if len(c.journal) >= maxJournalEvents {
		// Drop half the oldest entries amortized-O(1) rather than one per hit.
		c.journal = c.journal[maxJournalEvents/2:]
	}
	c.journal = append(c.journal, journalEvent{Type: eventType, Data: data})
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
	mu        sync.Mutex
	buffer    strings.Builder
	done      bool   // stream finished successfully
	errMsg    string // non-empty if the stream ended with an error
	errCode   string // machine-readable class of errMsg ("" = unclassified); see chat_errors.go
	tokensIn  int
	tokensOut int
	ownerID   string // caller identity (scope) that created this stream
	// orgID is the budget scope the stream is attributed to ("" = personal).
	// Set once prepareTurn resolves it; read by othersInFlight.
	orgID string
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
	// journal records agentic events (tool/tool_result/fix_proposal/
	// fix_decision) for replay on reconnect; guarded by mu. See journalEvent.
	journal []journalEvent
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
// with its machine-readable code (see chat_errors.go) so resume/begin can hand
// the code to a client that missed the live error event. Shared by every
// failure path (pre-stream, mid-stream, tool-loop) so the buffer/errMsg
// invariant (mutex-guarded, single writer at a time) has one implementation.
func (c *streamCtl) setFailure(code, msg string) {
	c.mu.Lock()
	c.errMsg = msg
	c.errCode = code
	c.mu.Unlock()
}

// markDone records a successful terminal chunk's token counts under lock.
// Shared by the single-turn stream path and the tool loop's final-answer turn.
func (c *streamCtl) markDone(tokensIn, tokensOut int) {
	c.mu.Lock()
	c.done = true
	c.tokensIn = tokensIn
	c.tokensOut = tokensOut
	c.mu.Unlock()
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
		Owner:     c.ownerID,
		Text:      c.buffer.String(),
		Done:      c.done,
		Error:     c.errMsg,
		ErrorCode: c.errCode,
		TokensIn:  c.tokensIn,
		TokensOut: c.tokensOut,
		Events:    c.snapshotEvents(),
	}
}
