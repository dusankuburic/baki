package database

import (
	"context"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// TestPurgeExpiredData_AbandonedKnowledgeUploads: the retention sweep deletes an unfinished knowledge upload older
// than KnowledgePendingTTL with its chunks (source text included), and leaves a recent unfinished upload and an old
// ready document alone.
func TestPurgeExpiredData_AbandonedKnowledgeUploads(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	org := leaseTestOrg(t, b, "purge")
	ctx := context.Background()
	save := func(id, state string, aged bool) {
		t.Helper()
		chunk := interfaces.KnowledgeChunk{ID: id + "-1", DocID: id, OrgID: org, Content: "text of " + id}
		if state == interfaces.KnowledgeDocumentPending {
			chunk.Pending = true
		} else {
			chunk.Embedding, chunk.EmbeddingIdentity = []float32{1, 0, 0}, "A"
		}
		doc := &interfaces.KnowledgeDocument{ID: id, OrgID: org, Filename: id + ".md", ChunkerVersion: 2, State: state, Content: "text of " + id}
		if err := b.SaveKnowledgeDocumentWithChunks(ctx, "", doc, []interfaces.KnowledgeChunk{chunk}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		if aged {
			if _, err := b.DB().ExecContext(ctx, `UPDATE knowledge_documents SET created_at = NOW() - make_interval(secs => $2) WHERE id = $1`,
				id, interfaces.KnowledgePendingTTL.Seconds()+3600); err != nil {
				t.Fatal(err)
			}
		}
	}
	oldPending, newPending, oldReady := org+"-old-pending", org+"-new-pending", org+"-old-ready"
	save(oldPending, interfaces.KnowledgeDocumentPending, true)
	save(newPending, interfaces.KnowledgeDocumentPending, false)
	save(oldReady, interfaces.KnowledgeDocumentReady, true)

	res, err := b.PurgeExpiredData(ctx, 0)
	if err != nil {
		t.Fatalf("PurgeExpiredData: %v", err)
	}
	if res.KnowledgeUploads < 1 {
		t.Errorf("KnowledgeUploads = %d; want the abandoned upload counted", res.KnowledgeUploads)
	}
	for id, want := range map[string]int{oldPending: 0, newPending: 1, oldReady: 1} {
		var docs, chunks int
		if err := b.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM knowledge_documents WHERE id = $1), (SELECT count(*) FROM knowledge_chunks WHERE doc_id = $1)`, id).Scan(&docs, &chunks); err != nil {
			t.Fatal(err)
		}
		if docs != want || chunks != want {
			t.Errorf("%s: %d documents, %d chunks after the sweep; want %d of each", id, docs, chunks, want)
		}
	}
}
