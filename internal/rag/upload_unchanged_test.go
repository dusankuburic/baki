package rag

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

const unchangedDoc = "# Naming\n\nVariables use camelCase. Subflows are named after the step they perform.\n\n# Errors\n\nEvery action that can fail sets an error handler."

// docsNamed lists org-1's documents of a filename.
func docsNamed(t *testing.T, store *testutil.FakeBackend, filename string) []*interfaces.KnowledgeDocument {
	t.Helper()
	docs, err := store.ListKnowledgeDocuments(context.Background(), "org-1", 0, 0)
	if err != nil {
		t.Fatalf("ListKnowledgeDocuments: %v", err)
	}
	var out []*interfaces.KnowledgeDocument
	for _, d := range docs {
		if d.Filename == filename {
			out = append(out, d)
		}
	}
	return out
}

// TestUploadDocument_IdenticalReuploadIsUnchanged (R-security-c22): re-uploading byte-identical content under the
// same filename, with the embedding identity the stored ready document's chunks carry, makes no embedding call and
// reports that document unchanged; other content, or another identity, is embedded as before.
func TestUploadDocument_IdenticalReuploadIsUnchanged(t *testing.T) {
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1")
	emb := &namedEmbedder{identity: "A", vec: []float32{1, 0}}
	svc := guardService(store, emb)
	ctx := context.Background()

	first, err := svc.UploadDocument(ctx, "scope", "org-1", "policy.md", unchangedDoc)
	if err != nil || first.State != interfaces.KnowledgeDocumentReady || first.Unchanged {
		t.Fatalf("first upload = %+v, %v; want a ready document", first, err)
	}
	calls := emb.calls

	again, err := svc.UploadDocument(ctx, "scope", "org-1", "policy.md", unchangedDoc)
	if err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if emb.calls != calls {
		t.Errorf("the identical re-upload made %d embedding calls, want none", emb.calls-calls)
	}
	if !again.Unchanged || again.ID != first.ID || again.State != interfaces.KnowledgeDocumentReady ||
		again.Embedded != 0 || again.PendingChunks != 0 || again.Chunks != first.Chunks || again.NextCursor != nil || again.Resumed {
		t.Errorf("re-upload = %+v; want the stored document %s reported unchanged", again, first.ID)
	}
	if docs := docsNamed(t, store, "policy.md"); len(docs) != 1 || docs[0].ID != first.ID {
		t.Errorf("documents of the name = %+v; want the one stored first", docs)
	}

	// Other content under the name is a new version, embedded and replacing the old one.
	edited, err := svc.UploadDocument(ctx, "scope", "org-1", "policy.md", unchangedDoc+"\n\nOne more rule.")
	if err != nil || edited.Unchanged || edited.ID == first.ID || edited.State != interfaces.KnowledgeDocumentReady {
		t.Fatalf("edited upload = %+v, %v; want a new ready version", edited, err)
	}
	if emb.calls == calls {
		t.Error("the edited upload was not embedded")
	}

	// The same content under another embedding identity is embedded again: the stored vectors are another model's.
	other := &namedEmbedder{identity: "B", vec: []float32{1, 0}}
	res, err := guardService(store, other).UploadDocument(ctx, "scope", "org-1", "policy.md", unchangedDoc+"\n\nOne more rule.")
	if err != nil || res.Unchanged || other.calls == 0 {
		t.Errorf("upload under another identity = %+v, %v (%d calls); want it embedded", res, err, other.calls)
	}
}

// An embedder that names no identity cannot be matched to the stored vectors: the re-upload is embedded.
func TestUploadDocument_ReuploadWithoutAnIdentityIsEmbedded(t *testing.T) {
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1")
	emb := &inputCountingEmbedder{}
	svc := guardService(store, emb)
	ctx := context.Background()
	if _, err := svc.UploadDocument(ctx, "scope", "org-1", "policy.md", unchangedDoc); err != nil {
		t.Fatal(err)
	}
	before := emb.billed()
	res, err := svc.UploadDocument(ctx, "scope", "org-1", "policy.md", unchangedDoc)
	if err != nil || res.Unchanged || emb.billed() == before {
		t.Errorf("re-upload = %+v, %v; want it embedded (no identity to match)", res, err)
	}
}

// Unchanged composes with the two-phase upload: an unfinished upload of the name (another text) is superseded by
// the identical re-upload, as by any re-upload, and the ready version stays; a continuation of it finds nothing.
func TestUploadDocument_UnchangedSupersedesAnUnfinishedUploadOfTheName(t *testing.T) {
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1")
	emb := &namedEmbedder{identity: "A", vec: []float32{1, 0}}
	svc := guardService(store, emb)
	ctx := context.Background()
	first, err := svc.UploadDocument(ctx, "scope", "org-1", "policy.md", unchangedDoc)
	if err != nil {
		t.Fatal(err)
	}

	// An edit too large for one request stays pending (the budget is spent after the first page).
	lowerReindexLimits(t, 100, 10000)
	lowerUploadBudget(t, 0)
	pending, err := svc.UploadDocument(ctx, "scope", "org-1", "policy.md", manyChunkDocument(250))
	if err != nil || pending.State != interfaces.KnowledgeDocumentPending {
		t.Fatalf("large upload = %+v, %v; want it pending", pending, err)
	}

	calls := emb.calls
	again, err := svc.UploadDocument(ctx, "scope", "org-1", "policy.md", unchangedDoc)
	if err != nil || !again.Unchanged || again.ID != first.ID {
		t.Fatalf("identical re-upload = %+v, %v; want %s unchanged", again, err, first.ID)
	}
	if emb.calls != calls {
		t.Errorf("the identical re-upload made %d embedding calls", emb.calls-calls)
	}
	if docs := docsNamed(t, store, "policy.md"); len(docs) != 1 || docs[0].ID != first.ID {
		t.Errorf("documents of the name = %+v; want only the ready version", docs)
	}
	if _, err := svc.ContinueDocument(ctx, "scope", "org-1", pending.ID, interfaces.KnowledgeChunkCursor{}); !errors.Is(err, ErrKnowledgeDocumentNotFound) {
		t.Errorf("continuing the superseded upload = %v, want ErrKnowledgeDocumentNotFound", err)
	}
}
