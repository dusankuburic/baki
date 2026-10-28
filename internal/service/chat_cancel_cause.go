package service

import "pad-analyzer/internal/ai"

// R-observability-4: a stream's context is cancelled with its stopper's reason as the cause (streamCtl.cancelCause),
// so the AI layer counts a provider call the stop cut short under pad_ai_requests_cancelled_total{reason} instead of
// as a provider error. The stream's 10-minute cap is the context's own deadline (reason deadline).

// streamCancelReason maps a stream's cancel code (cancelWithCode; "" is the user's stop) to the AI layer's reason.
func streamCancelReason(code string) ai.CancelReason {
	switch code {
	case ChatErrIdleTimeout:
		return ai.CancelIdle
	case ChatErrClientGone:
		return ai.CancelSubscriberGone
	case ChatErrServerRestarting, ChatErrInterrupted:
		// The server stopping, or a replica fenced off from the shared state (chat_replicas.go): either way the
		// server, not the user or the provider, ended the stream.
		return ai.CancelShutdown
	}
	return ai.CancelClient
}

// cancelCause is the cause the stream's context is cancelled with: the reason of the first cancellation asked for
// (cancelWithCode records it before it cancels); a cancel with none asked for (the turn's own cleanup) is the client's.
func (c *streamCtl) cancelCause() error {
	if c == nil {
		return ai.CancelCause(ai.CancelClient)
	}
	code, _ := c.stopRequested()
	return ai.CancelCause(streamCancelReason(code))
}
