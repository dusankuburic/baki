package scrubber

import (
	"strings"
	"testing"
)

// benchChunks slices text into fixed-size chunks, mimicking provider deltas.
func benchChunks(text string, size int) []string {
	var chunks []string
	for i := 0; i < len(text); i += size {
		end := i + size
		if end > len(text) {
			end = len(text)
		}
		chunks = append(chunks, text[i:end])
	}
	return chunks
}

func runStream(b *testing.B, chunks []string) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := NewStreamScrubber()
		var sink int
		for _, c := range chunks {
			sink += len(s.Write(c))
		}
		sink += len(s.Flush())
		if sink == 0 {
			b.Fatal("no output")
		}
	}
}

// BenchmarkStreamScrubber_Prose: plain assistant prose, no secrets — the
// common case. Chunk size ~16 bytes mimics token-ish deltas.
func BenchmarkStreamScrubber_Prose(b *testing.B) {
	text := strings.Repeat("The flow reads a spreadsheet, then loops over rows and updates a form. ", 60) // ~4.3KB
	runStream(b, benchChunks(text, 16))
}

// BenchmarkStreamScrubber_AnchorDense: prose that mentions secret keywords
// constantly, so anchors appear in nearly every chunk.
func BenchmarkStreamScrubber_AnchorDense(b *testing.B) {
	text := strings.Repeat("check the password field, the token count, an api key note, secret handling. ", 55) // ~4.3KB
	runStream(b, benchChunks(text, 16))
}

// BenchmarkStreamScrubber_GreedyHold: worst case — a PWD= value with no
// terminator keeps the whole tail held, growing toward the cap while every
// chunk arrives.
func BenchmarkStreamScrubber_GreedyHold(b *testing.B) {
	text := "connection uses PWD = " + strings.Repeat("abcdefghijklmno ", 250) // ~4KB held
	runStream(b, benchChunks(text, 16))
}
