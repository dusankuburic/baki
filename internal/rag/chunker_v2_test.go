package rag

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"pad-analyzer/internal/storage/interfaces"
)

// chunkerV2Fixture is a Markdown SOP exercising every rule of chunker version 2: nested headings, a fenced code block
// whose '#' lines are not headings, decimals, file names, URLs, abbreviations and initials inside an over-long
// paragraph, a terminator-less run, multi-byte runes, and text before the first heading.
func chunkerV2Fixture() string {
	var sb strings.Builder
	sb.WriteString("Intro before any heading: this SOP covers retries.\r\n\r\n")
	sb.WriteString("# Retry policy\n\nEvery flow retries a failed web call 3 times.\n\n")
	sb.WriteString("## Backoff\n\n")
	for i := range 14 {
		fmt.Fprintf(&sb, "Step %d waits 3.5 s, e.g. after a timeout in flow.pad as J. Smith wrote at https://example.com/docs/v1.2/retry.html for approx. two minutes! ", i)
	}
	sb.WriteString("\n\n```robin\n# not a heading: a PAD comment\nSET Retries TO 3\n```\n\n")
	sb.WriteString("### Ünïcödé limits ###\n\n")
	sb.WriteString(strings.Repeat("x", 2300))
	sb.WriteString("\n\n# Logging\n\nLog the error code (no. 42) and the %FlowName% variable.\n")
	return sb.String()
}

// TestChunkTextV2_GoldenGeometry pins chunker version 2 as TestChunkText_GoldenGeometry pins version 1: the fixture
// splits into exactly the chunks in testdata/chunker_v2.golden. A change to the split is a new version.
func TestChunkTextV2_GoldenGeometry(t *testing.T) {
	if chunkerVersion != 2 {
		t.Fatalf("chunkerVersion = %d; this golden pins version 2 — add chunker_v%d.golden and keep this test for the stored documents", chunkerVersion, chunkerVersion)
	}
	chunks := chunkDocument(2, chunkerV2Fixture(), "sop.md")
	var got strings.Builder
	fmt.Fprintf(&got, "chunkerVersion %d\n", 2)
	for i, c := range chunks {
		fmt.Fprintf(&got, "===== chunk %d =====\n%s\n", i, c)
	}
	path := filepath.Join("testdata", "chunker_v2.golden")
	if *updateChunkerGolden {
		if err := os.WriteFile(path, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (create it with -update-chunker-golden)", err)
	}
	if want := strings.ReplaceAll(string(raw), "\r\n", "\n"); got.String() != want {
		t.Errorf("chunker version 2 changed its split: the stored version-2 documents keep theirs — revert it, and put a geometry change in a new version.\n--- golden\n%s--- now\n%s", want, got.String())
	}
}

// TestChunkTextV2_MarkdownChunksStartWithTheirHeadingPath: every chunk of a Markdown file under a heading starts with
// the path of headings above it; the fenced '#' line is text; a plain-text file has no paths at all.
func TestChunkTextV2_MarkdownChunksStartWithTheirHeadingPath(t *testing.T) {
	chunks := chunkDocument(2, chunkerV2Fixture(), "sop.md")
	if len(chunks) < 5 {
		t.Fatalf("got %d chunks, want the fixture's sections cut", len(chunks))
	}
	if !strings.HasPrefix(chunks[0], "Intro before any heading") {
		t.Errorf("chunk 0 = %.60q, want the text before the first heading, without a path", chunks[0])
	}
	var backoff, unicodePath, logging, fenced bool
	for _, c := range chunks[1:] {
		switch {
		case strings.HasPrefix(c, "Retry policy > Backoff\n\n"):
			backoff = true
			fenced = fenced || strings.Contains(c, "# not a heading: a PAD comment")
		case strings.HasPrefix(c, "Retry policy > Backoff > Ünïcödé limits\n\n"):
			unicodePath = true
		case strings.HasPrefix(c, "Logging\n\n"):
			logging = true
		case strings.HasPrefix(c, "Retry policy\n\n"):
		default:
			t.Errorf("chunk without its heading path: %.80q", c)
		}
	}
	if !backoff || !unicodePath || !logging || !fenced {
		t.Errorf("paths seen: backoff %v, nested %v, top-level after a deeper one %v, fenced comment kept as text %v", backoff, unicodePath, logging, fenced)
	}
	for _, c := range chunkDocument(2, chunkerV2Fixture(), "sop.txt") {
		if strings.HasPrefix(c, "Retry policy") {
			t.Errorf("a plain-text file got a heading path: %.60q", c)
		}
	}
}

// TestChunkTextV2_SentenceBoundariesNeverAlterTheText: v1 re-joined the pieces of an over-long paragraph with a space
// after every '.', so "3.5 mm" was stored as "3. 5 mm"; v2 cuts only where a sentence ends and keeps every chunk an
// exact stretch of the document.
func TestChunkTextV2_SentenceBoundariesNeverAlterTheText(t *testing.T) {
	var sb strings.Builder
	for i := range 30 {
		fmt.Fprintf(&sb, "Rule %d: keep a 3.5 mm margin in flow.pad, see https://example.com/a.b/c.html, i.e. Dr. Who's notes. ", i)
	}
	text := strings.TrimSpace(sb.String())
	v1 := strings.Join(chunkText(text, knowledgeChunkRunes), "\n")
	if !strings.Contains(v1, "3. 5 mm") || !strings.Contains(v1, "flow. pad") {
		t.Fatalf("fixture rot: version 1 no longer alters this text")
	}
	chunks := chunkTextV2(text, knowledgeChunkRunes, false)
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want the paragraph cut", len(chunks))
	}
	for i, c := range chunks {
		if !strings.Contains(text, c) {
			t.Errorf("chunk %d is not a stretch of the document: %.80q", i, c)
		}
		for _, bad := range []string{"3. 5", "flow. pad", "example. com", "i. e", "Dr.\n"} {
			if strings.Contains(c, bad) {
				t.Errorf("chunk %d altered the text (%q)", i, bad)
			}
		}
		if i > 0 && !strings.HasPrefix(c, "Rule ") && !strings.HasPrefix(c, tailRunes(chunks[i-1], chunkOverlapRunes)) {
			t.Errorf("chunk %d starts mid-sentence without the carried overlap: %.60q", i, c)
		}
		// Every cut is at a sentence end: the chunk's new text starts a rule (the carry aside).
		if i > 0 {
			rest := strings.TrimPrefix(c, tailRunes(chunks[i-1], chunkOverlapRunes))
			if !strings.HasPrefix(rest, "Rule ") {
				t.Errorf("chunk %d's new text starts mid-sentence: %.60q", i, rest)
			}
		}
	}
}

