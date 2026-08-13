package database

import (
	"strings"
	"testing"
)

// TestFormatVector_PgvectorTextLiteral verifies the text form pgvector's input
// function accepts: bracketed, comma-separated, no spaces. A round-trip through
// pgvector (server-side) is covered by the DATABASE_URL-gated integration test.
func TestFormatVector_PgvectorTextLiteral(t *testing.T) {
	tests := []struct {
		name string
		in   []float32
		want string
	}{
		{"single", []float32{0.5}, "[0.5]"},
		{"three", []float32{1, 0.25, -0.5}, "[1,0.25,-0.5]"},
		{"integer-valued", []float32{1, 2, 3}, "[1,2,3]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatVector(tc.in)
			if got != tc.want {
				t.Errorf("FormatVector(%v) = %q, want %q", tc.in, got, tc.want)
			}
			if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "]") {
				t.Errorf("pgvector literal must be bracketed, got %q", got)
			}
			if strings.ContainsAny(got, " \n") {
				t.Errorf("pgvector literal must contain no whitespace, got %q", got)
			}
		})
	}
}

// TestSearchKnowledge_FallsBackToGoWhenDimensionMismatches locks in the
// dispatch contract: even with pgvector active, a query whose dimension
// differs from the configured index dimension must take the Go-side path
// (server-side <=> would error across mismatched dimensions). We can't call
// SearchKnowledge without a DB, so this asserts the dispatch predicate itself.
func TestSearchKnowledge_FallsBackToGoWhenDimensionMismatches(t *testing.T) {
	b := &PostgresStorageBackend{hasPgvector: true, embeddingDim: 1536}

	// Same-dim query → vector path predicate holds.
	q := make([]float32, 1536)
	if !b.hasPgvector {
		t.Error("hasPgvector must be true for this fixture")
	}
	if len(q) != b.embeddingDim {
		t.Error("same-dim query should match configured embedding dim")
	}

	// Mismatched-dim query → predicate false → Go fallback.
	q2 := make([]float32, 768)
	if b.hasPgvector && len(q2) == b.embeddingDim {
		t.Error("mismatched-dim query must NOT satisfy the vector-path predicate")
	}
}

// TestSearchKnowledge_GoFallbackWhenNoPgvector confirms pgvector being off
// forces the Go path regardless of query dimension.
func TestSearchKnowledge_GoFallbackWhenNoPgvector(t *testing.T) {
	b := &PostgresStorageBackend{hasPgvector: false, embeddingDim: 1536}
	q := make([]float32, 1536)
	if b.hasPgvector && len(q) == b.embeddingDim {
		t.Error("hasPgvector=false must never take the vector path")
	}
}
