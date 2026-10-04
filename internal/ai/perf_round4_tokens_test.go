package ai

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/pkoukk/tiktoken-go"
)

// refTruncate is TruncateToTokenLimit as it was before the prefix path: the
// whole text encoded, the first maxTokens-3 tokens decoded plus "...".
func refTruncate(t *testing.T, tkm *tiktoken.Tiktoken, text string, maxTokens int) string {
	t.Helper()
	return refTruncateTokens(tkm, text, tkm.Encode(text, nil, nil), maxTokens)
}

// refTruncateTokens is refTruncate for a text whose whole-text tokens are
// already known.
func refTruncateTokens(tkm *tiktoken.Tiktoken, text string, tokens []int, maxTokens int) string {
	if len(tokens) <= maxTokens {
		return text
	}
	if maxTokens <= 3 {
		return "..."
	}
	return tkm.Decode(tokens[:maxTokens-3]) + "..."
}

// countEncodedBytes routes encodeText through a byte counter for the test.
func countEncodedBytes(t *testing.T) *int {
	t.Helper()
	orig := encodeText
	n := new(int)
	encodeText = func(tkm *tiktoken.Tiktoken, text string) []int {
		*n += len(text)
		return orig(tkm, text)
	}
	t.Cleanup(func() { encodeText = orig })
	return n
}

func requireTokenizer(t *testing.T) *tiktoken.Tiktoken {
	t.Helper()
	tkm := getTokenizer()
	if tkm == nil {
		t.Skip("cl100k tokenizer unavailable")
	}
	return tkm
}

// The truncation of a text longer than the prefix cap is computed from the
// prefix alone and must be byte-identical to the old whole-text encode —
// for every input, including the ones where the prefix cannot decide and
// the whole text is encoded after all.
func TestTruncateToTokenLimit_PrefixPathIsExact(t *testing.T) {
	tkm := requireTokenizer(t)
	// repeatTo repeats unit until the text is at least n bytes.
	repeatTo := func(unit string, n int) string {
		return strings.Repeat(unit, n/len(unit)+1)
	}
	// Every shape is longer than the top budget's prefix cap (budget × 16
	// bytes), so each exercises the prefix path at every budget listed.
	cases := []struct {
		name    string
		text    string
		budgets []int
	}{
		{"pad crlf", padSource(80 << 10), []int{4000, 1200, 100, 4, 3, 1, 0}},
		{"prose lf", repeatTo("The quick brown fox jumps over the lazy dog. It's 12345 o'clock, isn't it?\nAnother line follows here.\n", 80<<10), []int{4000, 1200, 100}},
		{"indented tabs", repeatTo("IF %x% > 10 THEN\n\t\tSET y TO $'''a b c'''\n\t\tEND\n", 24<<10), []int{1200, 100, 4, 3, 1, 0}},
		{"trailing spaces", repeatTo("line with trailing spaces   \n   \n\t\n next-line\n", 24<<10), []int{1200, 100, 3}},
		{"cjk", repeatTo("流程分析：第一步\n    第二步 — 检查错误处理。\n", 24<<10), []int{1200, 100, 3}},
		{"emoji", repeatTo("ok 👍 done ✅\n  → next 🚀\n", 24<<10), []int{1200, 100, 3}},
		{"no line breaks", repeatTo("word ", 24<<10), []int{1200, 100, 3}},
		{"blank-line heavy", repeatTo("x\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n", 24<<10), []int{1200, 100, 3}},
		{"digits", repeatTo("1234567890123456789\n 987654321\n", 24<<10), []int{1200, 100, 3}},
	}
	for _, tc := range cases {
		whole := tkm.Encode(tc.text, nil, nil)
		for _, budget := range tc.budgets {
			if budget > 0 && len(tc.text) <= budget*truncatePrefixBytesPerToken {
				t.Fatalf("%s: %d bytes do not reach the prefix cap at budget %d", tc.name, len(tc.text), budget)
			}
			got := TruncateToTokenLimit(tc.text, budget)
			want := refTruncateTokens(tkm, tc.text, whole, budget)
			if got != want {
				t.Errorf("%s / budget %d: prefix-path result differs from the whole-text encode (len %d vs %d)", tc.name, budget, len(got), len(want))
			}
		}
	}
}

