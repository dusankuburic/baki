package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pad-analyzer/internal/ai"
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
	tokensIn  int
	tokensOut int
	ownerID   string // caller identity (scope) that created this stream
	// cancelReason is the client-facing explanation for a deliberate
	// cancellation (user stop, watchdog, shutdown); it replaces the provider's
	// raw "context canceled" wrapping when the failure is reported.
	cancelReason string
	// lastActivity is the UnixNano of the most recent provider chunk (any
	// kind, before scrub holdback), read by the idle check in watchStream.
	lastActivity atomic.Int64
	// journal records agentic events (tool/tool_result/fix_proposal/
	// fix_decision) for replay on reconnect; guarded by mu. See journalEvent.
	journal []journalEvent
}

// touch records provider activity for the idle timeout.
func (c *streamCtl) touch() { c.lastActivity.Store(time.Now().UnixNano()) }

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

// setError records the stream's terminal error message under lock. Shared by
// every failure path (pre-stream, mid-stream, tool-loop) so the buffer/errMsg
// invariant (mutex-guarded, single writer at a time) has one implementation.
func (c *streamCtl) setError(msg string) {
	c.mu.Lock()
	c.errMsg = msg
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

// failureMessage returns the client-facing message for a stream error. A
// deliberate cancellation surfaces its stored reason, the stream-duration
// timeout gets a readable message, and the common provider sentinels map to
// actionable copy — a raw "rate limited" or "provider circuit open: too many
// recent failures" told the user nothing they could act on.
func (c *streamCtl) failureMessage(ctx context.Context, err error) string {
	c.mu.Lock()
	reason := c.cancelReason
	c.mu.Unlock()
	if ctx.Err() != nil {
		if reason != "" {
			return reason
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "response stopped: maximum response time reached"
		}
	}
	switch {
	case errors.Is(err, ai.ErrContextLimit):
		return "conversation is too long for this model's context window — start a new conversation or remove some history"
	case errors.Is(err, ai.ErrRateLimited):
		return "the AI provider is rate-limiting requests — wait a moment and try again"
	case errors.Is(err, ai.ErrCircuitOpen):
		return "the AI provider is temporarily unavailable after repeated failures — wait a moment and try again"
	case errors.Is(err, ai.ErrProviderDown):
		return "the AI provider is temporarily unavailable — try again shortly"
	case errors.Is(err, ai.ErrInsufficientBalance):
		return "the AI provider account has insufficient balance — add credits or switch providers"
	case errors.Is(err, ai.ErrApiKeyInvalid):
		return "the API key for this provider is invalid — check it in Settings → AI Providers"
	case isStreamTruncation(err):
		return "the response was interrupted before it finished — try again"
	}
	return err.Error()
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
		TokensIn:  c.tokensIn,
		TokensOut: c.tokensOut,
		Events:    c.snapshotEvents(),
	}
}
