package testutil

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestFakePurge_AbandonedKnowledgeUploads mirrors the Postgres sweep: an unfinished upload older than
// KnowledgePendingTTL goes with its chunks; a recent unfinished upload and an old ready document stay.
func TestFakePurge_AbandonedKnowledgeUploads(t *testing.T) {
	ctx := context.Background()
	b := &FakeBackend{}
	b.SeedOrgs("org-1")
	for _, d := range []struct {
		id, state string
		aged      bool
	}{{"old-pending", interfaces.KnowledgeDocumentPending, true}, {"new-pending", interfaces.KnowledgeDocumentPending, false}, {"old-ready", interfaces.KnowledgeDocumentReady, true}} {
		chunk := interfaces.KnowledgeChunk{ID: d.id + "-1", DocID: d.id, OrgID: "org-1", Content: "x", Pending: d.state == interfaces.KnowledgeDocumentPending}
		if !chunk.Pending {
			chunk.Embedding = []float32{1, 0}
		}
		if err := b.SaveKnowledgeDocumentWithChunks(ctx, "", &interfaces.KnowledgeDocument{ID: d.id, OrgID: "org-1", Filename: d.id, State: d.state, Content: "x"}, []interfaces.KnowledgeChunk{chunk}); err != nil {
			t.Fatal(err)
		}
		if d.aged {
			b.KnowledgeDocs[d.id].CreatedAt = time.Now().Add(-interfaces.KnowledgePendingTTL - time.Hour)
		}
	}
	res, err := b.PurgeExpiredData(ctx, 0)
	if err != nil || res.KnowledgeUploads != 1 {
		t.Fatalf("PurgeExpiredData = %+v, %v; want one abandoned upload", res, err)
	}
	for id, want := range map[string]bool{"old-pending": false, "new-pending": true, "old-ready": true} {
		_, doc := b.KnowledgeDocs[id]
		_, chunk := b.KnowledgeChunks[id+"-1"]
		if doc != want || chunk != want {
			t.Errorf("%s: document kept %v, chunk kept %v; want %v", id, doc, chunk, want)
		}
	}
}
