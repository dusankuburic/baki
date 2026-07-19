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

	charCount := utf8.RuneCountInString(text)
	return int(float64(charCount) / 3.5)
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

	charCount := utf8.RuneCountInString(text)
	return int(float64(charCount) / 4.0)
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
