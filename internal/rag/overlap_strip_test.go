package rag

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"pad-analyzer/internal/storage/interfaces"
)

// stripOverlapPrefixReference is the previous implementation, kept verbatim
// as the oracle: it materialised the rune slices and two fresh strings per
// candidate length. Output must match byte-for-byte for valid UTF-8 whenever
// the overlap is within maxOverlapProbeRunes.
func stripOverlapPrefixReference(content, docID string, all []interfaces.KnowledgeChunk) string {
	runes := []rune(content)
	if len(runes) <= minOverlapRunes {
		return content
	}
	for _, other := range all {
		if other.DocID != docID || other.Content == content {
			continue
		}
		otherRunes := []rune(other.Content)
		maxN := len(runes)
		if len(otherRunes) < maxN {
			maxN = len(otherRunes)
		}
		for n := maxN; n >= minOverlapRunes; n-- {
			if string(otherRunes[len(otherRunes)-n:]) == string(runes[:n]) {
				return string(runes[n:])
			}
		}
	}
	return content
}

// overlapAlphabet mixes 1-, 2-, 3- and 4-byte runes so byte offsets and rune
// offsets disagree throughout the generated text.
var overlapAlphabet = []rune("abcdefghij éü日本語😀🙂 ")

func randRunes(r *rand.Rand, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteRune(overlapAlphabet[r.Intn(len(overlapAlphabet))])
	}
	return sb.String()
}

func TestStripOverlapPrefix_MatchesReferenceImplementation(t *testing.T) {
	r := rand.New(rand.NewSource(20261001))
	overlaps := []int{0, 1, 30, 59, 60, 61, 100, 149, 150, 151, 200, 250, 299, 300}
	for i := 0; i < 300; i++ {
		overlaps = append(overlaps, r.Intn(maxOverlapProbeRunes+1))
	}
	stripped := 0
	for ci, ov := range overlaps {
		tail := randRunes(r, ov)
		// Sentinels on either side of the tail stop a coincidental rune match
		// from extending the overlap past the probe cap (which is the one
		// place the two implementations are allowed to differ).
		lead := randRunes(r, r.Intn(300)) + "«"
		fresh := ""
		if r.Intn(4) != 0 { // 1 in 4: content is exactly the tail (fully contained)
			fresh = "»" + randRunes(r, r.Intn(400))
		}
		content := tail + fresh
		all := []interfaces.KnowledgeChunk{
			{DocID: "other", Content: lead + tail},                // same bytes, other doc: never stripped
			{DocID: "doc", Content: randRunes(r, 61+r.Intn(200))}, // same doc, unrelated: probes all fail
			{DocID: "doc", Content: lead + tail},                  // the genuine neighbour
			{DocID: "doc", Content: content},                      // itself: skipped
		}
		want := stripOverlapPrefixReference(content, "doc", all)
		got := stripOverlapPrefix(content, "doc", all)
		if got != want {
			t.Fatalf("case %d (overlap %d runes): got %q want %q", ci, ov, got, want)
		}
		if got != content {
			stripped++
		}
		if !utf8.ValidString(got) {
			t.Fatalf("case %d: result is not valid UTF-8", ci)
		}
		// Cross-document lookup must leave the content alone in both.
		if g, w := stripOverlapPrefix(content, "nobody", all), stripOverlapPrefixReference(content, "nobody", all); g != w || g != content {
			t.Fatalf("case %d: cross-doc got %q want %q", ci, g, w)
		}
	}
	if stripped < 100 {
		t.Fatalf("fixture rot: only %d of %d cases actually stripped an overlap", stripped, len(overlaps))
	}
}

// A multibyte tail is cut at a rune boundary, never mid-rune.
func TestStripOverlapPrefix_MultibyteBoundary(t *testing.T) {
	tail := strings.Repeat("日😀", 40) // 80 runes, 280 bytes
	fresh := "後続のテキスト" + strings.Repeat("x", 10)
	all := []interfaces.KnowledgeChunk{
		{DocID: "d", Content: "前" + tail},
		{DocID: "d", Content: tail + fresh},
	}
	if got := stripOverlapPrefix(tail+fresh, "d", all); got != fresh {
		t.Fatalf("got %q want %q", got, fresh)
	}
}

// Pins the intentional probe cap: a repeated passage longer than
// maxOverlapProbeRunes strips only the capped length (chunkText never carries
// more than chunkOverlapRunes, so a longer match is repetition, not overlap).
func TestStripOverlapPrefix_ProbeCappedAtTwiceChunkOverlap(t *testing.T) {
	if maxOverlapProbeRunes != 300 || chunkOverlapRunes != 150 {
		t.Fatalf("geometry changed: probe %d overlap %d — revisit this pin", maxOverlapProbeRunes, chunkOverlapRunes)
	}
	dup := strings.Repeat("d", 400)
	all := []interfaces.KnowledgeChunk{
		{DocID: "d", Content: strings.Repeat("x", 100) + dup},
		{DocID: "d", Content: dup},
	}
	if got := stripOverlapPrefix(dup, "d", all); got != strings.Repeat("d", 100) {
		t.Fatalf("want 300-rune cap applied, got %d runes", utf8.RuneCountInString(got))
	}
}

// assembleGuidelines calls this per retrieved chunk per chat turn; a naive
// implementation builds two strings per candidate length (O(n²) garbage). A
// 1000-rune pair with a failing same-doc probe before the match must stay
// allocation-free.
func TestStripOverlapPrefix_NoPerIterationAllocations(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	tail := randRunes(r, chunkOverlapRunes)
	content := tail + "»" + randRunes(r, knowledgeChunkRunes-chunkOverlapRunes-1)
	all := []interfaces.KnowledgeChunk{
		{DocID: "d", Content: randRunes(r, knowledgeChunkRunes)}, // every probe fails
		{DocID: "d", Content: randRunes(r, knowledgeChunkRunes-chunkOverlapRunes-1) + "«" + tail},
	}
	want := stripOverlapPrefixReference(content, "d", all)
	if want == content {
		t.Fatal("fixture rot: pair does not overlap")
	}
	var got string
	allocs := testing.AllocsPerRun(100, func() {
		got = stripOverlapPrefix(content, "d", all)
	})
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if allocs >= 10 {
		t.Fatalf("stripOverlapPrefix allocates %.0f times per call on a 1000-rune pair; want < 10 (per-iteration string building back?)", allocs)
	}
}
