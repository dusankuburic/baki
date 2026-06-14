package search

import (
	"regexp"
	"strings"
	"sync"
	"unicode"

	"pad-core/models"
)

type SearchIndex struct {
	flowID string

	tokens       map[string][]string
	blocks       map[string]*models.Block
	blockSubflow map[string]string
}

var camelCaseRe = regexp.MustCompile(`[A-Z][a-z]*|[a-z]+|[0-9]+`)

var (
	seenPool = sync.Pool{
		New: func() any {
			return make(map[string]bool, 64)
		},
	}
	builderPool = sync.Pool{
		New: func() any {
			return &strings.Builder{}
		},
	}
)

func tokenize(text string) []string {
	if text == "" {
		return nil
	}

	sb := builderPool.Get().(*strings.Builder)
	sb.Reset()
	defer builderPool.Put(sb)

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
		} else {
			sb.WriteRune(' ')
		}
	}

	words := strings.Fields(sb.String())
	var tokens []string
	seen := seenPool.Get().(map[string]bool)
	defer func() {
		// Clear map for reuse
		for k := range seen {
			delete(seen, k)
		}
		seenPool.Put(seen)
	}()

	for _, word := range words {
		camelParts := camelCaseRe.FindAllString(word, -1)
		var parts []string
		if len(camelParts) > 1 {
			parts = camelParts
		} else if len(word) > 1 {
			parts = []string{word}
		}

		for _, part := range parts {
			lower := strings.ToLower(part)
			if len(lower) <= 1 {
				continue
			}
			if !seen[lower] {
				tokens = append(tokens, lower)
				seen[lower] = true
			}
		}
	}

	return tokens
}

func NewSearchIndex(flowID string, doc *models.FlowDocument) *SearchIndex {
	idx := &SearchIndex{
		flowID:       flowID,
		tokens:       make(map[string][]string),
		blocks:       make(map[string]*models.Block),
		blockSubflow: make(map[string]string),
	}

	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		for j := range sf.Blocks {
			idx.indexBlock(&sf.Blocks[j], sf.ID)
		}
	}

	return idx
}

func (idx *SearchIndex) indexBlock(block *models.Block, subflowID string) {
	idx.blocks[block.ID] = block
	idx.blockSubflow[block.ID] = subflowID

	tokens := tokenize(block.Name)
	tokens = append(tokens, tokenize(string(block.Type))...)
	tokens = append(tokens, tokenize(block.RawType)...)

	for _, v := range block.Variables {
		tokens = append(tokens, tokenize(v)...)
	}

	for _, val := range block.Properties {
		tokens = append(tokens, tokenize(val)...)
	}

	for _, tok := range tokens {
		list := idx.tokens[tok]
		if len(list) == 0 || list[len(list)-1] != block.ID {
			idx.tokens[tok] = append(list, block.ID)
		}
	}

	for i := range block.Children {
		idx.indexBlock(&block.Children[i], subflowID)
	}
}

func (idx *SearchIndex) FlowID() string {
	return idx.flowID
}
