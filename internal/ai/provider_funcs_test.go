package ai

import (
	"strings"
	"testing"
)

// ---- EstimateTokensClaude / EstimateTokensOpenAI / EstimateTokensGemini ----

func TestEstimateTokensClaude_Basic(t *testing.T) {
	text := strings.Repeat("a", 350) // 350 chars → ~100 tokens at 3.5 chars/token
	got := EstimateTokensClaude(text)
	if got < 90 || got > 110 {
		t.Errorf("EstimateTokensClaude: expected ~100 tokens, got %d", got)
	}
}

func TestEstimateTokensOpenAI_DifferentRatio(t *testing.T) {
	text := strings.Repeat("a", 400) // 400 chars → ~100 tokens at 4.0 chars/token
	got := EstimateTokensOpenAI(text)
	if got < 90 || got > 110 {
		t.Errorf("EstimateTokensOpenAI: expected ~100 tokens, got %d", got)
	}
	// OpenAI uses 4.0 divisor, Claude uses 3.5 — same text should produce fewer tokens
	claude := EstimateTokensClaude(text)
	if got >= claude {
		t.Errorf("OpenAI (4.0 divisor) should return fewer tokens than Claude (3.5) for same text: openai=%d claude=%d", got, claude)
	}
}

func TestEstimateTokensGemini_SameAsClaude(t *testing.T) {
	text := "some sample text for gemini"
	if EstimateTokensGemini(text) != EstimateTokensClaude(text) {
		t.Error("Gemini and Claude use the same generic estimator, expected equal results")
	}
}

// ---- orDefault -------------------------------------------------------------

func TestOrDefault_Positive(t *testing.T) {
	if got := orDefault(5, 10); got != 5 {
		t.Errorf("orDefault(5, 10) = %d, want 5", got)
	}
}

func TestOrDefault_Zero(t *testing.T) {
	if got := orDefault(0, 10); got != 10 {
		t.Errorf("orDefault(0, 10) = %d, want 10", got)
	}
}

func TestOrDefault_Negative(t *testing.T) {
	if got := orDefault(-3, 10); got != 10 {
		t.Errorf("orDefault(-3, 10) = %d, want 10", got)
	}
}

// ---- convertMessages -------------------------------------------------------

func TestConvertMessages_Empty(t *testing.T) {
	out := convertMessages(nil, strings.ToUpper)
	if len(out) != 0 {
		t.Errorf("expected empty output for nil input, got %d", len(out))
	}
}

func TestConvertMessages_MapsRoles(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "world"},
	}
	out := convertMessages(msgs, func(role string) string {
		if role == "assistant" {
			return "model"
		}
		return role
	})
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
	if out[0].Role != "user" || out[0].Content != "hello" {
		t.Errorf("first message wrong: %+v", out[0])
	}
	if out[1].Role != "model" || out[1].Content != "world" {
		t.Errorf("assistant role not mapped to model: %+v", out[1])
	}
}

// ---- ProviderError ---------------------------------------------------------

func TestProviderError_Format(t *testing.T) {
	err := ProviderError("openai", 429, "rate limited")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	s := err.Error()
	if !strings.Contains(s, "openai") {
		t.Errorf("expected provider name in error, got: %q", s)
	}
	if !strings.Contains(s, "429") {
		t.Errorf("expected status code in error, got: %q", s)
	}
	if !strings.Contains(s, "rate limited") {
		t.Errorf("expected message in error, got: %q", s)
	}
}

// ---- hasHighEntropyPrintable -----------------------------------------------

func TestHasHighEntropyPrintable_OnlySpaces(t *testing.T) {
	// Spaces are printable but are unicode.IsSpace → not counted.
	if hasHighEntropyPrintable("        ") { // 8 spaces
		t.Error("expected false: spaces don't count as non-space printable")
	}
}

func TestHasHighEntropyPrintable_FewPrintable(t *testing.T) {
	if hasHighEntropyPrintable("abc\n\t\r") { // only 3 non-space printable
		t.Error("expected false for fewer than 8 printable chars")
	}
}

func TestHasHighEntropyPrintable_Enough(t *testing.T) {
	if !hasHighEntropyPrintable("abcdefgh") { // exactly 8 non-space printable
		t.Error("expected true for 8 printable non-space chars")
	}
}

func TestHasHighEntropyPrintable_MixedWithSpaces(t *testing.T) {
	// 8 printable non-space + lots of spaces → still true
	if !hasHighEntropyPrintable("a b c d e f g h") {
		t.Error("expected true: 8 non-space printable chars present despite spaces")
	}
}
