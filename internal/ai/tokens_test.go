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
		{"Hello world this is a test", 5, 15},
		{strings.Repeat("a", 350), 95, 105},
	}
	for _, tt := range tests {
		got := EstimateTokens(tt.text)
		if got < tt.minToken || got > tt.maxToken {
			t.Errorf("EstimateTokens(%q) = %d, want between %d and %d", tt.text, got, tt.minToken, tt.maxToken)
		}
	}
}

func TestTruncateToTokenLimit(t *testing.T) {
	text := strings.Repeat("a", 100)
	result := TruncateToTokenLimit(text, 10)
	maxChars := int(float64(10) * 3.5)
	if len(result) > maxChars {
		t.Errorf("truncated text too long: %d > %d", len(result), maxChars)
	}
	if len(text) <= maxChars {
		t.Error("should not have truncated short text")
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
