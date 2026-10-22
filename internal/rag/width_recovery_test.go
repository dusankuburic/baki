package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
)

// seedIdentityDoc stores one document of org-1 (id docID, chunk ids docID-0, docID-1, …) whose chunks hold vec
// under identity.
func seedIdentityDoc(store *tu.FakeBackend, docID string, vec []float32, identity string, contents ...string) {
	ctx := context.Background()
	_ = store.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: docID, OrgID: "org-1", Filename: docID + ".md"})
	chunks := make([]interfaces.KnowledgeChunk, len(contents))
	for i, c := range contents {
		chunks[i] = interfaces.KnowledgeChunk{ID: fmt.Sprintf("%s-%d", docID, i), DocID: docID, Content: c, Embedding: append([]float32(nil), vec...), EmbeddingIdentity: identity}
	}
	_ = store.SaveKnowledgeChunks(ctx, "", chunks)
}

// TestCheckCorpusModel_NoVectorIndex_OwnCorpusAcceptedWithoutProof (R-config-8): on a store without pgvector
// every row is outside an index that does not exist, which says nothing about the model; a corpus whose every
// row carries the caller's identity is accepted by the census alone — the upload costs its own embedding and
// nothing more, and an unforced re-index converts nothing and buys nothing.
func TestCheckCorpusModel_NoVectorIndex_OwnCorpusAcceptedWithoutProof(t *testing.T) {
	ctx := context.Background()
	store := &tu.FakeBackend{EmbeddingDim: 2, NoPgvector: true}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1, 0}, "A", "A")
	emb := &namedEmbedder{identity: "A", vec: []float32{1, 0}}
	svc := guardService(store, emb)
	if err := svc.AddDocument(ctx, "u1", "org-1", "new.md", "a new guideline"); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if emb.calls != 1 {
		t.Errorf("embed calls = %d, want 1 (the document only: without a vector index the identity census is the whole proof)", emb.calls)
	}
	res, err := svc.ReindexOrg(ctx, "u1", "org-1", false, interfaces.KnowledgeChunkCursor{})
	if err != nil || res.Chunks != 0 {
		t.Errorf("unforced re-index of the caller's own corpus = %+v, %v; want nothing to do", res, err)
	}
	if emb.calls != 1 {
		t.Errorf("embed calls = %d after the unforced re-index, want still 1 (no proof, nothing re-bought)", emb.calls)
	}
}

// TestKnowledge_NoVectorIndex_ReportsNoWidth (R-config-8): without a vector index no width puts anything in it
// and the in-process search finds the chunks at the embedder's width, so an embedder off PAD_EMBEDDING_DIM is
// neither warned about nor reported to the listing, and the service says it has no index.
func TestKnowledge_NoVectorIndex_ReportsNoWidth(t *testing.T) {
	probe := installWidthLogProbe(t)
	ctx := context.Background()
	store := &tu.FakeBackend{EmbeddingDim: 2, NoPgvector: true}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	svc := widthService(store, 2, &namedEmbedder{identity: "wide", vec: []float32{1, 0, 0}})
	if svc.HasVectorIndex() {
		t.Error("HasVectorIndex = true on a store without pgvector")
	}
	if err := svc.AddDocument(ctx, "u1", "org-1", "a.md", "retry every call"); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if got, err := svc.Search(ctx, "u1", "org-1", "retries"); err != nil || got == "" {
		t.Fatalf("Search = %q, %v; want the chunk from the in-process search", got, err)
	}
	docs, err := svc.ListDocuments(ctx, "u1", "org-1", 0, 0)
	if err != nil || len(docs) != 1 || docs[0].VectorIndexed != 0 {
		t.Fatalf("ListDocuments = %+v, %v; want one document with nothing indexed", docs, err)
	}
	if m := svc.EmbeddingWidthMismatch(ctx, "org-1", docs); m != nil {
		t.Errorf("EmbeddingWidthMismatch without a vector index = %+v, want nil", m)
	}
	if warns := probe.warnings("PAD_EMBEDDING_DIM"); len(warns) != 0 {
		t.Errorf("got %d width warnings without a vector index, want none", len(warns))
	}
}

// TestEmbeddingWidthMismatch_ListingProbesTheRowsOutsideTheIndex (R-config-8): the listing measures the rows
// OUTSIDE the vector index, not the guard's sample, which prefers indexed documents — a corpus with three or more
// documents in the index and one stranded outside it still reports the stranded width.
func TestEmbeddingWidthMismatch_ListingProbesTheRowsOutsideTheIndex(t *testing.T) {
	ctx := context.Background()
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	for _, id := range []string{"doc-a", "doc-b", "doc-c"} {
		seedIdentityDoc(store, id, []float32{1, 0}, "A", "fits "+id)
	}
	seedIdentityDoc(store, "doc-wide", []float32{1, 0, 0}, "A", "too wide")
	svc := widthService(store, 2, &namedEmbedder{identity: "A", vec: []float32{1, 0}})
	docs, err := svc.ListDocuments(ctx, "u1", "org-1", 0, 0)
	if err != nil || len(docs) != 4 {
		t.Fatalf("ListDocuments = %d docs, %v; want 4", len(docs), err)
	}
	want := EmbeddingWidthMismatch{Configured: 2, Observed: 3, Source: WidthSourceStored}
	if m := svc.EmbeddingWidthMismatch(ctx, "org-1", docs); m == nil || *m != want {
		t.Errorf("EmbeddingWidthMismatch = %+v, want %+v from the document outside the index", m, want)
	}
}

