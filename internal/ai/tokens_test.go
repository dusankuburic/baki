package ai

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		text     string
		minToken int
		maxToken int
	}{
		{"", 0, 0},
		{"Hello", 1, 2},
		{"Hello world this is a test", 5, 8},
		// A long sentence rather than repeated chars to avoid BPE extreme compression
		{"The quick brown fox jumps over the lazy dog repeatedly to test the token limits of the encoder.", 15, 25},
	}
	for _, tt := range tests {
		got := EstimateTokens(tt.text)
		if got < tt.minToken || got > tt.maxToken {
			t.Errorf("EstimateTokens(%q) = %d, want between %d and %d", tt.text, got, tt.minToken, tt.maxToken)
		}
	}
}

func TestTruncateToTokenLimit(t *testing.T) {
	text := "This is a slightly longer sentence meant to be truncated at exactly ten tokens."
	// 14 words, ~15 tokens
	result := TruncateToTokenLimit(text, 10)
	
	// The result should end with "..."
	if !strings.HasSuffix(result, "...") {
		t.Errorf("expected truncated text to end with '...', got: %q", result)
	}

	// The truncated text should be ~10 tokens
	truncatedTokens := EstimateTokens(result)
	if truncatedTokens > 10 {
		t.Errorf("truncated text has too many tokens: got %d, max 10", truncatedTokens)
	}

	short := "hello"
	if got := TruncateToTokenLimit(short, 1000); got != short {
		t.Errorf("short text should not be truncated")
	}
}

func TestTruncateToTokens(t *testing.T) {
	text := "hello world"
	result := TruncateToTokens(text, 1000)
	if result != text {
		t.Errorf("should not truncate short text")
	}
}
