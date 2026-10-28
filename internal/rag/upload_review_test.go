package rag

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// TestRechunkOrg_NeverSupersedesAPendingUpload: a rechunk of a ready v1 document whose name has an unfinished upload
// of other text (an admin's new version, part of it already bought) leaves that upload alone — it skips the document
// and reports it (SkippedPending) — instead of deleting the upload as a stale one and bringing the old text back as
// a new version. Continuing the admin's upload then replaces the v1 document as usual.
func TestRechunkOrg_NeverSupersedesAPendingUpload(t *testing.T) {
	lowerReindexLimits(t, 100, 10000)
	lowerUploadBudget(t, 0)
	store := &testutil.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1")
	old := rechunkSource(30)
	seedV1Document(t, store, "doc-v1", "sop.md", old, old, modelA)
	emb := &inputCountingEmbedder{vec: modelA}
	svc := guardService(store, emb)
	ctx := context.Background()

	upload, err := svc.UploadDocument(ctx, "scope", "org-1", "sop.md", manyChunkDocument(250))
	if err != nil || upload.State != interfaces.KnowledgeDocumentPending || upload.Embedded != 100 {
		t.Fatalf("the admin's upload = %+v, %v; want it paused with one page bought", upload, err)
	}
	billed := emb.billed()

	res, err := svc.RechunkOrg(ctx, "scope", "org-1")
	if err != nil {
		t.Fatalf("RechunkOrg: %v", err)
	}
	if res.SkippedPending != 1 || res.Rechunked != 0 || res.Chunks != 0 || res.NextCursor != nil {
		t.Errorf("rechunk = %+v; want the document skipped for its pending upload and nothing bought", res)
	}
	if got := emb.billed() - billed; got != 0 {
		t.Errorf("the rechunk bought %d inputs; want none", got)
	}
	d, err := store.GetKnowledgeDocument(ctx, "org-1", upload.ID)
	if err != nil || d.State != interfaces.KnowledgeDocumentPending || d.PendingChunks != 150 {
		t.Fatalf("the admin's upload after the rechunk = %+v, %v; want it untouched (150 still to embed)", d, err)
	}
	if v1, err := store.GetKnowledgeDocument(ctx, "org-1", "doc-v1"); err != nil || v1.State != interfaces.KnowledgeDocumentReady || v1.ChunkerVersion != 1 {
		t.Errorf("the v1 document = %+v, %v; want it serving as it was", v1, err)
	}

	lowerUploadBudget(t, 1<<40)
	done, err := svc.ContinueDocument(ctx, "scope", "org-1", upload.ID, interfaces.KnowledgeChunkCursor{})
	if err != nil || done.State != interfaces.KnowledgeDocumentReady {
		t.Fatalf("continuing the admin's upload = %+v, %v", done, err)
	}
	if _, err := store.GetKnowledgeDocument(ctx, "org-1", "doc-v1"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("the v1 document after the admin's upload finished: %v; want it replaced", err)
	}
}

// TestContinueDocument_StaleCursorRestartsTheWalk: a continuation whose cursor lies past every chunk still to embed
// (forged, or stale) embeds from the start instead of answering 200 embedded 0 with the same cursor forever; following
// the answers finishes the document.
func TestContinueDocument_StaleCursorRestartsTheWalk(t *testing.T) {
	lowerReindexLimits(t, 100, 10000)
	lowerUploadBudget(t, 0)
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1")
	svc := guardService(store, &inputCountingEmbedder{})
	ctx := context.Background()
	res, err := svc.UploadDocument(ctx, "scope", "org-1", "big.txt", manyChunkDocument(250))
	if err != nil || res.PendingChunks != 150 {
		t.Fatalf("UploadDocument = %+v, %v", res, err)
	}
	cursor := interfaces.KnowledgeChunkCursor{DocID: res.ID, ID: "ffffffff-ffff-ffff-ffff-ffffffffffff"}
	for requests := 1; ; requests++ {
		if requests > 5 {
			t.Fatalf("the continuation never finished; last answer %+v", res)
		}
		res, err = svc.ContinueDocument(ctx, "scope", "org-1", res.ID, cursor)
		if err != nil {
			t.Fatalf("ContinueDocument (request %d): %v", requests, err)
		}
		if res.PendingChunks > 0 && res.Embedded == 0 {
			t.Fatalf("request %d = %+v; a continuation with chunks left embedded nothing", requests, res)
		}
		if res.NextCursor == nil {
			break
		}
		cursor = *res.NextCursor
	}
	if res.State != interfaces.KnowledgeDocumentReady {
		t.Errorf("final = %+v; want ready", res)
	}
}

// deleteFailingStore fails every document delete.
type deleteFailingStore struct{ *testutil.FakeBackend }

var errDeleteFailed = errors.New("simulated: the delete failed")

func (deleteFailingStore) DeleteKnowledgeDocument(context.Context, string, string, string) error {
	return errDeleteFailed
}

// TestUploadDocument_SupersedeFailureFailsTheUpload: when the unfinished upload a re-upload supersedes cannot be
// removed, the re-upload fails and stores nothing — two pending versions of one name never coexist (continuing the
// older one later would retire the newer one once it is ready).
func TestUploadDocument_SupersedeFailureFailsTheUpload(t *testing.T) {
	lowerReindexLimits(t, 100, 10000)
	lowerUploadBudget(t, 0)
	base := &testutil.FakeBackend{}
	base.SeedOrgs("org-1")
	emb := &inputCountingEmbedder{}
	ctx := context.Background()
	first, err := guardService(base, emb).UploadDocument(ctx, "scope", "org-1", "sop.txt", manyChunkDocument(250))
	if err != nil || first.State != interfaces.KnowledgeDocumentPending {
		t.Fatalf("first = %+v, %v", first, err)
	}
	billed := emb.billed()

	failing := NewKnowledgeService(deleteFailingStore{base}, nil, nil, WithEmbedder(emb))
	second, err := failing.UploadDocument(ctx, "scope", "org-1", "sop.txt", manyChunkDocument(120)+"\n\nOther text.")
	if !errors.Is(err, errDeleteFailed) || second.ID != "" {
		t.Errorf("re-upload with the supersede failing = %+v, %v; want it refused, nothing stored", second, err)
	}
	if got := emb.billed() - billed; got != 0 {
		t.Errorf("the refused re-upload bought %d inputs", got)
	}
	docs, _ := base.ListKnowledgeDocuments(ctx, "org-1", 0, 0)
	if len(docs) != 1 || docs[0].ID != first.ID {
		t.Errorf("documents = %d; want only the first unfinished upload", len(docs))
	}
}
