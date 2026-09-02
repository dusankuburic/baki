package ai

import (
	"sync"
	"unicode/utf8"

	"github.com/pkoukk/tiktoken-go"
	tiktokenloader "github.com/pkoukk/tiktoken-go-loader"
	"pad-core/logger"
)

var (
	tokenizer     *tiktoken.Tiktoken
	tokenizerOnce sync.Once
)

func getTokenizer() *tiktoken.Tiktoken {
	tokenizerOnce.Do(func() {
		// Use the offline BPE loader so the vocabulary is embedded in the binary.
		// Without this, tiktoken-go downloads cl100k_base from a remote URL on
		// first use, which blocks (and fails) in egress-restricted containers.
		tiktoken.SetBpeLoader(tiktokenloader.NewOfflineLoader())
		tkm, err := tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			logger.Error("Failed to initialize tiktoken, falling back to heuristic", "error", err)
			tokenizer = nil
			return
		}
		tokenizer = tkm
	})
	return tokenizer
}

func estimateTokensGeneric(text string) int {
	tkm := getTokenizer()
	if tkm != nil {
		tokens := tkm.Encode(text, nil, nil)
		return len(tokens)
	}

	return heuristicTokenEstimate(text, 3.5)
}

// heuristicTokenEstimate is the tokenizer-unavailable fallback: Latin-ish
// text runs ~divisor runes per token, but CJK ideographs cost ~1 token PER
// rune — a uniform /3.5 or /4 undercounted Chinese/Japanese/Korean ~3-4×,
// letting oversized prompts pass window guards and surface as provider 400s.
// CJK runes are counted at 1 token each; the rest at divisor.
func heuristicTokenEstimate(text string, divisor float64) int {
	cjk, other := 0, 0
	for _, r := range text {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF, // CJK Unified Ideographs
			r >= 0x3400 && r <= 0x4DBF, // Extension A
			r >= 0x3040 && r <= 0x30FF, // Hiragana + Katakana
			r >= 0xAC00 && r <= 0xD7AF, // Hangul syllables
			r >= 0xF900 && r <= 0xFAFF: // CJK Compatibility Ideographs
			cjk++
		default:
			other++
		}
	}
	return cjk + int(float64(other)/divisor)
}

func EstimateTokensClaude(text string) int {
	return estimateTokensGeneric(text)
}

func EstimateTokensOpenAI(text string) int {
	tkm := getTokenizer()
	if tkm != nil {
		tokens := tkm.Encode(text, nil, nil)
		return len(tokens)
	}

	return heuristicTokenEstimate(text, 4.0)
}

func EstimateTokensGemini(text string) int {
	return estimateTokensGeneric(text)
}

func EstimateTokens(text string) int {
	return estimateTokensGeneric(text)
}

func TruncateToTokenLimit(text string, maxTokens int) string {
	tkm := getTokenizer()
	if tkm != nil {
		tokens := tkm.Encode(text, nil, nil)
		if len(tokens) <= maxTokens {
			return text
		}
		if maxTokens <= 3 {
			return "..."
		}
		truncatedTokens := tokens[:maxTokens-3]
		return tkm.Decode(truncatedTokens) + "..."
	}

	// Fallback heuristic
	maxChars := int(float64(maxTokens) * 3.5)
	if utf8.RuneCountInString(text) <= maxChars {
		return text
	}

	count := 0
	for i := range text {
		if count == maxChars-3 {
			return text[:i] + "..."
		}
		count++
	}

	return text
}