// TestEmbeddingWidthMismatch_SaysWhichSideIsOff (R-config-8): the report names the side whose width differs, as the
// two need opposite remedies — an embedder off PAD_EMBEDDING_DIM (change the deployment's width and restart) and
// stored chunks off it (re-index alone).
func TestEmbeddingWidthMismatch_SaysWhichSideIsOff(t *testing.T) {
	ctx := context.Background()
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	svc := widthService(store, 2, &namedEmbedder{identity: "A", vec: []float32{1, 0, 0}})
	if err := svc.AddDocument(ctx, "u1", "org-1", "a.md", "retry every call"); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	docs, _ := svc.ListDocuments(ctx, "u1", "org-1", 0, 0)
	want := EmbeddingWidthMismatch{Configured: 2, Observed: 3, Source: WidthSourceEmbedder}
	if m := svc.EmbeddingWidthMismatch(ctx, "org-1", docs); m == nil || *m != want {
		t.Errorf("EmbeddingWidthMismatch after a wide upload = %+v, want %+v", m, want)
	}

	stranded := &tu.FakeBackend{EmbeddingDim: 2}
	stranded.SeedOrgs("org-1")
	seedIdentityChunks(stranded, []float32{1, 0, 0}, "A", "A")
	fits := widthService(stranded, 2, &namedEmbedder{identity: "A", vec: []float32{1, 0}})
	docs, _ = fits.ListDocuments(ctx, "u1", "org-1", 0, 0)
	want = EmbeddingWidthMismatch{Configured: 2, Observed: 3, Source: WidthSourceStored}
	if m := fits.EmbeddingWidthMismatch(ctx, "org-1", docs); m == nil || *m != want {
		t.Errorf("EmbeddingWidthMismatch of a stranded corpus = %+v, want %+v", m, want)
	}
}

// TestAddDocument_OwnRowsOutsideTheIndexAccepted_ReindexWalksOnlyThose (R-config-8): a corpus whose every row is
// the caller's own, some in the vector index and some outside it (a width recovery left half done), is the
// caller's model: an upload is accepted — no second model enters the corpus — and an unforced re-index converts
// the rows outside the index alone, never re-buying the ones in it; once every row is in it there is nothing
// left to do.
func TestAddDocument_OwnRowsOutsideTheIndexAccepted_ReindexWalksOnlyThose(t *testing.T) {
	ctx := context.Background()
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityDoc(store, "doc-wide", []float32{1, 0, 0}, "A", "wide one", "wide two")
	seedIdentityDoc(store, "doc-fits", []float32{1, 0}, "A", "fits")
	emb := &namedEmbedder{identity: "A", vec: []float32{1, 0}}
	svc := guardService(store, emb)
	if err := svc.AddDocument(ctx, "u1", "org-1", "new.md", "a new guideline"); err != nil {
		t.Fatalf("upload at the configured width into the caller's own mixed-width corpus: %v, want it accepted", err)
	}

	res, err := svc.ReindexOrg(ctx, "u1", "org-1", false, interfaces.KnowledgeChunkCursor{})
	if err != nil || res.Chunks != 2 || res.Docs != 1 {
		t.Fatalf("unforced re-index = %+v, %v; want the two chunks outside the index converted and nothing else", res, err)
	}
	for id, c := range store.KnowledgeChunks {
		if c.EmbeddingIdentity != "A" || len(c.Embedding) != 2 {
			t.Errorf("chunk %s = identity %q width %d, want A at width 2", id, c.EmbeddingIdentity, len(c.Embedding))
		}
	}
	calls := emb.calls
	if res, err := svc.ReindexOrg(ctx, "u1", "org-1", false, interfaces.KnowledgeChunkCursor{}); err != nil || res.Chunks != 0 {
		t.Errorf("unforced re-index of the converted corpus = %+v, %v; want nothing to do", res, err)
	}
	if emb.calls != calls {
		t.Errorf("an unforced re-index of the converted corpus embedded (%d calls)", emb.calls-calls)
	}
}

