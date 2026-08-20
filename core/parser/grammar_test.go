package parser

import (
	"strings"
	"testing"
)

// maskStrings replaces the content of string literals with spaces so that
// variable extraction and pattern matching ignore string values.

func TestMaskStrings_SingleQuote(t *testing.T) {
	input := "SET X TO 'hello world'"
	got := maskStrings(input)
	// The content (and quotes) inside '...' must be masked.
	if strings.Contains(got, "hello") {
		t.Errorf("single-quoted content should be masked; got %q", got)
	}
	// Non-string parts must be preserved.
	if !strings.Contains(got, "SET X TO") {
		t.Errorf("non-string part should survive masking; got %q", got)
	}
}

func TestMaskStrings_DoubleQuote(t *testing.T) {
	input := `SET X TO "hello world"`
	got := maskStrings(input)
	if strings.Contains(got, "hello") {
		t.Errorf("double-quoted content should be masked; got %q", got)
	}
	if !strings.Contains(got, "SET X TO") {
		t.Errorf("non-string part should survive masking; got %q", got)
	}
}

func TestMaskStrings_TripleQuote(t *testing.T) {
	input := "SET X TO '''multi\nline\nvalue'''"
	got := maskStrings(input)
	if strings.Contains(got, "multi") || strings.Contains(got, "line") || strings.Contains(got, "value") {
		t.Errorf("triple-quoted content should be masked; got %q", got)
	}
}

func TestMaskStrings_InterpolatedTripleQuote(t *testing.T) {
	input := `SET X TO $'''hello %Var% world'''`
	got := maskStrings(input)
	if strings.Contains(got, "hello") || strings.Contains(got, "world") {
		t.Errorf("interpolated triple-quoted content should be masked; got %q", got)
	}
}

func TestMaskStrings_NoStrings(t *testing.T) {
	input := "SET X TO 42"
	got := maskStrings(input)
	if got != input {
		t.Errorf("no string literals: expected unchanged output, got %q", got)
	}
}

func TestMaskStrings_EmptyString(t *testing.T) {
	got := maskStrings("")
	if got != "" {
		t.Errorf("empty input: expected empty output, got %q", got)
	}
}

func TestMaskStrings_MultipleStrings(t *testing.T) {
	input := "A 'foo' B 'bar'"
	got := maskStrings(input)
	if strings.Contains(got, "foo") || strings.Contains(got, "bar") {
		t.Errorf("both string literals should be masked; got %q", got)
	}
	// The structural characters A, B should still be present.
	if !strings.HasPrefix(strings.TrimSpace(got), "A") {
		t.Errorf("expected output to start with 'A'; got %q", got)
	}
}

// TestMaskStrings_LiteralsCollapseToSpace pins the masking contract: each
// literal collapses to a single space (the masked text is only ever used for
// regex matching — offsets never map back — so preserving literal length is
// not required, and collapsing avoids per-rune allocations).
func TestMaskStrings_LiteralsCollapseToSpace(t *testing.T) {
	input := "X 'abc' Y"
	got := maskStrings(input)
	if got != "X   Y" {
		t.Errorf("maskStrings(%q) = %q, want %q", input, got, "X   Y")
	}
}

func TestMaskStrings_EmptyQuotedString(t *testing.T) {
	// '' is an empty single-quoted string.
	input := "SET X TO ''"
	got := maskStrings(input)
	if !strings.Contains(got, "SET X TO") {
		t.Errorf("prefix should survive; got %q", got)
	}
}

func TestMaskStrings_UnclosedSingleQuote(t *testing.T) {
	// Unclosed quote: masks to end of input without panicking.
	input := "SET X TO 'unclosed"
	got := maskStrings(input)
	if strings.Contains(got, "unclosed") {
		t.Errorf("unclosed literal content should be masked; got %q", got)
	}
	if !strings.HasPrefix(got, "SET X TO") {
		t.Errorf("prefix should survive; got %q", got)
	}
}
