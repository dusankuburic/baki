package rag

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"pad-analyzer/internal/storage/interfaces"
)

// Chunker v2 (R-knowledge-9). Version 1 (chunkText) split paragraphs on "\n\n" and, inside an over-long paragraph,
// cut at every '.', '!' and '?' and re-joined the pieces with a space, which altered the stored text ("3.5 mm"
// became "3. 5 mm", "flow.pad" "flow. pad") and ignored the headings most SOPs are organised by. Version 2 keeps the
// geometry — knowledgeChunkRunes per chunk, chunkOverlapRunes carried from one chunk into the next — and changes
// three things:
//
//   - A Markdown document (by its file name) is cut into sections at its ATX headings (# … ######, outside fenced
//     code); a chunk never spans two sections, starts with its section's heading path ("Guide > Retries", the
//     headings above it, joined by " > ") and a blank line, and carries its overlap only within its section.
//   - An over-long paragraph is cut only where a sentence really ends: a terminator followed by whitespace, not after
//     a known abbreviation or an initial, and not when the next word starts lower-case; a decimal ("3.5"), a file
//     name ("flow.pad") or a URL has no whitespace after its dots and is never cut. A sentence longer than a chunk is
//     cut at its last whitespace that fits, or hard at the size when it has none.
//   - The pieces are exact stretches of the paragraph, whitespace included, so a chunk's text is the document's text:
//     consecutive pieces of a paragraph are concatenated as they stood, and paragraphs are joined by a blank line as
//     in version 1. Line endings are normalised to "\n" first.
//
// The size invariant is version 1's: every chunk, heading path included, is at most size runes.

// maxHeadingPathRunes caps the heading path prepended to a chunk, so a chunk keeps most of its room for text; a
// longer path keeps its innermost headings behind a leading "…".
const maxHeadingPathRunes = 200

// headingPathSeparator joins the headings of a path, outermost first.
const headingPathSeparator = " > "

