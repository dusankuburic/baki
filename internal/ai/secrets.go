package ai

import (
	"math"
	"regexp"
	"strings"
	"unicode"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)\s*[:=]\s*\S+`),
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9\-._~+/]+=*`),
	regexp.MustCompile(`(?i)(sk|pk|tk)_[a-zA-Z0-9]{20,}`),
}

func isPotentialSecret(value string) bool {
	if len(value) < 8 {
		return false
	}
	lower := strings.ToLower(value)
	keywords := []string{"password", "passwd", "secret", "token", "api_key", "apikey", "private_key", "access_key"}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	for _, pat := range secretPatterns {
		if pat.MatchString(value) {
			return true
		}
	}
	return shannonEntropy(value) > 4.0
}

func maskSecret(value string) string {
	if len(value) <= 4 {
		return "****"
	}
	return value[:2] + strings.Repeat("*", len(value)-4) + value[len(value)-2:]
}

func shannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	freq := make(map[rune]int)
	for _, r := range s {
		freq[r]++
	}
	var entropy float64
	total := float64(len(s))
	for _, count := range freq {
		p := float64(count) / total
		if p > 0 {
			entropy -= p * math.Log2(p)
		}
	}
	return entropy
}

func hasHighEntropyPrintable(s string) bool {
	printable := 0
	for _, r := range s {
		if unicode.IsPrint(r) && !unicode.IsSpace(r) {
			printable++
		}
	}
	return printable >= 8
}
