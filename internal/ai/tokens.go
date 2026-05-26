package ai

import "unicode/utf8"

func EstimateTokensGeneric(text string) int {
	charCount := utf8.RuneCountInString(text)
	return int(float64(charCount) / 3.5)
}

func EstimateTokensClaude(text string) int {
	return EstimateTokensGeneric(text)
}

func EstimateTokensOpenAI(text string) int {
	charCount := utf8.RuneCountInString(text)
	return int(float64(charCount) / 4.0)
}

func EstimateTokensGemini(text string) int {
	return EstimateTokensGeneric(text)
}

func EstimateTokens(text string) int {
	return EstimateTokensGeneric(text)
}

func TruncateToTokens(text string, maxTokens int) string {
	return TruncateToTokenLimit(text, maxTokens)
}

func TruncateToTokenLimit(text string, maxTokens int) string {
	maxChars := int(float64(maxTokens) * 3.5)
	if utf8.RuneCountInString(text) <= maxChars {
		return text
	}

	count := 0
	for i, _ := range text {
		if count == maxChars-3 {
			return text[:i] + "..."
		}
		count++
	}

	return text
}
