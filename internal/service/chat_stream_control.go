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

// streamCtl holds the mutable, concurrency-safe state of one in-flight chat
// stream: the accumulated (scrubbed) output buffer, terminal status, and the
// cancellation plumbing shared between the stream worker goroutine, the
// watchdog, and any client-facing begin/cancel/resume call.
type streamCtl struct {
	cancel context.CancelFunc
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
// deliberate cancellation surfaces its stored reason, and the stream-duration
// timeout gets a readable message — everything else is the error as-is.
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
	if errors.Is(err, ai.ErrContextLimit) {
		return "conversation is too long for this model's context window — start a new conversation or remove some history"
	}
	return err.Error()
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
	}
}