// TestReindexOrg_EmbedderOffTheConfiguredWidth_RefusedUnforced (R-config-8): an embedder whose vectors are not
// PAD_EMBEDDING_DIM wide cannot put one chunk into the vector index, so an unforced re-index with it would re-buy
// the same rows on every click and change nothing: once the proof has measured the embedder's width it is refused
// with ErrEmbeddingWidthOff, which names both widths and the remedy, and the corpus is left as it was. Force is
// the admin's word that the vectors must change anyway (the step before a restart at the embedder's width), and
// without a vector index there is nothing to be outside of, so the request runs.
func TestReindexOrg_EmbedderOffTheConfiguredWidth_RefusedUnforced(t *testing.T) {
	ctx := context.Background()
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityDoc(store, "doc-fits", []float32{1, 0}, "A", "fits")
	emb := &namedEmbedder{identity: "A", vec: []float32{1, 0, 0}}
	svc := widthService(store, 2, emb)
	if err := svc.AddDocument(ctx, "u1", "org-1", "wide.md", "a wide guideline"); err != nil {
		t.Fatalf("AddDocument at the embedder's width: %v", err)
	}

	calls := emb.calls
	res, err := svc.ReindexOrg(ctx, "u1", "org-1", false, interfaces.KnowledgeChunkCursor{})
	if !errors.Is(err, ErrEmbeddingWidthOff) {
		t.Fatalf("unforced re-index with the embedder at width 3 under PAD_EMBEDDING_DIM 2 = %+v, %v; want ErrEmbeddingWidthOff", res, err)
	}
	for _, want := range []string{"PAD_EMBEDDING_DIM is 2", "3 wide", "set PAD_EMBEDDING_DIM to 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	if emb.calls != calls+1 {
		t.Errorf("the refused re-index embedded %d times, want 1 (the proof that measured the width, no page)", emb.calls-calls)
	}
	if got := len(store.KnowledgeChunks["doc-fits-0"].Embedding); got != 2 {
		t.Errorf("the refused re-index rewrote the indexed chunk to width %d", got)
	}

	if res, err := svc.ReindexOrg(ctx, "u1", "org-1", true, interfaces.KnowledgeChunkCursor{}); err != nil || res.Chunks == 0 {
		t.Errorf("forced re-index = %+v, %v; want the admin's word to run it", res, err)
	}

	plain := &tu.FakeBackend{EmbeddingDim: 2, NoPgvector: true}
	plain.SeedOrgs("org-1")
	seedIdentityDoc(plain, "doc-fits", []float32{1, 0}, "A", "fits")
	noIndex := widthService(plain, 2, &namedEmbedder{identity: "A", vec: []float32{1, 0, 0}})
	if err := noIndex.AddDocument(ctx, "u1", "org-1", "wide.md", "a wide guideline"); err != nil {
		t.Fatalf("AddDocument without a vector index: %v", err)
	}
	if _, err := noIndex.ReindexOrg(ctx, "u1", "org-1", false, interfaces.KnowledgeChunkCursor{}); err != nil {
		t.Errorf("unforced re-index without a vector index: %v, want it to run", err)
	}
	// ...also where the proof does measure the embedder: another identity's rows are the stranded-corpus
	// recovery, which converts them to the caller's width and the in-process search then finds.
	other := &tu.FakeBackend{EmbeddingDim: 2, NoPgvector: true}
	other.SeedOrgs("org-1")
	seedIdentityChunks(other, []float32{1, 0}, "B", "B")
	recovery := widthService(other, 2, &namedEmbedder{identity: "A", vec: []float32{1, 0, 0}})
	if res, err := recovery.ReindexOrg(ctx, "u1", "org-1", false, interfaces.KnowledgeChunkCursor{}); err != nil || res.Chunks != 2 {
		t.Errorf("unforced re-index of another identity's rows without a vector index = %+v, %v; want both converted", res, err)
	}
}

// TestSearch_NoVectorIndex_EmptyResultIsNotBlamedOnTheWidth (R-config-8): without a vector index VectorIndexed is
// zero for every corpus, so an empty in-process search is a miss, not the width switch the ops hint names; with
// one, a corpus with nothing in it at the configured width still is.
func TestSearch_NoVectorIndex_EmptyResultIsNotBlamedOnTheWidth(t *testing.T) {
	probe := installWidthLogProbe(t)
	ctx := context.Background()
	store := &tu.FakeBackend{EmbeddingDim: 2, NoPgvector: true}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityDoc(store, "doc-1", []float32{1, 0}, "A", "retry every call")
	svc := widthService(store, 2, &namedEmbedder{identity: "A", vec: []float32{0, 1}})
	if out, err := svc.Search(ctx, "u1", "org-1", "unrelated"); err != nil || out != "" {
		t.Fatalf("Search = %q, %v; want nothing found", out, err)
	}
	if warns := probe.warnings("dimension mismatch"); len(warns) != 0 {
		t.Errorf("an empty search without a vector index logged the width hint %d times", len(warns))
	}

	indexed := &tu.FakeBackend{EmbeddingDim: 2}
	indexed.SeedOrgs("org-1")
	seedIdentityDoc(indexed, "doc-1", []float32{1, 0, 0}, "A", "retry every call")
	stranded := widthService(indexed, 2, &namedEmbedder{identity: "A", vec: []float32{0, 1}})
	if out, err := stranded.Search(ctx, "u1", "org-1", "unrelated"); err != nil || out != "" {
		t.Fatalf("Search of a stranded corpus = %q, %v; want nothing found", out, err)
	}
	if warns := probe.warnings("dimension mismatch"); len(warns) != 1 {
		t.Errorf("an empty search of a corpus with nothing in the vector index logged the width hint %d times, want once", len(warns))
	}
}