// isMarkdownFilename reports whether a document is Markdown by its name: only then are '#' lines headings (a plain
// text file may hold PAD code, whose comments start with '#').
func isMarkdownFilename(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range []string{".md", ".markdown", ".mdx"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// chunkDocument cuts a document the way the given chunker version does; the version an upload stores
// (chunkerVersion) is the one it cut with.
func chunkDocument(version int, text, filename string) []string {
	if version <= 1 {
		return chunkText(text, knowledgeChunkRunes)
	}
	return chunkTextV2(text, knowledgeChunkRunes, isMarkdownFilename(filename))
}

// markdownSection is a stretch of a document under one heading path ("" before the first heading, or in a document
// that is not Markdown).
type markdownSection struct {
	path string
	body string
}

// chunkTextV2 is chunker version 2 (see above); markdown turns the heading sections on.
func chunkTextV2(text string, size int, markdown bool) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	sections := []markdownSection{{body: text}}
	if markdown {
		sections = markdownSections(text)
	}
	var chunks []string
	for _, sec := range sections {
		prefix := ""
		if sec.path != "" {
			prefix = capHeadingPath(sec.path) + "\n\n"
		}
		budget := size - utf8.RuneCountInString(prefix)
		for _, body := range chunkBodyV2(sec.body, budget, budget*chunkOverlapPercent/100) {
			chunks = append(chunks, prefix+body)
		}
	}
	if len(chunks) == 0 && markdown {
		return chunkTextV2(text, size, false) // headings and nothing under them: the text itself
	}
	return chunks
}

// capHeadingPath keeps a path within maxHeadingPathRunes, dropping its outermost runes behind "…".
func capHeadingPath(path string) string {
	runes := []rune(path)
	if len(runes) <= maxHeadingPathRunes {
		return path
	}
	return "…" + string(runes[len(runes)-maxHeadingPathRunes+1:])
}

// markdownSections cuts text at its ATX heading lines outside fenced code blocks. A heading line opens a section
// whose path is the headings above it by level, itself last; the heading line itself is not part of the body (the
// path carries it). Text before the first heading is a section without a path.
func markdownSections(text string) []markdownSection {
	type heading struct {
		level int
		text  string
	}
	var (
		sections []markdownSection
		stack    []heading
		body     strings.Builder
		fence    string
	)
	pathOf := func() string {
		parts := make([]string, len(stack))
		for i, h := range stack {
			parts[i] = h.text
		}
		return strings.Join(parts, headingPathSeparator)
	}
	path := ""
	emit := func() {
		if strings.TrimSpace(body.String()) != "" {
			sections = append(sections, markdownSection{path: path, body: body.String()})
		}
		body.Reset()
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent <= 3 {
			if marker := codeFence(trimmed); marker != "" {
				switch {
				case fence == "":
					fence = marker
				case strings.HasPrefix(marker, fence[:1]) && len(marker) >= len(fence) && strings.TrimSpace(strings.TrimLeft(trimmed, marker[:1])) == "":
					fence = ""
				}
			} else if fence == "" {
				if level, title, ok := atxHeading(trimmed); ok {
					emit()
					for len(stack) > 0 && stack[len(stack)-1].level >= level {
						stack = stack[:len(stack)-1]
					}
					if title != "" {
						stack = append(stack, heading{level, title})
					}
					path = pathOf()
					continue
				}
			}
		}
		body.WriteString(line)
	}
	emit()
	return sections
}

// codeFence is the fence marker a line opens or closes a fenced code block with (three or more backticks or tildes),
// or "".
func codeFence(line string) string {
	for _, ch := range []string{"`", "~"} {
		n := 0
		for n < len(line) && line[n] == ch[0] {
			n++
		}
		if n >= 3 {
			return line[:n]
		}
	}
	return ""
}

// atxHeading parses an ATX heading line ("## Title ##"): its level and its title, closing hashes and surrounding
// whitespace removed.
func atxHeading(line string) (level int, title string, ok bool) {
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := strings.TrimRight(line[level:], "\n")
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}
	rest = strings.TrimSpace(rest)
	if closing := strings.TrimRight(rest, "#"); closing != rest && (closing == "" || strings.HasSuffix(closing, " ") || strings.HasSuffix(closing, "\t")) {
		rest = strings.TrimSpace(closing)
	}
	return level, rest, true
}

// chunkBodyV2 packs one section's paragraphs into chunks of at most size runes, carrying the last overlap runes of a
// chunk into the next one; the text of every chunk is a stretch of the section's paragraphs as written, paragraphs
// joined by a blank line.
func chunkBodyV2(text string, size, overlap int) []string {
	var paragraphs []string
	for _, p := range strings.Split(text, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			paragraphs = append(paragraphs, p)
		}
	}
	if len(paragraphs) == 0 {
		return nil
	}
	var (
		chunks  []string
		current strings.Builder
		runes   int
		// fresh: something was written since the last flush (a chunk of only the carry would repeat its predecessor).
		fresh bool
		// open: the current chunk ends inside paragraph para, so its next piece continues it without a separator.
		open bool
	)
	flush := func() {
		if !fresh {
			current.Reset()
			runes, open = 0, false
			return
		}
		content := current.String()
		chunks = append(chunks, content)
		current.Reset()
		runes, fresh = 0, false
		if all := []rune(content); overlap > 0 && len(all) > overlap {
			current.WriteString(string(all[len(all)-overlap:]))
			runes = overlap
			return
		}
		open = false
	}
	// add appends a piece of a paragraph: joined to the chunk's text by sep unless it continues the paragraph the
	// chunk ends in.
	add := func(piece string, continues bool) {
		n := utf8.RuneCountInString(piece)
		sep := 0
		if runes > 0 && (!continues || !open) {
			sep = 2
		}
		for runes > 0 && runes+sep+n > size {
			flush()
			if runes == 0 {
				sep = 0
			}
		}
		if sep > 0 {
			current.WriteString("\n\n")
			runes += 2
		}
		current.WriteString(piece)
		runes += n
		fresh, open = true, true
	}
	for _, para := range paragraphs {
		if utf8.RuneCountInString(para) <= size {
			add(para, false)
			open = false
			continue
		}
		for i, piece := range sentencePieces(para, size) {
			add(piece, i > 0)
		}
		open = false
	}
	if fresh {
		chunks = append(chunks, current.String())
	}
	return chunks
}

