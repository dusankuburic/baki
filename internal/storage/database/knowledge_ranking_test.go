package database

import (
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

func chunk(id string, emb ...float32) interfaces.KnowledgeChunk {
	return interfaces.KnowledgeChunk{ID: id, Content: id, Embedding: emb}
}

func TestRankKnowledgeChunks_OrdersByCosineSimilarity(t *testing.T) {
	chunks := []interfaces.KnowledgeChunk{
		chunk("far", 0, 1),     // orthogonal to query → sim 0 → filtered by threshold
		chunk("near", 1, 0.1),  // close to query direction → high sim
		chunk("mid", 0.7, 0.7), // 45° → mid sim
	}
	got, err := rankKnowledgeChunks("org", chunks, []float32{1, 0}, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "far" has cosine similarity 0 (orthogonal) and is filtered by the
	// 0.5 relevance threshold — only near and mid survive.
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2 (orthogonal chunk filtered by threshold)", len(got))
	}
	if got[0].ID != "near" || got[1].ID != "mid" {
		t.Errorf("ranking = [%s %s], want [near mid]", got[0].ID, got[1].ID)
	}
}

func TestRankKnowledgeChunks_RespectsLimit(t *testing.T) {
	chunks := []interfaces.KnowledgeChunk{
		chunk("a", 1, 0), chunk("b", 0.9, 0.1), chunk("c", 0.8, 0.2),
	}
	got, err := rankKnowledgeChunks("org", chunks, []float32{1, 0}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d chunks, want 2 (limit)", len(got))
	}
}

// The whole-corpus dimension mismatch (e.g. the embedding provider was changed
// after indexing) must fail loudly instead of returning silently-wrong results
// scored 0 by cosineSimilarity's length guard.
func TestRankKnowledgeChunks_AllMismatched_Errors(t *testing.T) {
	// Stored chunks are 3-dim; query is 2-dim.
	chunks := []interfaces.KnowledgeChunk{
		chunk("x", 1, 0, 0), chunk("y", 0, 1, 0),
	}
	got, err := rankKnowledgeChunks("org", chunks, []float32{1, 0}, 3)
	if err == nil {
		t.Fatalf("expected a dimension-mismatch error, got %d chunks and nil error", len(got))
	}
	if got != nil {
		t.Errorf("expected nil result on mismatch, got %v", got)
	}
}

// A partial mismatch skips the incomparable chunks but still returns the
// comparable ones (no error), so a mixed corpus mid-reindex degrades gracefully.
func TestRankKnowledgeChunks_PartialMismatch_SkipsIncomparable(t *testing.T) {
	chunks := []interfaces.KnowledgeChunk{
		chunk("old", 1, 0, 0), // 3-dim, incomparable → skipped
		chunk("new", 1, 0),    // 2-dim, comparable
	}
	got, err := rankKnowledgeChunks("org", chunks, []float32{1, 0}, 3)
	if err != nil {
		t.Fatalf("partial mismatch should not error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "new" {
		t.Errorf("got %+v, want only the comparable 'new' chunk", got)
	}
}

func TestRankKnowledgeChunks_EmptyCorpus(t *testing.T) {
	got, err := rankKnowledgeChunks("org", nil, []float32{1, 0}, 3)
	if err != nil {
		t.Fatalf("empty corpus should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d chunks, want 0", len(got))
	}
}