// tailRunes is s's last n runes.
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// TestChunkTextV2_SizeInvariantAndExactText: over random documents (headings, long sentences, runs without any
// whitespace, multi-byte text) every chunk is at most the size, heading path included, and its text after the path
// is an exact stretch of its section.
func TestChunkTextV2_SizeInvariantAndExactText(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	words := []string{"retry", "3.5", "flow.pad", "e.g.", "Dr.", "Ünïcödé", "日本語", "https://x.example/a.b", "end.", "Next!", "why?", "%Var%"}
	for doc := range 40 {
		var sb strings.Builder
		for p := range 3 + r.Intn(12) {
			if r.Intn(3) == 0 {
				fmt.Fprintf(&sb, "%s Heading %d-%d\n\n", strings.Repeat("#", 1+r.Intn(4)), doc, p)
			}
			n := 5 + r.Intn(400)
			for w := range n {
				if w > 0 {
					sb.WriteByte(' ')
				}
				if r.Intn(50) == 0 {
					sb.WriteString(strings.Repeat("z", r.Intn(1500)))
					continue
				}
				sb.WriteString(words[r.Intn(len(words))])
			}
			sb.WriteString("\n\n")
		}
		text := sb.String()
		size := []int{knowledgeChunkRunes, 300, 120}[doc%3]
		chunks := chunkTextV2(text, size, true)
		if len(chunks) == 0 {
			t.Fatalf("doc %d: no chunks", doc)
		}
		for i, c := range chunks {
			if n := utf8.RuneCountInString(c); n > size {
				t.Fatalf("doc %d chunk %d: %d runes > size %d", doc, i, n, size)
			}
			body := c
			if strings.HasPrefix(c, "Heading ") {
				if k := strings.Index(c, "\n\n"); k >= 0 {
					body = c[k+2:]
				}
			}
			if !strings.Contains(text, body) {
				t.Fatalf("doc %d chunk %d is not a stretch of the document: %.80q", doc, i, body)
			}
		}
	}
}

