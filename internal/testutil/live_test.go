package testutil

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
)

// The live harness's cap is unit-tested here, offline: the live suites themselves only run with keys.

// A reasoning model counts its hidden thinking against the same output cap as the answer: the per-call cap gets the
// reasoning allowance on top and the effort is pinned to "low", so a 64-token smoke call does not end with nothing
// but thoughts; a non-reasoning model keeps the bare cap and no effort (a reasoning_effort it would reject).
func TestLiveBudget_ReasoningModelsGetTheAllowanceAndLowEffort(t *testing.T) {
	for _, tc := range []struct {
		model      string
		requested  int
		effort     string
		wantTokens int
		wantEffort string
	}{
		{"gpt-4o-mini", 0, "", 64, ""},
		{"claude-haiku-4-5", 4096, "", 64, ""},
		{"gemini-2.5-flash", 0, "", 64 + LiveReasoningAllowance, "low"},
		{"gemini-2.5-pro", 16384, "high", 64 + LiveReasoningAllowance, "low"},
		{"grok-3-mini", 0, "", 64 + LiveReasoningAllowance, "low"},
		{"o4-mini", 32, "", 32, "low"},
		{"gpt-5-mini", 0, "medium", 64 + LiveReasoningAllowance, "low"},
	} {
		inner := &FakeProvider{}
		p := NewLiveBudget("t", 1_000_000, 10).Wrap(inner, 64)
		if err := p.Stream(context.Background(), ai.Request{Model: tc.model, MaxTokens: tc.requested, ReasoningEffort: tc.effort}, func(ai.Chunk) {}); err != nil {
			t.Fatal(err)
		}
		got := inner.Requests()[0]
		if got.MaxTokens != tc.wantTokens || got.ReasoningEffort != tc.wantEffort {
			t.Errorf("%s (asked %d, effort %q): sent max %d effort %q, want %d %q",
				tc.model, tc.requested, tc.effort, got.MaxTokens, got.ReasoningEffort, tc.wantTokens, tc.wantEffort)
		}
	}
}

// The ceiling stays meaningful: once the reported usage reaches it, every further call is refused before it is made.
func TestLiveBudget_RefusesPastTheCeiling(t *testing.T) {
	inner := &FakeProvider{Turns: []FakeTurn{{Text: "a", TokensIn: 900, TokensOut: 200}}}
	b := NewLiveBudget("t", 1000, 10)
	p := b.Wrap(inner, 64)
	if err := p.Stream(context.Background(), ai.Request{Model: "gpt-4o-mini"}, func(ai.Chunk) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), ai.Request{Model: "gpt-4o-mini"}); !errors.Is(err, ErrLiveBudgetExhausted) {
		t.Errorf("the call past 1100/1000 tokens was not refused: %v", err)
	}
	if inner.Calls() != 1 {
		t.Errorf("the inner provider saw %d stream calls, want 1", inner.Calls())
	}
	if tokens, calls := b.Used(); tokens != 1100 || calls != 1 {
		t.Errorf("used %d tokens in %d calls, want 1100 in 1", tokens, calls)
	}
}

// Every provider's way of saying "stopped at the output cap" is a truncation, a clean stop is not.
func TestLiveTruncated(t *testing.T) {
	for finish, want := range map[string]bool{
		"length": true, "max_tokens": true, "MAX_TOKENS": true, "max_output_tokens": true,
		"model_context_window_exceeded": true, "stop": false, "end_turn": false, "STOP": false, "tool_calls": false, "": false,
	} {
		if got := LiveTruncated(finish); got != want {
			t.Errorf("LiveTruncated(%q) = %v, want %v", finish, got, want)
		}
	}
}

// Failure text never carries credential material or an endpoint: the key itself, every value of a credential
// document, and any URL (a *url.Error prints the request URL).
func TestLiveProvider_Redact(t *testing.T) {
	doc := `{"endpoint":"https://my-secret-res.services.ai.azure.com","auth":{"mode":"api_key","key":"abcd1234efgh5678"},"chatDeployment":"gpt-4.1-team"}`
	lp := LiveProvider{Key: doc}
	uerr := &url.Error{Op: "Post", URL: "https://my-secret-res.services.ai.azure.com/openai/v1/chat/completions?api-version=x", Err: errors.New("dial tcp: i/o timeout")}
	for _, s := range []string{
		uerr.Error(),
		"status 401 for key abcd1234efgh5678 on my-secret-res.services.ai.azure.com",
		"credential " + doc,
	} {
		got := lp.Redact(s)
		for _, leak := range []string{"my-secret-res", "abcd1234efgh5678", "services.ai.azure.com", "https://"} {
			if strings.Contains(got, leak) {
				t.Errorf("Redact(%q) = %q still carries %q", s, got, leak)
			}
		}
	}
	plain := LiveProvider{Key: "sk-ant-api03-VERYSECRET"}
	if got := plain.Redact("auth failed for sk-ant-api03-VERYSECRET at https://api.anthropic.com/v1/messages"); strings.Contains(got, "VERYSECRET") || strings.Contains(got, "https://") {
		t.Errorf("a bare key leaks: %q", got)
	}
	if got := plain.Redact("Chat: 42 in / 7 out"); got != "Chat: 42 in / 7 out" {
		t.Errorf("Redact changed text with nothing to hide: %q", got)
	}
}
