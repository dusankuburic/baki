package testutil

import (
	"math"
	"testing"
)

// The fake's lexical search scores a phrase against a chunk as pg_trgm's word_similarity does, so the contract's
// lexical scenario holds on both backends: the values below are what Postgres 16 answers for each pair.
func TestWordSimilarity_MatchesPgTrgm(t *testing.T) {
	for _, tc := range []struct {
		phrase, text string
		want         float64
	}{
		{"word", "two words", 0.8},
		{"run dos", "x run y dos z", 0.8},
		{"dos command", "Run DOS commands", 0.9166667},
		{"run dos command", "Run the command in DOS", 0.6956522},
		{"run dos command", "Never use the Run DOS command action with administrator rights.", 1},
		{"admin rights", "Legacy note: Run DOS command is fine in test flows.", 0.07692308},
		{"flow.pad", "open flow.pad first", 1},
		{"%invoice_total%", "Set %Invoice_Total% to 0", 1},
		{"über straße", "die Über Straße hier", 1},
		{"ab", "xab", 0.33333334},
		{"", "anything", 0},
	} {
		if got := wordSimilarity(tc.phrase, tc.text); math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("wordSimilarity(%q, %q) = %v, want %v (pg_trgm)", tc.phrase, tc.text, got, tc.want)
		}
	}
}
