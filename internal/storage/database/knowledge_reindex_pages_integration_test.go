package database

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage/interfaces"
)

// pageFailEmbedder embeds every input as vec, but fails any batch holding a
// chunk whose text marks it as at or beyond failFrom (the third re-index
// page of 500 when failFrom is 1000). failFrom < 0 never fails.
type pageFailEmbedder struct {
	ai.Provider
	vec      []float32
	failFrom int
}

func (p pageFailEmbedder) ID() string { return "page-fail-embed" }
func (p pageFailEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		var n int
		if _, err := fmt.Sscanf(text[strings.LastIndexByte(text, ' ')+1:], "%d", &n); err == nil && p.failFrom >= 0 && n >= p.failFrom {
			return nil, fmt.Errorf("embeddings: %w", ai.ErrProviderDown)
		}
		out[i] = append([]float32(nil), p.vec...)
	}
	return out, nil
}

// TestReindexOrg_PerPageCommit_Postgres pins R-7's per-page commit against a
// live database: with the re-index route outside the request transaction
// (no RLS tx on the context), a provider failure on page 3 keeps pages 1-2
// — 1 000 chunks — converted, and a resumed request from their cursor
// converts the rest. Inside a request-long transaction (the pre-R-7 shape,
// rolled back on the error response) the same failure keeps nothing.
// Gated on DATABASE_URL (the podman harness).
func TestReindexOrg_PerPageCommit_Postgres(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	org := fmt.Sprintf("reindex-pages-org-%d", stamp)
	doc := fmt.Sprintf("reindex-pages-doc-%d", stamp)
	seedOrgRow(t, b.DB(), org)
	t.Cleanup(func() {
		_, _ = b.DB().ExecContext(context.Background(), `DELETE FROM knowledge_chunks WHERE doc_id = $1`, doc)
		_, _ = b.DB().ExecContext(context.Background(), `DELETE FROM knowledge_documents WHERE id = $1`, doc)
	})
	if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: doc, OrgID: org, Filename: "big.md"}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	const n = 1200
	chunks := make([]interfaces.KnowledgeChunk, n)
	for i := range chunks {
		chunks[i] = interfaces.KnowledgeChunk{ID: fmt.Sprintf("%s-c%05d", doc, i), DocID: doc, Content: fmt.Sprintf("guideline %d", i), Embedding: []float32{1, 0, 0}}
	}
	if err := b.SaveKnowledgeChunks(ctx, "", chunks); err != nil {
		t.Fatalf("SaveKnowledgeChunks: %v", err)
	}
	converted := func() int {
		var c int
		if err := b.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_chunks WHERE doc_id = $1 AND embedding = '[0,1,0]'::jsonb`, doc).Scan(&c); err != nil {
			t.Fatalf("count converted: %v", err)
		}
		return c
	}
	modelB := []float32{0, 1, 0}
	zero := interfaces.KnowledgeChunkCursor{}

	// Pre-R-7 shape: one request-long RLS transaction, rolled back on error.
	tx, err := b.BeginRLS(ctx, "")
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	failing := rag.NewKnowledgeService(b, nil, nil, rag.WithEmbedder(pageFailEmbedder{vec: modelB, failFrom: 1000}))
	if _, err := failing.ReindexOrg(WithRLSTx(ctx, tx), "scope", org, true, zero); err == nil {
		_ = tx.Rollback()
		t.Fatal("re-index inside a request transaction succeeded, want the page-3 failure")
	}
	_ = tx.Rollback()
	if got := converted(); got != 0 {
		t.Fatalf("%d chunks converted after a rolled-back request transaction, want 0", got)
	}

	// R-7: no request transaction — every page commits on its own.
	if _, err := failing.ReindexOrg(ctx, "scope", org, true, zero); err == nil {
		t.Fatal("re-index succeeded, want the page-3 failure")
	}
	if got := converted(); got != 1000 {
		t.Fatalf("%d chunks converted after a page-3 failure, want pages 1-2 (1000) kept", got)
	}

	// Resume after the last committed chunk: the resumed guard sees the
	// converted pages on the caller's model and the walk finishes the rest.
	ok := rag.NewKnowledgeService(b, nil, nil, rag.WithEmbedder(pageFailEmbedder{vec: modelB, failFrom: -1}))
	res, err := ok.ReindexOrg(ctx, "scope", org, false, interfaces.KnowledgeChunkCursor{DocID: doc, ID: fmt.Sprintf("%s-c%05d", doc, 999)})
	if err != nil {
		t.Fatalf("resumed re-index: %v", err)
	}
	if res.Chunks != n-1000 || res.NextCursor != nil {
		t.Errorf("resumed result = %+v, want the remaining %d chunks and no cursor", res, n-1000)
	}
	if got := converted(); got != n {
		t.Errorf("%d of %d chunks converted after the resume", got, n)
	}

	// The resumed-guard sample reads backwards from the cursor.
	got, err := b.SampleKnowledgeChunkEmbeddingsUpTo(ctx, org, interfaces.KnowledgeChunkCursor{DocID: doc, ID: fmt.Sprintf("%s-c%05d", doc, 999)}, 3)
	if err != nil || len(got) != 3 || got[0].ID != fmt.Sprintf("%s-c%05d", doc, 999) || got[2].ID != fmt.Sprintf("%s-c%05d", doc, 997) || len(got[0].Embedding) != 3 {
		t.Errorf("SampleKnowledgeChunkEmbeddingsUpTo = %+v, %v; want c00999, c00998, c00997 with embeddings", got, err)
	}
	if other, err := b.SampleKnowledgeChunkEmbeddingsUpTo(ctx, "another-org", interfaces.KnowledgeChunkCursor{DocID: doc, ID: "~"}, 3); err != nil || len(other) != 0 {
		t.Errorf("another org's sample = %d chunks (%v), want none", len(other), err)
	}
}