// For any cut tokenStablePrefix picks, the prefix's tokens are exactly the
// leading tokens of the whole text — the property the exactness above
// rests on, checked on hand-picked boundary shapes and on random texts.
func TestTokenStablePrefix_TokensArePrefixOfWhole(t *testing.T) {
	tkm := requireTokenizer(t)
	check := func(name, text string, maxBytes int) (p int) {
		t.Helper()
		p = tokenStablePrefix(text, maxBytes)
		if p == 0 {
			return 0
		}
		if p > maxBytes || text[p-1] != '\n' {
			t.Fatalf("%s: cut %d (max %d) is not after a line break", name, p, maxBytes)
		}
		whole := tkm.Encode(text, nil, nil)
		head := tkm.Encode(text[:p], nil, nil)
		if len(head) > len(whole) {
			t.Fatalf("%s: prefix has more tokens (%d) than the whole (%d)", name, len(head), len(whole))
		}
		for i := range head {
			if head[i] != whole[i] {
				t.Fatalf("%s: token %d differs at cut %d: prefix %d, whole %d", name, i, p, head[i], whole[i])
			}
		}
		return p
	}

	shaped := map[string]string{
		"crlf":              "abc\r\n    def\r\nghi\r\n",
		"indent":            "a\n        b\n",
		"tab indent":        "a\n\t\tb\n",
		"space run":         "a   \n  b\n",
		"only spaces":       "a\n   \n  b\n",
		"unicode after":     "a\n b\nc\n",
		"digits":            "12345\n678\n",
		"contraction":       "it's\n 'tis\n",
		"punct newline":     "a!!!\n\n\n#b\n",
		"form feed":         "a\n\fb\nc\n",
		"multibyte":         "привет\n  мир\n",
		"nothing printable": "a\n\n\n",
	}
	for name, text := range shaped {
		for maxBytes := 1; maxBytes < len(text); maxBytes++ {
			check(name, text, maxBytes)
		}
	}

	alphabet := []string{"a", "b", "the", "SET", "%x%", "'s", "1", "23", "4567", " ", "  ", "\t", "\n", "\r\n", "\n\n", "!", "#", "...", "é", "日本", "👍", " ", "\f", "$'''", "=>"}
	rng := rand.New(rand.NewSource(4))
	found := 0
	for i := 0; i < 300; i++ {
		var b strings.Builder
		for b.Len() < 600 {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		text := b.String()
		if check("random", text, 1+rng.Intn(len(text)-1)) > 0 {
			found++
		}
	}
	if found < 100 {
		t.Fatalf("only %d of 300 random texts yielded a cut; the generator no longer exercises the boundary rule", found)
	}
}

// A 5 MB text at the default 4000-token budget tokenises its prefix only:
// the whole-text encode (~1.5 s, ~700 MB of garbage) never runs.
func TestTruncateToTokenLimit_EncodesOnlyThePrefix(t *testing.T) {
	requireTokenizer(t)
	text := padSource(5 << 20)
	seen := countEncodedBytes(t)
	out := TruncateToTokenLimit(text, 4000)
	if !strings.HasSuffix(out, "...") || len(out) > 64<<10 {
		t.Fatalf("truncation result looks wrong: %d bytes, suffix %q", len(out), out[max(0, len(out)-3):])
	}
	if *seen >= 128<<10 {
		t.Errorf("tokenised %d bytes of a %d-byte text, want under 128 KB (the budget × 16 prefix)", *seen, len(text))
	}
}

func BenchmarkTruncateToTokenLimit_5MB(b *testing.B) {
	text := padSource(5 << 20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := TruncateToTokenLimit(text, 4000); len(out) == 0 {
			b.Fatal("empty")
		}
	}
}
