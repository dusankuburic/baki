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
		// The offline BPE loader embeds the vocabulary in the binary; without it tiktoken-go downloads
		// cl100k_base remotely on first use, which blocks (and fails) in egress-restricted containers.
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

// encodeText is the tokenizer's Encode — the one BPE pass every estimate and
// truncation goes through, so tests can count the bytes it sees.
var encodeText = func(tkm *tiktoken.Tiktoken, text string) []int {
	return tkm.Encode(text, nil, nil)
}

func estimateTokensGeneric(text string) int {
	tkm := getTokenizer()
	if tkm != nil {
		tokens := encodeText(tkm, text)
		return len(tokens)
	}

	return heuristicTokenEstimate(text, 3.5)
}

// heuristicTokenEstimate is the tokenizer-unavailable fallback: Latin-ish text runs ~divisor runes per token,
// but CJK ideographs cost ~1 token PER rune — a uniform divisor undercounted CJK ~3-4×.
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
		tokens := encodeText(tkm, text)
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

// truncatePrefixBytesPerToken sizes the prefix TruncateToTokenLimit encodes
// first: maxTokens × 16 bytes, four times the ~4 bytes a cl100k token covers
// on prose, so a multi-megabyte text is never tokenised whole.
const truncatePrefixBytesPerToken = 16

// TruncateToTokenLimit returns text when it is at most maxTokens tokens, else its first maxTokens-3 tokens
// plus "...". A text longer than maxTokens × truncatePrefixBytesPerToken bytes is cut at a pre-token boundary
// (tokenStablePrefix) and only that prefix encoded — its tokens are exactly the whole text's first tokens, so
// the result matches the full encode whenever the prefix overflows.
func TruncateToTokenLimit(text string, maxTokens int) string {
	tkm := getTokenizer()
	if tkm != nil {
		if p := tokenStablePrefix(text, maxTokens*truncatePrefixBytesPerToken); p > 0 {
			// text[p:] is not empty, so the whole text has at least one
			// token more than the prefix: maxTokens tokens in the prefix
			// prove the overflow.
			if tokens := encodeText(tkm, text[:p]); len(tokens) >= maxTokens {
				return truncateTokens(tkm, tokens, maxTokens)
			}
		}
		tokens := encodeText(tkm, text)
		if len(tokens) <= maxTokens {
			return text
		}
		return truncateTokens(tkm, tokens, maxTokens)
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

// truncateTokens is the over-budget half of TruncateToTokenLimit: the first
// maxTokens-3 of tokens decoded, plus the "..." marker.
func truncateTokens(tkm *tiktoken.Tiktoken, tokens []int, maxTokens int) string {
	if maxTokens <= 3 {
		return "..."
	}
	return tkm.Decode(tokens[:maxTokens-3]) + "..."
}

// tokenStablePrefix returns a cut p ≤ maxBytes such that encoding text[:p] yields exactly the first tokens of
// encoding text, or 0 when the text is not longer than maxBytes or no such cut exists. cl100k's pre-tokeniser
// byte-pair-encodes each matched piece on its own, so a prefix ending on a piece boundary tokenises to a
// prefix of the whole; a '\n' followed by spaces/tabs and a printable ASCII byte is such a boundary (the piece
// holding the '\n' is the newline-terminated tail of one of the two line-break alternatives). Searched
// backwards from maxBytes.
func tokenStablePrefix(text string, maxBytes int) int {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return 0
	}
	// maxIndent bounds the spaces/tabs skipped after a candidate '\n'; a longer run is not worth a scan.
	const maxIndent = 256
	for p := maxBytes; p > 0; p-- {
		if text[p-1] != '\n' {
			continue
		}
		q := p
		for q < len(text) && q-p < maxIndent && (text[q] == ' ' || text[q] == '\t') {
			q++
		}
		if q < len(text) && text[q] > ' ' && text[q] < 0x7f {
			return p
		}
	}
	return 0
}
