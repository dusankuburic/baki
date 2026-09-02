package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
)

// TestFailureMessage_FriendlyProviderCopy pins the U3 mapping: the common
// provider sentinels surface as actionable copy instead of developer-speak
// ("provider circuit open: too many recent failures" told the user nothing).
func TestFailureMessage_FriendlyProviderCopy(t *testing.T) {
	ctl := &streamCtl{}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"rate limited", ai.ErrRateLimited, "the AI provider is rate-limiting requests"},
		{"circuit open", ai.ErrCircuitOpen, "temporarily unavailable after repeated failures"},
		{"provider down", fmt.Errorf("wrapped: %w", ai.ErrProviderDown), "temporarily unavailable"},
		{"insufficient balance", ai.ErrInsufficientBalance, "insufficient balance"},
		{"invalid key", ai.ErrApiKeyInvalid, "Settings → AI Providers"},
		{"truncated stream", fmt.Errorf("openai stream truncated before terminal marker: %w", errors.New("unexpected EOF")), "interrupted before it finished"},
		{"malformed stream", fmt.Errorf("gemini stream had 3 undecodable event(s) and no terminal marker"), "interrupted before it finished"},
		{"context limit", ai.ErrContextLimit, "context window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ctl.failureMessage(context.Background(), tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("failureMessage(%v) = %q, want substring %q", tc.err, got, tc.want)
			}
		})
	}
	// Unknown errors still pass through verbatim.
	if got := ctl.failureMessage(context.Background(), errors.New("something novel")); got != "something novel" {
		t.Errorf("unknown error should pass through, got %q", got)
	}
}
