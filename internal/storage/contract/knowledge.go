package contract

import (
	"context"
	"sort"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// runKnowledgeScenarios covers the org knowledge base's embedding identity (ADR 0027): every chunk carries the
// identity of the embedder that produced its vector, "" (NULL) being a legacy row; a search names the caller's
// identity and gets the rows at it plus the legacy rows; the counts say what is current, stranded (another
// identity) and legacy; the stamp converts the legacy rows after a proof; a re-index page walks only the rows
// not yet at the identity and the update stamps what it converted.
func runKnowledgeScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	ids := func(chunks []interfaces.KnowledgeChunk) string {
		out := make([]string, len(chunks))
		for i, c := range chunks {
			out[i] = c.ID
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	run("Knowledge_chunks_carry_their_embedding_identity", func(t *testing.T) {
		admin := "contract-kb-admin-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: admin, Email: admin + "@example.com", Password: "x", Role: auth.RoleMember}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), admin) })
		orgID := "contract-kb-org-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "KB", OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), orgID) })
		docID := "contract-kb-doc-" + runID
		if err := b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "kb.md", ChunkerVersion: 1}); err != nil {
			t.Fatalf("SaveKnowledgeDocument: %v", err)
		}
		vec := []float32{1, 0, 0}
		a, bb, legacy := docID+"-a", docID+"-b", docID+"-legacy"
		if err := b.SaveKnowledgeChunks(ctx, admin, []interfaces.KnowledgeChunk{
			{ID: a, DocID: docID, OrgID: orgID, Content: "under A", Embedding: vec, EmbeddingIdentity: "A"},
			{ID: bb, DocID: docID, OrgID: orgID, Content: "under B", Embedding: vec, EmbeddingIdentity: "B"},
			{ID: legacy, DocID: docID, OrgID: orgID, Content: "legacy", Embedding: vec},
		}); err != nil {
			t.Fatalf("SaveKnowledgeChunks: %v", err)
		}

		hits, err := b.SearchKnowledge(ctx, orgID, vec, 10, "A")
		if err != nil || ids(hits) != a+","+legacy {
			t.Errorf("SearchKnowledge under A = %s, %v; want the A row and the legacy row", ids(hits), err)
		}
		hits, err = b.SearchKnowledge(ctx, orgID, vec, 10, "")
		if err != nil || ids(hits) != a+","+bb+","+legacy {
			t.Errorf("SearchKnowledge without an identity = %s, %v; want every row", ids(hits), err)
		}
		counts, err := b.CountKnowledgeChunksByIdentity(ctx, orgID, "A")
		if err != nil || counts.Total != 3 || counts.Current != 1 || counts.Stranded != 1 || counts.Legacy() != 1 {
			t.Errorf("CountKnowledgeChunksByIdentity(A) = %+v, %v; want total 3, current 1, stranded 1, legacy 1", counts, err)
		}
		byDoc, err := b.CountStrandedKnowledgeChunksByDocument(ctx, orgID, "A")
		if err != nil || len(byDoc) != 1 || byDoc[docID] != 1 {
			t.Errorf("CountStrandedKnowledgeChunksByDocument(A) = %v, %v; want {%s: 1}", byDoc, err, docID)
		}
		docs, err := b.ListKnowledgeDocuments(ctx, orgID, 0, 0)
		if err != nil || len(docs) != 1 || docs[0].ChunkerVersion != 1 || docs[0].ChunkCount != 3 {
			t.Errorf("ListKnowledgeDocuments = %+v, %v; want the document at chunker version 1 with 3 chunks", docs, err)
		}

		// A second organisation with a legacy row of its own: the stamp is per org and never crosses.
		otherOrg, otherDoc := orgID+"-other", docID+"-other"
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: otherOrg, Name: "KB2", OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}}); err != nil {
			t.Fatalf("SaveOrg(other): %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), otherOrg) })
		if err := b.SaveKnowledgeDocument(ctx, admin, &interfaces.KnowledgeDocument{ID: otherDoc, OrgID: otherOrg, Filename: "kb2.md", ChunkerVersion: 1}); err != nil {
			t.Fatalf("SaveKnowledgeDocument(other): %v", err)
		}
		if err := b.SaveKnowledgeChunks(ctx, admin, []interfaces.KnowledgeChunk{{ID: otherDoc + "-legacy", DocID: otherDoc, OrgID: otherOrg, Content: "other legacy", Embedding: vec}}); err != nil {
			t.Fatalf("SaveKnowledgeChunks(other): %v", err)
		}

		n, err := b.StampKnowledgeChunkIdentity(ctx, admin, orgID, "", "A")
		if err != nil || n != 1 {
			t.Fatalf("StampKnowledgeChunkIdentity(legacy → A) = %d, %v; want 1 row", n, err)
		}
		if counts, _ := b.CountKnowledgeChunksByIdentity(ctx, orgID, "A"); counts.Current != 2 || counts.Stranded != 1 || counts.Legacy() != 0 {
			t.Errorf("counts after the stamp = %+v, want current 2, stranded 1, legacy 0", counts)
		}
		if counts, _ := b.CountKnowledgeChunksByIdentity(ctx, otherOrg, "A"); counts.Total != 1 || counts.Legacy() != 1 {
			t.Errorf("the other org's counts after the stamp = %+v, want its one legacy row untouched", counts)
		}
		hits, _ = b.SearchKnowledge(ctx, orgID, vec, 10, "C")
		if ids(hits) != "" {
			t.Errorf("SearchKnowledge under C after the stamp = %s, want nothing (no legacy row left)", ids(hits))
		}

		page, err := b.ListKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0, "A")
		if err != nil || ids(page) != bb || page[0].Content != "under B" {
			t.Errorf("ListKnowledgeChunkContents not at A = %+v, %v; want the B row only", page, err)
		}
		page[0].Embedding, page[0].EmbeddingIdentity = vec, "A"
		if err := b.UpdateKnowledgeChunkEmbeddings(ctx, admin, page); err != nil {
			t.Fatalf("UpdateKnowledgeChunkEmbeddings: %v", err)
		}
		if page, err := b.ListKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0, "A"); err != nil || len(page) != 0 {
			t.Errorf("ListKnowledgeChunkContents not at A after the update = %+v, %v; want nothing", page, err)
		}
		if counts, _ := b.CountKnowledgeChunksByIdentity(ctx, orgID, "A"); counts.Current != 3 || counts.Stranded != 0 {
			t.Errorf("counts after the update = %+v, want every row current", counts)
		}
	})

	// An upload's document and chunks are one write: a refused chunk leaves no document, a refused document no chunk.
	run("Knowledge_document_and_chunks_written_together", func(t *testing.T) {
		admin := "contract-kbw-admin-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: admin, Email: admin + "@example.com", Password: "x", Role: auth.RoleMember}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), admin) })
		orgID := "contract-kbw-org-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "KBW", OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), orgID) })
		vec := []float32{1, 0, 0}
		chunk := func(id, docID string) interfaces.KnowledgeChunk {
			return interfaces.KnowledgeChunk{ID: id, DocID: docID, OrgID: orgID, Content: "text of " + id, Embedding: vec, EmbeddingIdentity: "A"}
		}
		listed := func() map[string]int {
			t.Helper()
			docs, err := b.ListKnowledgeDocuments(ctx, orgID, 0, 0)
			if err != nil {
				t.Fatalf("ListKnowledgeDocuments: %v", err)
			}
			out := map[string]int{}
			for _, d := range docs {
				out[d.ID] = d.ChunkCount
			}
			return out
		}

		doc := "contract-kbw-doc-" + runID
		if err := b.SaveKnowledgeDocumentWithChunks(ctx, admin, &interfaces.KnowledgeDocument{ID: doc, OrgID: orgID, Filename: "kbw.md", ChunkerVersion: 1},
			[]interfaces.KnowledgeChunk{chunk(doc+"-1", doc), chunk(doc+"-2", doc)}); err != nil {
			t.Fatalf("SaveKnowledgeDocumentWithChunks: %v", err)
		}
		if got := listed(); len(got) != 1 || got[doc] != 2 {
			t.Errorf("documents after the upload = %v, want %s with 2 chunks", got, doc)
		}

		// A chunk the store refuses (an id already stored): the new document is not written either.
		refused := doc + "-refused"
		if err := b.SaveKnowledgeDocumentWithChunks(ctx, admin, &interfaces.KnowledgeDocument{ID: refused, OrgID: orgID, Filename: "kbw2.md", ChunkerVersion: 1},
			[]interfaces.KnowledgeChunk{chunk(refused+"-1", refused), chunk(doc+"-1", refused)}); err == nil {
			t.Error("an upload with a chunk id already stored succeeded")
		}
		// A document the store refuses (an id already stored): its chunks are not written.
		if err := b.SaveKnowledgeDocumentWithChunks(ctx, admin, &interfaces.KnowledgeDocument{ID: doc, OrgID: orgID, Filename: "kbw.md", ChunkerVersion: 1},
			[]interfaces.KnowledgeChunk{chunk(doc+"-3", doc)}); err == nil {
			t.Error("an upload with a document id already stored succeeded")
		}
		if got := listed(); len(got) != 1 || got[doc] != 2 {
			t.Errorf("documents after the refused uploads = %v, want only %s with its 2 chunks", got, doc)
		}
		if counts, err := b.CountKnowledgeChunksByIdentity(ctx, orgID, "A"); err != nil || counts.Total != 2 {
			t.Errorf("chunks after the refused uploads = %+v, %v; want the first upload's 2", counts, err)
		}
	})
}
