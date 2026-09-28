package ai

import "errors"

// EmbeddingFailureCause returns the provider error a split cascade ended on.
// embedTerminalError hides it from errors.Is on purpose (the outer
// RetryingProvider must not re-run the — already locally retried — cascade);
// callers that only classify the failure AFTER Embed has returned (the
// knowledge-base HTTP layer mapping a 401/403/429/5xx to a machine code) use
// this to see the real cause. Any other error, including nil, is returned
// unchanged, so it is safe to apply to every Embed error.
func EmbeddingFailureCause(err error) error {
	var t *embedTerminalError
	if errors.As(err, &t) && t.cause != nil {
		return t.cause
	}
	return err
}
