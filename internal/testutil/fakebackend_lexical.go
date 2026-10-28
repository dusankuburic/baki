package testutil

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"pad-analyzer/internal/storage/interfaces"
)

// knowledgeMinWordSimilarity is the Postgres lexical search's floor (pg_trgm word_similarity), and
// knowledgeLexicalPerPhrase its cap on one phrase's candidates.
const (
	knowledgeMinWordSimilarity = 0.8
	knowledgeLexicalPerPhrase  = 20
)

// SearchKnowledgeLexical scores the org's chunks against each phrase as the Postgres statement does (pg_trgm's
// word_similarity, wordSimilarity here; each phrase's 20 best visible matches at the floor), the sum of the phrases'
// scores first (then document and chunk id), and returns up to limit with their document's filename. identity
// keeps the chunks embedded under it and the legacy ones; "" keeps every chunk. A pending chunk (an upload not
// finished) is never a hit.
func (m *FakeBackend) SearchKnowledgeLexical(_ context.Context, orgID string, phrases []string, limit int, identity string) ([]interfaces.KnowledgeChunk, error) {
	if len(phrases) == 0 || limit <= 0 {
		return nil, nil
	}
	type scored struct {
		chunk interfaces.KnowledgeChunk
		score float64
	}
	m.mu.RLock()
	visible := map[string]scored{}
	for _, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID || c.Pending {
			continue
		}
		if identity != "" && c.EmbeddingIdentity != "" && c.EmbeddingIdentity != identity {
			continue
		}
		visible[c.ID] = scored{chunk: interfaces.KnowledgeChunk{ID: c.ID, DocID: c.DocID, Content: c.Content, Filename: d.Filename, ChunkerVersion: d.ChunkerVersion}}
	}
	m.mu.RUnlock()
	// Each phrase's candidates are its knowledgeLexicalPerPhrase best visible matches at the floor, by similarity
	// then chunk id, as the Postgres statement takes them (its window of 200 visible matches never binds here).
	type match struct {
		id string
		ws float64
	}
	for _, p := range phrases {
		var matches []match
		for id, v := range visible {
			// float32, as pg_trgm computes it: the floor compares the same value Postgres does.
			if ws := float64(float32(wordSimilarity(p, v.chunk.Content))); ws >= knowledgeMinWordSimilarity {
				matches = append(matches, match{id, ws})
			}
		}
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].ws != matches[j].ws {
				return matches[i].ws > matches[j].ws
			}
			return matches[i].id < matches[j].id
		})
		for _, mt := range matches[:min(knowledgeLexicalPerPhrase, len(matches))] {
			v := visible[mt.id]
			v.score += mt.ws
			visible[mt.id] = v
		}
	}
	var hits []scored
	for _, v := range visible {
		if v.score > 0 {
			hits = append(hits, v)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if hits[i].chunk.DocID != hits[j].chunk.DocID {
			return hits[i].chunk.DocID < hits[j].chunk.DocID
		}
		return hits[i].chunk.ID < hits[j].chunk.ID
	})
	out := make([]interfaces.KnowledgeChunk, 0, min(limit, len(hits)))
	for _, h := range hits[:min(limit, len(hits))] {
		out = append(out, h.chunk)
	}
	return out, nil
}

// trigrams are pg_trgm's: the text lowercased and cut into words (runs of letters and digits), each padded with two
// spaces in front and one behind, every three consecutive runes of a padded word a trigram, in text order.
func trigrams(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		padded := []rune("  " + w + " ")
		for i := 0; i+3 <= len(padded); i++ {
			out = append(out, string(padded[i:i+3]))
		}
	}
	return out
}

// wordSimilarity is pg_trgm's word_similarity(phrase, text): over the extents of text's trigram sequence, the best
// count / (len(phrase's distinct trigrams) + len(extent) - count), count being the phrase's distinct trigrams the
// extent holds. An extent worth considering starts and ends on a trigram of the phrase; one longer than eight times
// the phrase cannot score above 1/8, under any floor this is used with.
func wordSimilarity(phrase, text string) float64 {
	want := map[string]bool{}
	for _, t := range trigrams(phrase) {
		want[t] = true
	}
	if len(want) == 0 {
		return 0
	}
	seq := trigrams(text)
	n := float64(len(want))
	best := 0.0
	for i := range seq {
		if !want[seq[i]] {
			continue
		}
		seen := map[string]bool{}
		for j := i; j < len(seq) && j-i < 8*len(want); j++ {
			if !want[seq[j]] {
				continue
			}
			seen[seq[j]] = true
			count := float64(len(seen))
			if sim := count / (n + float64(j-i+1) - count); sim > best {
				best = sim
			}
		}
	}
	return best
}
