package ai

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestEmbeddingFailureCause_UnwrapsTerminal: a 429 that exhausted the split
// cascade is opaque to errors.Is by design (the retry wrapper must not
// re-run it); EmbeddingFailureCause exposes the cause — through outer %w
// wrapping — so the HTTP layer can classify it. Non-terminal errors pass
// through unchanged.
func TestEmbeddingFailureCause_UnwrapsTerminal(t *testing.T) {
	rl := &RateLimitError{RetryAfter: time.Second}
	err := fmt.Errorf("openai: %w", &embedTerminalError{cause: rl})
	if errors.Is(err, ErrRateLimited) {
		t.Fatal("fixture: a terminal error must hide its cause from errors.Is")
	}

	got := EmbeddingFailureCause(err)
	if !errors.Is(got, ErrRateLimited) {
		t.Errorf("cause = %v, want one satisfying errors.Is(_, ErrRateLimited)", got)
	}
	var gotRL *RateLimitError
	if !errors.As(got, &gotRL) || gotRL.RetryAfter != time.Second {
		t.Errorf("cause = %v, want the *RateLimitError with its Retry-After hint", got)
	}

	plain := fmt.Errorf("plain: %w", ErrApiKeyInvalid)
	if got := EmbeddingFailureCause(plain); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Errorf("a non-terminal error must be returned unchanged, got %v", EmbeddingFailureCause(plain))
	}
	if EmbeddingFailureCause(nil) != nil {
		t.Error("nil must stay nil")
	}
	if EmbeddingFailureCause(&embedTerminalError{}) == nil {
		t.Error("a terminal error without a cause must not become nil")
	}
}