// sentencePieces cuts a paragraph into exact consecutive stretches — their concatenation is the paragraph — one per
// sentence (sentenceEnds), each at most size runes: a longer sentence is cut at its last whitespace that fits, else
// hard at size.
func sentencePieces(para string, size int) []string {
	var pieces []string
	start := 0
	for _, end := range append(sentenceEnds(para), len(para)) {
		for start < end {
			cut := end
			if limit := start + runeOffset(para[start:end], size); limit < end {
				if cut = lastSpaceCut(para, start, limit); cut == 0 {
					cut = limit
				}
			}
			pieces = append(pieces, para[start:cut])
			start = cut
		}
	}
	return pieces
}

// runeOffset is the byte offset of s's n-th rune (len(s) when s is shorter).
func runeOffset(s string, n int) int {
	i := 0
	for k := 0; k < n && i < len(s); k++ {
		_, sz := utf8.DecodeRuneInString(s[i:])
		i += sz
	}
	return i
}

// lastSpaceCut is the byte offset just after the last whitespace run in para[start:limit] (not at start), 0 when none.
func lastSpaceCut(para string, start, limit int) int {
	for i := limit; i > start; {
		r, sz := utf8.DecodeLastRuneInString(para[:i])
		if unicode.IsSpace(r) {
			return i
		}
		i -= sz
	}
	return 0
}

// sentenceAbbreviations end with a '.' that does not end a sentence (compared lower-case, without the dot).
var sentenceAbbreviations = map[string]bool{
	"e.g": true, "i.e": true, "etc": true, "vs": true, "cf": true, "al": true, "approx": true, "ca": true,
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "sr": true, "jr": true, "st": true,
	"inc": true, "ltd": true, "co": true, "corp": true, "dept": true, "no": true, "nr": true, "fig": true,
	"p": true, "pp": true, "vol": true, "ch": true, "sec": true, "ref": true, "min": true, "max": true,
	"incl": true, "esp": true, "resp": true, "z.b": true, "bzw": true, "usw": true, "u.a": true, "d.h": true,
}

// sentenceEnds lists the byte offsets in para where a sentence ends: after a run of '.', '!' or '?' (and any closing
// quotes or brackets) followed by whitespace, the offset after that whitespace — unless the word before a '.' is a
// known abbreviation or a single letter (an initial), or the next word starts with a lower-case letter. A dot with
// no whitespace after it (a decimal, a file name, a URL) never ends one.
func sentenceEnds(para string) []int {
	var ends []int
	for i := 0; i < len(para); {
		r, sz := utf8.DecodeRuneInString(para[i:])
		if r != '.' && r != '!' && r != '?' {
			i += sz
			continue
		}
		start := i
		j := i
		for j < len(para) && strings.IndexByte(".!?", para[j]) >= 0 {
			j++
		}
		for j < len(para) && strings.IndexByte(`"')]`, para[j]) >= 0 {
			j++
		}
		for j < len(para) {
			r2, sz2 := utf8.DecodeRuneInString(para[j:])
			if r2 == '”' || r2 == '’' || r2 == '»' {
				j += sz2
				continue
			}
			break
		}
		k := j
		for k < len(para) {
			r2, sz2 := utf8.DecodeRuneInString(para[k:])
			if !unicode.IsSpace(r2) {
				break
			}
			k += sz2
		}
		i = j
		if k == j || k >= len(para) {
			continue // no whitespace after it, or the paragraph's end
		}
		if next, _ := utf8.DecodeRuneInString(para[k:]); unicode.IsLower(next) {
			continue
		}
		if para[start] == '.' && j-start == 1 && abbreviationBefore(para[:start]) {
			continue
		}
		ends = append(ends, k)
		i = k
	}
	return ends
}

