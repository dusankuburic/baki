package search

import (
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"Click on element", []string{"click", "on", "element"}},
		{"WebAutomation.Click.Click", []string{"web", "automation", "click"}},
		{"getExcelValue", []string{"get", "excel", "value"}},
		{"  spaces  ", []string{"spaces"}},
		{"with_underscore-hyphen", []string{"with", "underscore", "hyphen"}},
		{"", nil},
		{"a", nil},
	}

	for _, tt := range tests {
		got := tokenize(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i, v := range got {
			if v != tt.want[i] {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", tt.input, i, v, tt.want[i])
			}
		}
	}
}

func TestTokenizeDeduplicates(t *testing.T) {
	got := tokenize("Click click CLICK")
	counts := make(map[string]int)
	for _, tok := range got {
		counts[tok]++
	}
	for tok, count := range counts {
		if count > 1 {
			t.Errorf("duplicate token %q found %d times", tok, count)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"abc", "abcd", 1},
		{"kitten", "sitting", 3},
		{"click", "clikc", 2},
		{"saturday", "sunday", 3},
		{"caf\u00e9", "cafe", 1},
		{"\u00fcber", "uber", 1},
	}

	for _, tt := range tests {
		got := Levenshtein(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("Levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestLevenshteinSymmetry(t *testing.T) {
	pairs := [][2]string{
		{"abc", "def"},
		{"click", "clikc"},
		{"longerstring", "short"},
	}
	for _, p := range pairs {
		a := Levenshtein(p[0], p[1])
		b := Levenshtein(p[1], p[0])
		if a != b {
			t.Errorf("Levenshtein not symmetric: (%q,%q)=%d vs (%q,%q)=%d", p[0], p[1], a, p[1], p[0], b)
		}
	}
}
