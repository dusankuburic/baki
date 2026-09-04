package scrubber

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestStreamScrubber_NeverEmitsPartialRune pins the UTF-8 boundary rule.
//
// Every released chunk is JSON-encoded into its own SSE event downstream, and
// encoding/json does not reject invalid UTF-8 — it silently substitutes U+FFFD.
// So a provider that splits a multi-byte character across two deltas (which
// they do, at arbitrary byte offsets) used to deliver "hello ��� world"
// to the client for "hello ✅ world". Any emoji, CJK or accented character in
// model output was affected.
//
// The test walks the split point across every byte offset and asserts two
// things per split: each individual chunk is valid UTF-8 on its own, and the
// text reassembled after a per-chunk JSON round-trip equals the input.
func TestStreamScrubber_NeverEmitsPartialRune(t *testing.T) {
	inputs := []struct{ name, text string }{
		{"emoji", "hello ✅ world"},
		{"cjk", "hello 世界 world"},
		{"accent", "héllo wörld"},
		{"4-byte emoji", "ok 🚀 go"},
		{"mixed", "café 世 ✅ 🚀 done"},
	}

	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			b := []byte(in.text)
			for cut := 1; cut < len(b); cut++ {
				ss := NewStreamScrubber()
				pieces := []string{
					ss.Write(string(b[:cut])),
					ss.Write(string(b[cut:])),
					ss.Flush(),
				}

				var rebuilt strings.Builder
				for _, p := range pieces {
					if p == "" {
						continue
					}
					if !utf8.ValidString(p) {
						t.Fatalf("cut=%d: emitted a chunk that is not valid UTF-8: %q", cut, p)
					}
					// Simulate the SSE hop: each chunk marshalled on its own.
					enc, err := json.Marshal(p)
					if err != nil {
						t.Fatalf("cut=%d: marshal: %v", cut, err)
					}
					var back string
					if err := json.Unmarshal(enc, &back); err != nil {
						t.Fatalf("cut=%d: unmarshal: %v", cut, err)
					}
					rebuilt.WriteString(back)
				}
				if got := rebuilt.String(); got != in.text {
					t.Fatalf("cut=%d: client would see %q, want %q", cut, got, in.text)
				}
			}
		})
	}
}

// TestStreamScrubber_ByteForByteDelivery drives one rune at a time, the worst
// case for a token-level provider: every Write carries a fragment of a
// multi-byte character.
func TestStreamScrubber_ByteForByteDelivery(t *testing.T) {
	const text = "héllo 世界 ✅ 🚀"
	ss := NewStreamScrubber()
	var out strings.Builder
	for _, c := range []byte(text) {
		piece := ss.Write(string([]byte{c}))
		if !utf8.ValidString(piece) {
			t.Fatalf("emitted invalid UTF-8 chunk: %q", piece)
		}
		out.WriteString(piece)
	}
	tail := ss.Flush()
	if !utf8.ValidString(tail) {
		t.Fatalf("flush emitted invalid UTF-8: %q", tail)
	}
	out.WriteString(tail)
	if got := out.String(); got != text {
		t.Errorf("byte-at-a-time delivery produced %q, want %q", got, text)
	}
}

// TestRuneSafeBoundary_MalformedInputIsNotHeld guards the escape hatch: bytes
// that are malformed rather than merely incomplete must be released, otherwise
// garbage from a provider would stall the stream permanently.
func TestRuneSafeBoundary_MalformedInputIsNotHeld(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		n    int
		want int
	}{
		// Complete runes: boundary untouched.
		{"ascii", []byte("abc"), 3, 3},
		{"complete 2-byte", []byte("é"), 2, 2},
		{"complete 3-byte", []byte("✅"), 3, 3},
		{"complete 4-byte", []byte("🚀"), 4, 4},
		// Incomplete trailing rune: held back.
		{"partial 2-byte", []byte("é")[:1], 1, 0},
		{"partial 3-byte head", []byte("a✅")[:2], 2, 1},
		{"partial 3-byte two", []byte("a✅")[:3], 3, 1},
		{"partial 4-byte", []byte("a🚀")[:3], 3, 1},
		// Lone continuation bytes with no lead in range: released, not held.
		{"orphan continuations", []byte{0x80, 0x80, 0x80, 0x80, 0x80}, 5, 5},
		// Invalid lead byte: treated as one byte, released.
		{"invalid lead", []byte{'a', 0xFF}, 2, 2},
		// Degenerate inputs.
		{"zero", []byte("abc"), 0, 0},
		{"negative", []byte("abc"), -1, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := runeSafeBoundary(c.b, c.n); got != c.want {
				t.Errorf("runeSafeBoundary(%q, %d) = %d, want %d", c.b, c.n, got, c.want)
			}
		})
	}
}