// abbreviationBefore reports whether the word ending text (the text before a '.') is a known abbreviation or a single
// letter.
func abbreviationBefore(text string) bool {
	k := len(text)
	for k > 0 {
		r, sz := utf8.DecodeLastRuneInString(text[:k])
		if unicode.IsLetter(r) || r == '.' {
			k -= sz
			continue
		}
		break
	}
	word := strings.ToLower(text[k:])
	if word == "" {
		return false
	}
	if utf8.RuneCountInString(word) == 1 {
		return true
	}
	return sentenceAbbreviations[word]
}

// stripOverlapV2 is stripOverlapPrefix for a chunk cut by chunker v2: the overlap a chunk carries follows its heading
// path, so the carried prefix is looked for both at the chunk's start (a chunk without a path) and after the first
// blank line (the path's end). A stripped chunk keeps its path, so the passage still names its section.
//
// Only a true carried prefix is ever removed. v2 carries an overlap only within a section, and every chunk of a
// section starts with the same path, so the probe after the first line matches only another chunk that starts with
// that same line and blank line: a chunk without a path whose first paragraph is followed by text another chunk ends
// with (repeated boilerplate) is left whole, as version 1 would leave it.
func stripOverlapV2(content, docID string, all []interfaces.KnowledgeChunk) string {
	if got := stripOverlapAt(content, 0, docID, all); got != content {
		return got
	}
	sep := strings.Index(content, "\n\n")
	if sep <= 0 || strings.Contains(content[:sep], "\n") || utf8.RuneCountInString(content[:sep]) > maxHeadingPathRunes {
		return content
	}
	start := sep + 2
	path := content[:start]
	var section []interfaces.KnowledgeChunk
	for _, c := range all {
		if c.DocID == docID && strings.HasPrefix(c.Content, path) {
			section = append(section, c)
		}
	}
	got := stripOverlapAt(content, start, docID, section)
	if got == content {
		return content
	}
	if strings.TrimSpace(got[start:]) == "" {
		return ""
	}
	return got
}

// stripOverlapAt removes, from content[start:], the longest prefix (at least minOverlapRunes, at most
// maxOverlapProbeRunes runes) that another chunk of the same document ends with; content[:start] is kept. content
// when nothing matches.
func stripOverlapAt(content string, start int, docID string, all []interfaces.KnowledgeChunk) string {
	body := content[start:]
	var offs [maxOverlapProbeRunes + 1]int
	k, i := 0, 0
	for i < len(body) && k < maxOverlapProbeRunes {
		_, sz := utf8.DecodeRuneInString(body[i:])
		i += sz
		k++
		offs[k] = i
	}
	if k <= minOverlapRunes {
		return content
	}
	for _, other := range all {
		if other.DocID != docID || other.Content == content {
			continue
		}
		for n := k; n >= minOverlapRunes; n-- {
			if strings.HasSuffix(other.Content, body[:offs[n]]) {
				if start == 0 {
					return body[offs[n]:]
				}
				return content[:start] + body[offs[n]:]
			}
		}
	}
	return content
}

// documentChunkerVersion is the chunker version of docID's chunks among all (search hits carry their document's), 1
// when none says.
func documentChunkerVersion(docID string, all []interfaces.KnowledgeChunk) int {
	for _, c := range all {
		if c.DocID == docID && c.ChunkerVersion > 0 {
			return c.ChunkerVersion
		}
	}
	return 1
}
