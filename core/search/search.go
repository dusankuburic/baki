package search

import (
	"sort"
	"strings"
	"time"

	"pad-core/models"
)

// fuzzyMaxDistance is the maximum Levenshtein distance for a fuzzy token match.
const fuzzyMaxDistance = 2

func (idx *SearchIndex) Search(query models.SearchQuery) *models.SearchResults {
	start := time.Now()
	text := strings.ToLower(strings.TrimSpace(query.Text))
	if text == "" {
		return &models.SearchResults{Query: query}
	}

	queryTokens := tokenize(text)
	if len(queryTokens) == 0 {
		return &models.SearchResults{Query: query}
	}

	candidates := make(map[string]int)
	fuzzyHits := make(map[string]int)

	for _, qt := range queryTokens {
		if ids, ok := idx.tokens[qt]; ok {
			for _, blockID := range ids {
				candidates[blockID]++
			}
		} else if query.Fuzzy && len(qt) >= 4 {
			for token, blockIDs := range idx.tokens {
				// Levenshtein distance is at least the length difference, so a
				// token whose length differs from the query token by more than
				// the fuzzy threshold can never match — skip the O(len²) DP for
				// it. This is exact (same results), it just avoids wasted work
				// across the full token index on every fuzzy query.
				if d := len(qt) - len(token); d > fuzzyMaxDistance || d < -fuzzyMaxDistance {
					continue
				}
				if Levenshtein(qt, token) <= fuzzyMaxDistance {
					for _, id := range blockIDs {
						candidates[id]++
						fuzzyHits[id]++
					}
				}
			}
		}
	}

	typeFilter := make(map[models.BlockType]bool)
	for _, bt := range query.BlockTypes {
		typeFilter[bt] = true
	}

	var results []models.SearchResult
	for blockID, hits := range candidates {
		block := idx.blocks[blockID]
		if block == nil {
			continue
		}

		if len(typeFilter) > 0 && !typeFilter[block.Type] {
			continue
		}

		isFuzzy := fuzzyHits[blockID] > 0

		score := computeScore(block, text, hits, len(queryTokens), isFuzzy)
		if score <= 0 {
			continue
		}

		matchedField := detectMatchedField(block, text, queryTokens)
		matchedText := block.Name
		highlights := computeHighlights(block.Name, queryTokens)

		results = append(results, models.SearchResult{
			BlockID:      blockID,
			SubflowID:    idx.blockSubflow[blockID],
			MatchedField: matchedField,
			MatchedText:  matchedText,
			Score:        score,
			Highlights:   highlights,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if query.MaxResults > 0 && len(results) > query.MaxResults {
		results = results[:query.MaxResults]
	}

	return &models.SearchResults{
		Query:      query,
		Results:    results,
		TotalCount: len(results),
		DurationMs: int(time.Since(start).Milliseconds()),
	}
}

func computeScore(block *models.Block, query string, hits int, totalQueryTokens int, isFuzzyFallback bool) int {
	nameLower := strings.ToLower(block.Name)
	score := 0

	if nameLower == query {
		score = 100
	} else if strings.HasPrefix(nameLower, query) {
		score = 80
	} else if strings.Contains(nameLower, query) {
		score = 60
	}

	if hits == totalQueryTokens && score > 0 {
		score += 20
	}

	if score == 0 {
		for _, val := range block.Properties {
			if strings.Contains(strings.ToLower(val), query) {
				score = 40
				break
			}
		}
	}

	if score == 0 {
		for _, v := range block.Variables {
			if strings.Contains(strings.ToLower(v), query) {
				score = 50
				break
			}
		}
	}

	if score == 0 && hits > 0 {
		score = 30 + (hits * 5)
	}

	if isFuzzyFallback && score > 0 {
		score = int(float64(score) * 0.7)
	}

	return score
}

func detectMatchedField(block *models.Block, query string, queryTokens []string) string {
	nameLower := strings.ToLower(block.Name)
	if strings.Contains(nameLower, query) {
		return "name"
	}
	for _, qt := range queryTokens {
		if strings.Contains(nameLower, qt) {
			return "name"
		}
	}
	for k, v := range block.Properties {
		vLower := strings.ToLower(v)
		if strings.Contains(vLower, query) {
			return "property:" + k
		}
		for _, qt := range queryTokens {
			if strings.Contains(vLower, qt) {
				return "property:" + k
			}
		}
	}
	for _, v := range block.Variables {
		vLower := strings.ToLower(v)
		if strings.Contains(vLower, query) {
			return "variable"
		}
		for _, qt := range queryTokens {
			if strings.Contains(vLower, qt) {
				return "variable"
			}
		}
	}
	return "name"
}

func computeHighlights(text string, queryTokens []string) []models.Highlight {
	type span struct{ start, end int }
	lower := strings.ToLower(text)
	var spans []span

	for _, qt := range queryTokens {
		start := 0
		for {
			idx := strings.Index(lower[start:], qt)
			if idx == -1 {
				break
			}
			absIdx := start + idx
			end := absIdx + len(qt)
			spans = append(spans, span{absIdx, end})
			start = end
		}
	}

	if len(spans) == 0 {
		return nil
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	merged := make([]span, 0, len(spans))
	cur := spans[0]
	for _, s := range spans[1:] {
		if s.start <= cur.end {
			if s.end > cur.end {
				cur.end = s.end
			}
		} else {
			merged = append(merged, cur)
			cur = s
		}
	}
	merged = append(merged, cur)

	highlights := make([]models.Highlight, len(merged))
	for i, s := range merged {
		highlights[i] = models.Highlight{Start: s.start, End: s.end}
	}
	return highlights
}