// TestStripOverlapPrefix_ReadsTheDocumentVersion: two neighbouring v2 chunks of one section are both retrieved — the
// second's carried overlap follows its heading path, so the v1 probe at the start cannot find it; read as v2 it is
// removed and the path is kept. The same pair labelled v1 is left as it is.
func TestStripOverlapPrefix_ReadsTheDocumentVersion(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("# Guide\n\n## Retries\n\n")
	for i := range 40 {
		fmt.Fprintf(&sb, "Sentence %d explains how the flow retries the call. ", i)
	}
	chunks := chunkDocument(2, sb.String(), "guide.md")
	if len(chunks) < 2 || !strings.HasPrefix(chunks[1], "Guide > Retries\n\n") {
		t.Fatalf("fixture rot: %d chunks, second %.40q", len(chunks), chunks[min(1, len(chunks)-1)])
	}
	hits := []interfaces.KnowledgeChunk{
		{DocID: "d", Content: chunks[0], ChunkerVersion: 2},
		{DocID: "d", Content: chunks[1], ChunkerVersion: 2},
	}
	got := stripOverlapPrefix(chunks[1], "d", hits)
	// The carry is chunk 0's tail up to the sentence chunk 0 ends with (18); the new text starts at sentence 19.
	body := strings.TrimPrefix(chunks[1], "Guide > Retries\n\n")
	want := "Guide > Retries\n\n" + body[strings.Index(body, "Sentence 19 "):]
	if got != want || got == chunks[1] {
		t.Errorf("v2 strip = %.120q\nwant %.120q", got, want)
	}
	if unchanged := stripOverlapPrefix(chunks[0], "d", hits); unchanged != chunks[0] {
		t.Errorf("the first chunk lost text: %.80q", unchanged)
	}
	v1 := []interfaces.KnowledgeChunk{{DocID: "d", Content: chunks[0], ChunkerVersion: 1}, {DocID: "d", Content: chunks[1], ChunkerVersion: 1}}
	if got := stripOverlapPrefix(chunks[1], "d", v1); got != chunks[1] {
		t.Errorf("read as v1 the pair was stripped: %.80q", got)
	}
}

// TestStripOverlapPrefix_V2NeverStripsFromTheMiddle: a v2 chunk without a heading path whose first paragraph is
// followed by boilerplate that another hit of the same document ends with keeps every rune — only a true carried
// prefix (at the start, or right after a heading path the other chunk shares) is removed.
func TestStripOverlapPrefix_V2NeverStripsFromTheMiddle(t *testing.T) {
	boilerplate := "Contact the automation team on the support channel before changing any production flow settings."
	chunk := "Short intro line.\n\n" + boilerplate + " Then restart the machine and rerun the flow."
	other := "Earlier section text that ends with the shared notice. " + boilerplate
	hits := []interfaces.KnowledgeChunk{
		{DocID: "d", Content: other, ChunkerVersion: 2},
		{DocID: "d", Content: chunk, ChunkerVersion: 2},
	}
	if got := stripOverlapPrefix(chunk, "d", hits); got != chunk {
		t.Errorf("text stripped from the middle of a chunk:\n got %q\nwant %q", got, chunk)
	}
	// The same overlap after a heading path the other chunk also starts with is a carried prefix and goes.
	withPath := "Guide > Retries\n\n" + boilerplate + " Then restart."
	prev := "Guide > Retries\n\nFirst part of the section. " + boilerplate
	hits = []interfaces.KnowledgeChunk{{DocID: "d", Content: prev, ChunkerVersion: 2}, {DocID: "d", Content: withPath, ChunkerVersion: 2}}
	if got, want := stripOverlapPrefix(withPath, "d", hits), "Guide > Retries\n\n Then restart."; got != want {
		t.Errorf("carried overlap after the path: got %q, want %q", got, want)
	}
}
