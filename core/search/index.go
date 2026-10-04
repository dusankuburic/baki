package search

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"pad-core/models"
)

// SearchIndex is the inverted token index of one flow document, immutable
// after NewSearchIndex, so any number of goroutines may Search it.
//
// It holds NO pointers into the document: FlowService caches indexes by flow
// id holding the document only through a weak pointer, and block pointers
// here would pin the whole tree the cache meant to let go. Blocks are
// resolved on the document at query time; the token map's block ids are the
// ids' own strings, not substrings of anything larger (see indexBuilder.add).
type SearchIndex struct {
	flowID string

	// tokens maps each lower-cased token to the ids of the blocks it occurs
	// in, in index order, each block listed once per token.
	tokens map[string][]string
	// blockCount is how many blocks were indexed — the index's size driver,
	// which byte-budgeted caches charge it by.
	blockCount int
}

var seenPool = sync.Pool{
	New: func() any {
		return make(map[string]bool, 64)
	},
}

// isWordRune reports whether r belongs to a word: a Unicode letter or digit.
// Everything else (punctuation, spaces, underscores, invalid UTF-8, which
// decodes to utf8.RuneError) separates words.
func isWordRune(r rune) bool {
	if r < utf8.RuneSelf {
		return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// eachPart calls emit with every token part of text, before lower-casing and
// de-duplication, in order. A word is a maximal run of letters/digits, split
// into camel-case parts when that yields at least two parts, emitted whole
// otherwise if longer than one byte. The parts are exactly the regexp
// pipeline's (pinned by TestTokenize_MatchesRegexpPipeline) and are
// substrings of text, so there is no scratch copy.
func eachPart(text string, emit func(part string)) {
	for i := 0; i < len(text); {
		r, w := rune(text[i]), 1
		if r >= utf8.RuneSelf {
			r, w = utf8.DecodeRuneInString(text[i:])
		}
		if !isWordRune(r) {
			i += w
			continue
		}
		start := i
		for i += w; i < len(text); i += w {
			r, w = rune(text[i]), 1
			if r >= utf8.RuneSelf {
				r, w = utf8.DecodeRuneInString(text[i:])
			}
			if !isWordRune(r) {
				break
			}
		}
		emitWordParts(text[start:i], emit)
	}
}

// emitWordParts is the camel-case split of one word (see eachPart). The
// first part is held back until a second one proves the word splits.
func emitWordParts(word string, emit func(part string)) {
	parts, firstStart, firstEnd := 0, 0, 0
	for j := 0; j < len(word); {
		c := word[j]
		start := j
		switch {
		case 'A' <= c && c <= 'Z':
			j++
			for j < len(word) && 'a' <= word[j] && word[j] <= 'z' {
				j++
			}
		case 'a' <= c && c <= 'z':
			for j < len(word) && 'a' <= word[j] && word[j] <= 'z' {
				j++
			}
		case '0' <= c && c <= '9':
			for j < len(word) && '0' <= word[j] && word[j] <= '9' {
				j++
			}
		default:
			// Not part of any camel-case part. Bytes of a multi-byte rune are
			// all >= 0x80, so skipping byte-wise skips the whole rune.
			j++
			continue
		}
		parts++
		switch parts {
		case 1:
			firstStart, firstEnd = start, j
			continue
		case 2:
			emit(word[firstStart:firstEnd])
		}
		emit(word[start:j])
	}
	if parts <= 1 && len(word) > 1 {
		emit(word)
	}
}

// appendLower appends the lower-cased part to buf. ASCII parts are lowered
// in place; anything else goes through strings.ToLower so the result is
// exactly what it returns.
func appendLower(buf []byte, part string) []byte {
	for i := 0; i < len(part); i++ {
		if part[i] >= utf8.RuneSelf {
			return append(buf, strings.ToLower(part)...)
		}
	}
	for i := 0; i < len(part); i++ {
		c := part[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf = append(buf, c)
	}
	return buf
}

// tokenize returns the distinct lower-cased tokens of text (parts longer
// than one byte once lowered), in first-occurrence order. A token that needed
// no lowering is a substring of text.
func tokenize(text string) []string {
	if text == "" {
		return nil
	}
	var tokens []string
	seen := seenPool.Get().(map[string]bool)
	defer func() {
		// Clear map for reuse
		for k := range seen {
			delete(seen, k)
		}
		seenPool.Put(seen)
	}()
	eachPart(text, func(part string) {
		lower := strings.ToLower(part)
		if len(lower) <= 1 {
			return
		}
		if !seen[lower] {
			tokens = append(tokens, lower)
			seen[lower] = true
		}
	})
	return tokens
}

func NewSearchIndex(flowID string, doc *models.FlowDocument) *SearchIndex {
	idx := &SearchIndex{
		flowID: flowID,
		tokens: make(map[string][]string),
	}

	b := indexBuilder{idx: idx}
	b.emit = b.add
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		for j := range sf.Blocks {
			b.indexBlock(&sf.Blocks[j])
		}
	}

	return idx
}

// indexBuilder streams each field's parts straight into the token map; the
// per-block "already listed" check in add de-duplicates on its own.
type indexBuilder struct {
	idx     *SearchIndex
	blockID string
	// buf holds the current part lower-cased; reused across parts so a token
	// already in the map costs no allocation (m[string(buf)] lookups don't
	// copy).
	buf  []byte
	emit func(part string)
}

func (b *indexBuilder) add(part string) {
	b.buf = appendLower(b.buf[:0], part)
	if len(b.buf) <= 1 {
		return
	}
	list := b.idx.tokens[string(b.buf)]
	if len(list) == 0 || list[len(list)-1] != b.blockID {
		// The key is a fresh copy (string(b.buf) in an assignment allocates),
		// never a substring of the block's text. Assigning to an existing
		// key also overwrites the stored key, so this holds on every store.
		b.idx.tokens[string(b.buf)] = append(list, b.blockID)
	}
}

func (b *indexBuilder) indexBlock(block *models.Block) {
	b.idx.blockCount++
	b.blockID = block.ID
	eachPart(block.Name, b.emit)
	eachPart(string(block.Type), b.emit)
	eachPart(block.RawType, b.emit)
	for _, v := range block.Variables {
		eachPart(v, b.emit)
	}
	for _, val := range block.Properties {
		eachPart(val, b.emit)
	}

	for i := range block.Children {
		b.indexBlock(&block.Children[i])
	}
}

func (idx *SearchIndex) FlowID() string {
	return idx.flowID
}

// BlockCount is the number of blocks the index was built over; the index's
// memory is roughly proportional to it.
func (idx *SearchIndex) BlockCount() int {
	return idx.blockCount
}
