package ai

import (
	"strings"
	"testing"
)

// TestGeminiSSE_ThinkingTokensBilled pins R4a: Gemini 2.5-series models bill
// thoughtsTokenCount as OUTPUT separately from candidatesTokenCount, which
// excludes it — parsing only candidates undercounted output spend (and the
// daily budget with it) for every thinking model.
func TestGeminiSSE_ThinkingTokensBilled(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"thinking hard"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"final"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":40,"thoughtsTokenCount":60}}`,
		"",
	}, "\n")

	var got Chunk
	err := parseGeminiSSE(strings.NewReader(sse), func(c Chunk) {
		if c.Done {
			got = c
		}
	})
	if err != nil {
		t.Fatalf("parseGeminiSSE: %v", err)
	}
	if !got.Done {
		t.Fatal("no Done chunk parsed")
	}
	if got.TokensIn != 100 {
		t.Errorf("TokensIn = %d, want 100", got.TokensIn)
	}
	if got.TokensOut != 100 { // 40 candidates + 60 thoughts
		t.Errorf("TokensOut = %d, want 100 (candidates 40 + thoughts 60)", got.TokensOut)
	}
}

// TestHeuristicTokenEstimate_CJK pins the R4b fallback: without a tokenizer,
// CJK text estimates ~1 token per rune instead of /3.5-or-/4 — a uniform
// divisor undercounted Chinese/Japanese/Korean 3-4×, letting oversized
// prompts pass the context-window guard and 400 at the provider.
func TestHeuristicTokenEstimate_CJK(t *testing.T) {
	cjk := strings.Repeat("密", 100) // 100 CJK ideographs
	latin := strings.Repeat("a", 100)

	// CJK at ~1/rune; Latin at /3.5 (generic) and /4 (openai) respectively.
	if got := heuristicTokenEstimate(cjk, 3.5); got != 100 {
		t.Errorf("generic CJK estimate = %d, want 100", got)
	}
	if got := heuristicTokenEstimate(latin, 3.5); got != 28 {
		t.Errorf("generic Latin estimate = %d, want 28", got)
	}
	if got := heuristicTokenEstimate(latin, 4.0); got != 25 {
		t.Errorf("openai Latin estimate = %d, want 25", got)
	}
	// Mixed: 50 CJK + 70 Latin at /3.5 → 50 + 20.
	mixed := strings.Repeat("密", 50) + strings.Repeat("a", 70)
	if got := heuristicTokenEstimate(mixed, 3.5); got != 50+20 {
		t.Errorf("mixed estimate = %d, want 70", got)
	}
	// Hiragana/Katakana/Hangul covered too.
	for _, r := range []rune{'あ', 'ア', '한'} {
		if got := heuristicTokenEstimate(string(r), 3.5); got != 1 {
			t.Errorf("rune %q estimate = %d, want 1", r, got)
		}
	}
}
