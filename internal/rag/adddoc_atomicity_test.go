package rag

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// failingChunkStore wraps a working backend and fails the chunk insert on
// demand — the shape of any transient DB error mid-upload.
type failingChunkStore struct {
	*testutil.FakeBackend
	failChunks error
}

func (f *failingChunkStore) SaveKnowledgeChunks(ctx context.Context, userID string, c []interfaces.KnowledgeChunk) error {
	if f.failChunks != nil {
		return f.failChunks
	}
	return f.FakeBackend.SaveKnowledgeChunks(ctx, userID, c)
}

func docCount(t *testing.T, store *testutil.FakeBackend, orgID string) int {
	t.Helper()
	docs, err := store.ListKnowledgeDocuments(context.Background(), orgID, 0)
	if err != nil {
		t.Fatalf("ListKnowledgeDocuments: %v", err)
	}
	return len(docs)
}

// TestAddDocument_FailedReuploadKeepsTheExistingVersion is the regression gate
// for a re-upload that can destroy both versions of a document.
//
// AddDocument deleted the existing document FIRST, then saved the new one, then
// saved its chunks. The comment there claimed the ordering was safe because the
// delete happens "AFTER embedding succeeded" — true for an EMBEDDING failure,
// but the delete still precedes both writes. A failure in SaveKnowledgeDocument
// or SaveKnowledgeChunks leaves the org with NEITHER version, and the rollback
// at the end deletes the NEW document when the old one is already gone.
//
// Reachable by any org admin on any transient DB error, on the ordinary
// "re-upload a corrected version" path — the moment a user is most likely to
// assume their previous copy is still safe.
func TestAddDocument_FailedReuploadKeepsTheExistingVersion(t *testing.T) {
	base := &testutil.FakeBackend{}
	store := &failingChunkStore{FakeBackend: base}
	svc := NewKnowledgeService(store, nil, nil)
	svc.embedOverride = &stubEmbedder{}

	const orgID, filename = "org-1", "handbook.md"
	ctx := context.Background()

	if err := svc.AddDocument(ctx, "scope", orgID, filename, "the original approved content"); err != nil {
		t.Fatalf("initial upload: %v", err)
	}
	if got := docCount(t, base, orgID); got != 1 {
		t.Fatalf("fixture rot: expected 1 document after upload, got %d", got)
	}

	// Re-upload a corrected version; the chunk insert fails partway.
	store.failChunks = errors.New("simulated: connection reset during chunk insert")
	if err := svc.AddDocument(ctx, "scope", orgID, filename, "a corrected version"); err == nil {
		t.Fatal("expected the re-upload to fail")
	}

	if got := docCount(t, base, orgID); got == 0 {
		t.Error("a FAILED re-upload destroyed the existing document — the org now has neither " +
			"the old version nor the new one")
	}
}
