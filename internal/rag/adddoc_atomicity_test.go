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

// SaveKnowledgeDocumentWithChunks fails whole when the chunk insert does: one transaction.
func (f *failingChunkStore) SaveKnowledgeDocumentWithChunks(ctx context.Context, userID string, d *interfaces.KnowledgeDocument, c []interfaces.KnowledgeChunk) error {
	if f.failChunks != nil {
		return f.failChunks
	}
	return f.FakeBackend.SaveKnowledgeDocumentWithChunks(ctx, userID, d, c)
}

func docCount(t *testing.T, store *testutil.FakeBackend, orgID string) int {
	t.Helper()
	docs, err := store.ListKnowledgeDocuments(context.Background(), orgID, 0, 0)
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
	base.SeedOrgs("org-1") // the knowledge base needs its organisation
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

// crashingChunkStore loses the process at the chunk insert: that write and every later one (the compensating delete
// of the document among them) fail, as nothing after a crash runs.
type crashingChunkStore struct {
	*testutil.FakeBackend
	crashed bool
}

var errCrashed = errors.New("simulated: the process died before the chunks were written")

func (c *crashingChunkStore) SaveKnowledgeChunks(context.Context, string, []interfaces.KnowledgeChunk) error {
	c.crashed = true
	return errCrashed
}

// SaveKnowledgeDocumentWithChunks dies at its chunk insert: the transaction never commits, so nothing is written.
func (c *crashingChunkStore) SaveKnowledgeDocumentWithChunks(context.Context, string, *interfaces.KnowledgeDocument, []interfaces.KnowledgeChunk) error {
	c.crashed = true
	return errCrashed
}

func (c *crashingChunkStore) DeleteKnowledgeDocument(ctx context.Context, userID, orgID, id string) error {
	if c.crashed {
		return errCrashed
	}
	return c.FakeBackend.DeleteKnowledgeDocument(ctx, userID, orgID, id)
}

// TestAddDocument_CrashBeforeChunksLeavesNoDocument: an upload that dies between its document and its chunks leaves
// no document behind, and the retry stores the document with its chunks. The document row used to be committed on
// its own first, so a crash left it listed with no chunks (nothing for retrieval, and a re-upload of the same file
// then kept it as a version to supersede).
func TestAddDocument_CrashBeforeChunksLeavesNoDocument(t *testing.T) {
	base := &testutil.FakeBackend{}
	base.SeedOrgs("org-1") // the knowledge base needs its organisation
	crashing := NewKnowledgeService(&crashingChunkStore{FakeBackend: base}, nil, nil)
	crashing.embedOverride = &stubEmbedder{}
	const orgID, filename = "org-1", "handbook.md"
	ctx := context.Background()

	if err := crashing.AddDocument(ctx, "scope", orgID, filename, "the approved content"); err == nil {
		t.Fatal("premise: the upload that crashed succeeded")
	}
	docs, err := base.ListKnowledgeDocuments(ctx, orgID, 0, 0)
	if err != nil {
		t.Fatalf("ListKnowledgeDocuments: %v", err)
	}
	for _, d := range docs {
		t.Errorf("the crashed upload left document %s (%s) with %d chunks", d.ID, d.Filename, d.ChunkCount)
	}

	// The restarted process retries the upload.
	svc := NewKnowledgeService(base, nil, nil)
	svc.embedOverride = &stubEmbedder{}
	if err := svc.AddDocument(ctx, "scope", orgID, filename, "the approved content"); err != nil {
		t.Fatalf("retrying the upload: %v", err)
	}
	docs, err = base.ListKnowledgeDocuments(ctx, orgID, 0, 0)
	if err != nil {
		t.Fatalf("ListKnowledgeDocuments: %v", err)
	}
	if len(docs) != 1 || docs[0].ChunkCount == 0 {
		t.Errorf("after the retry: %d documents (%+v), want one with its chunks", len(docs), docs)
	}
}
