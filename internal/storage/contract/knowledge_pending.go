package contract

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// runKnowledgePendingScenarios covers the two-phase upload (R-knowledge-2), the kept source text and the rechunk walk
// (R-knowledge-9) and the per-document identity stamp (R-knowledge-3), all v51: a pending chunk is stored without an
// embedding and is out of the corpus — never a search hit, embedded or not, out of the census, the samples and the
// re-index pages — until its document is finished, which makes the document ready, puts its chunks into the corpus
// and retires the older ready versions of its filename at once; a legacy sample covers both ends of each document;
// the stamp names its documents.
func runKnowledgePendingScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	ids := func(chunks []interfaces.KnowledgeChunk) string {
		out := make([]string, len(chunks))
		for i, c := range chunks {
			out[i] = c.ID
		}
		return strings.Join(out, ",")
	}
	// seed creates an admin and an org of the admin for one scenario.
	seed := func(t *testing.T, prefix string) (admin, orgID string) {
		t.Helper()
		admin = prefix + "-admin-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: admin, Email: admin + "@example.com", Password: "x", Role: auth.RoleMember}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), admin) })
		orgID = prefix + "-org-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: prefix, OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: auth.RoleAdmin}}}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), orgID) })
		return admin, orgID
	}
	vec := []float32{1, 0, 0}

	run("Knowledge_pending_chunks_are_never_searched_and_finish_into_ready", func(t *testing.T) {
		admin, orgID := seed(t, "contract-kbp")
		old := "contract-kbp-old-" + runID
		if err := b.SaveKnowledgeDocumentWithChunks(ctx, admin, &interfaces.KnowledgeDocument{ID: old, OrgID: orgID, Filename: "sop.md", ChunkerVersion: 1},
			[]interfaces.KnowledgeChunk{{ID: old + "-1", DocID: old, OrgID: orgID, Content: "old version", Embedding: vec, EmbeddingIdentity: "A"}}); err != nil {
			t.Fatalf("save the ready version: %v", err)
		}
		other := "contract-kbp-other-" + runID
		if err := b.SaveKnowledgeDocumentWithChunks(ctx, admin, &interfaces.KnowledgeDocument{ID: other, OrgID: orgID, Filename: "other.md", ChunkerVersion: 1},
			[]interfaces.KnowledgeChunk{{ID: other + "-1", DocID: other, OrgID: orgID, Content: "other document", Embedding: vec, EmbeddingIdentity: "A"}}); err != nil {
			t.Fatalf("save another document: %v", err)
		}
		doc := "contract-kbp-new-" + runID
		pending := []interfaces.KnowledgeChunk{
			{ID: doc + "-1", DocID: doc, OrgID: orgID, Content: "new one", Pending: true},
			{ID: doc + "-2", DocID: doc, OrgID: orgID, Content: "new two", Pending: true},
			{ID: doc + "-3", DocID: doc, OrgID: orgID, Content: "new three", Pending: true},
		}
		if err := b.SaveKnowledgeDocumentWithChunks(ctx, admin, &interfaces.KnowledgeDocument{ID: doc, OrgID: orgID, Filename: "sop.md", ChunkerVersion: 2,
			State: interfaces.KnowledgeDocumentPending, Content: "new one new two new three"}, pending); err != nil {
			t.Fatalf("save the pending version: %v", err)
		}

		for _, identity := range []string{"A", ""} {
			hits, err := b.SearchKnowledge(ctx, orgID, vec, 10, identity)
			if err != nil || strings.Contains(ids(hits), doc) || len(hits) != 2 {
				t.Errorf("SearchKnowledge(%q) = %s, %v; want the two ready chunks, never a pending one", identity, ids(hits), err)
			}
		}
		if total, _, err := b.CountKnowledgeChunks(ctx, orgID); err != nil || total != 2 {
			t.Errorf("CountKnowledgeChunks = %d, %v; want 2 (pending chunks are not in the corpus yet)", total, err)
		}
		if counts, err := b.CountKnowledgeChunksByIdentity(ctx, orgID, "A"); err != nil || counts.Total != 2 || counts.Legacy() != 0 {
			t.Errorf("CountKnowledgeChunksByIdentity(A) = %+v, %v; want 2 current chunks and no legacy (a pending chunk is not legacy)", counts, err)
		}
		if page, err := b.ListKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0, ""); err != nil || strings.Contains(ids(page), doc) || len(page) != 2 {
			t.Errorf("ListKnowledgeChunkContents = %s, %v; want the ready chunks only (a re-index never embeds a pending chunk)", ids(page), err)
		}
		if page, err := b.ListUnindexedKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0); err != nil || strings.Contains(ids(page), doc) {
			t.Errorf("ListUnindexedKnowledgeChunkContents = %s, %v; want no pending chunk", ids(page), err)
		}
		if sample, err := b.SampleKnowledgeChunkEmbeddings(ctx, orgID, 10); err != nil || strings.Contains(ids(sample), doc) {
			t.Errorf("SampleKnowledgeChunkEmbeddings = %s, %v; want no pending chunk", ids(sample), err)
		}

		docs, err := b.ListKnowledgeDocuments(ctx, orgID, 0, 0)
		if err != nil {
			t.Fatalf("ListKnowledgeDocuments: %v", err)
		}
		byID := map[string]*interfaces.KnowledgeDocument{}
		for _, d := range docs {
			byID[d.ID] = d
		}
		if d := byID[doc]; d == nil || d.State != interfaces.KnowledgeDocumentPending || d.PendingChunks != 3 || d.ChunkCount != 3 || !d.HasSource || d.ChunkerVersion != 2 || d.Content != "" {
			t.Errorf("listed pending document = %+v; want pending, 3 of 3 chunks pending, source kept (not listed), version 2", d)
		}
		if d := byID[old]; d == nil || d.State != interfaces.KnowledgeDocumentReady || d.PendingChunks != 0 || d.HasSource {
			t.Errorf("listed ready document = %+v; want ready, nothing pending, no source", d)
		}

		got, err := b.GetKnowledgeDocument(ctx, orgID, doc)
		if err != nil || got.Content != "new one new two new three" || !got.HasSource || got.State != interfaces.KnowledgeDocumentPending || got.PendingChunks != 3 || got.ChunkCount != 3 || got.Filename != "sop.md" {
			t.Errorf("GetKnowledgeDocument = %+v, %v; want the pending document with its source text and counts", got, err)
		}
		if _, err := b.GetKnowledgeDocument(ctx, orgID+"-nope", doc); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("GetKnowledgeDocument from another org: %v, want ErrNotFound", err)
		}
		if _, err := b.GetKnowledgeDocument(ctx, orgID, doc+"-missing"); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("GetKnowledgeDocument of a missing id: %v, want ErrNotFound", err)
		}

		page, err := b.ListPendingKnowledgeChunks(ctx, orgID, doc, "", 2)
		if err != nil || ids(page) != doc+"-1,"+doc+"-2" || page[0].Content != "new one" {
			t.Fatalf("ListPendingKnowledgeChunks first page = %+v, %v; want chunks 1 and 2", page, err)
		}
		if rest, err := b.ListPendingKnowledgeChunks(ctx, orgID, doc, page[1].ID, 2); err != nil || ids(rest) != doc+"-3" {
			t.Errorf("ListPendingKnowledgeChunks after chunk 2 = %s, %v; want chunk 3", ids(rest), err)
		}
		if n, err := b.FinishKnowledgeDocument(ctx, admin, orgID, doc); err != nil || n != 3 {
			t.Errorf("FinishKnowledgeDocument with nothing embedded = %d, %v; want 3 pending", n, err)
		}

		for i := range page {
			page[i].Embedding, page[i].EmbeddingIdentity = vec, "A"
		}
		if err := b.UpdateKnowledgeChunkEmbeddings(ctx, admin, page); err != nil {
			t.Fatalf("UpdateKnowledgeChunkEmbeddings: %v", err)
		}
		if rest, err := b.ListPendingKnowledgeChunks(ctx, orgID, doc, "", 0); err != nil || ids(rest) != doc+"-3" {
			t.Errorf("pending after two embedded = %s, %v; want chunk 3 alone", ids(rest), err)
		}
		// Embedded, but the upload is not finished: still out of the corpus, the old version answering alone.
		if hits, err := b.SearchKnowledge(ctx, orgID, vec, 10, "A"); err != nil || strings.Contains(ids(hits), doc) || !strings.Contains(ids(hits), old) {
			t.Errorf("SearchKnowledge with the upload half embedded = %s, %v; want the old version, none of the new one", ids(hits), err)
		}
		if counts, err := b.CountKnowledgeChunksByIdentity(ctx, orgID, "A"); err != nil || counts.Total != 2 {
			t.Errorf("census with the upload half embedded = %+v, %v; want the 2 live chunks", counts, err)
		}
		if page, err := b.ListUnindexedKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0); err != nil || strings.Contains(ids(page), doc) {
			t.Errorf("ListUnindexedKnowledgeChunkContents with the upload half embedded = %s, %v; want none of it", ids(page), err)
		}
		if sample, err := b.SampleKnowledgeChunkEmbeddings(ctx, orgID, 10); err != nil || strings.Contains(ids(sample), doc) {
			t.Errorf("SampleKnowledgeChunkEmbeddings with the upload half embedded = %s, %v; want none of it", ids(sample), err)
		}
		if sample, err := b.SampleKnowledgeChunkEmbeddingsUpTo(ctx, orgID, interfaces.KnowledgeChunkCursor{DocID: doc + "~", ID: "~"}, 10); err != nil || strings.Contains(ids(sample), doc) {
			t.Errorf("SampleKnowledgeChunkEmbeddingsUpTo with the upload half embedded = %s, %v; want none of it", ids(sample), err)
		}
		if docs, _ := b.ListKnowledgeDocuments(ctx, orgID, 0, 0); len(docs) != 3 {
			t.Errorf("listed %d documents, want 3", len(docs))
		} else {
			for _, d := range docs {
				if d.ID == doc && (d.PendingChunks != 1 || d.ChunkCount != 3) {
					t.Errorf("listed upload = %+v; want 1 of 3 chunks still to embed", d)
				}
			}
		}
		if n, err := b.FinishKnowledgeDocument(ctx, admin, orgID, doc); err != nil || n != 1 {
			t.Errorf("FinishKnowledgeDocument with one left = %d, %v; want 1 pending", n, err)
		}
		if got, _ := b.GetKnowledgeDocument(ctx, orgID, doc); got == nil || got.State != interfaces.KnowledgeDocumentPending || got.PendingChunks != 1 {
			t.Errorf("document with one chunk left = %+v; want still pending, 1 chunk", got)
		}
		if _, err := b.GetKnowledgeDocument(ctx, orgID, old); err != nil {
			t.Errorf("the ready version is gone before the new one is ready: %v", err)
		}

		last := []interfaces.KnowledgeChunk{{ID: doc + "-3", DocID: doc, OrgID: orgID, Embedding: vec, EmbeddingIdentity: "A"}}
		if err := b.UpdateKnowledgeChunkEmbeddings(ctx, admin, last); err != nil {
			t.Fatalf("UpdateKnowledgeChunkEmbeddings(last): %v", err)
		}
		if n, err := b.FinishKnowledgeDocument(ctx, admin, orgID, doc); err != nil || n != 0 {
			t.Fatalf("FinishKnowledgeDocument when all are embedded = %d, %v; want 0", n, err)
		}
		if got, _ := b.GetKnowledgeDocument(ctx, orgID, doc); got == nil || got.State != interfaces.KnowledgeDocumentReady || got.PendingChunks != 0 {
			t.Errorf("finished document = %+v; want ready", got)
		}
		if _, err := b.GetKnowledgeDocument(ctx, orgID, old); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the superseded ready version after the finish: %v, want ErrNotFound (retired)", err)
		}
		if _, err := b.GetKnowledgeDocument(ctx, orgID, other); err != nil {
			t.Errorf("a document of another filename after the finish: %v, want kept", err)
		}
		hits, err := b.SearchKnowledge(ctx, orgID, vec, 10, "A")
		if err != nil || len(hits) != 4 {
			t.Errorf("SearchKnowledge after the finish = %s, %v; want the new document's 3 chunks and the other document's", ids(hits), err)
		}
		for _, h := range hits {
			if h.DocID == doc && h.ChunkerVersion != 2 {
				t.Errorf("hit %s carries chunker version %d, want its document's 2", h.ID, h.ChunkerVersion)
			}
		}
		if n, err := b.FinishKnowledgeDocument(ctx, admin, orgID, doc+"-missing"); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("FinishKnowledgeDocument of a missing id = %d, %v; want ErrNotFound", n, err)
		}
	})

	run("Knowledge_documents_below_a_chunker_version", func(t *testing.T) {
		admin, orgID := seed(t, "contract-kbv")
		save := func(id string, version int, state, content string) {
			t.Helper()
			chunk := interfaces.KnowledgeChunk{ID: id + "-1", DocID: id, OrgID: orgID, Content: "text", Embedding: vec, EmbeddingIdentity: "A"}
			if state == interfaces.KnowledgeDocumentPending {
				chunk.Embedding, chunk.EmbeddingIdentity, chunk.Pending = nil, "", true
			}
			if err := b.SaveKnowledgeDocumentWithChunks(ctx, admin, &interfaces.KnowledgeDocument{ID: id, OrgID: orgID, Filename: id + ".md", ChunkerVersion: version, State: state, Content: content},
				[]interfaces.KnowledgeChunk{chunk}); err != nil {
				t.Fatalf("save %s: %v", id, err)
			}
		}
		p := "contract-kbv-" + runID
		save(p+"-a-v1-src", 1, "", "source a")
		save(p+"-b-v1-nosrc", 1, "", "")
		save(p+"-c-v2", 2, "", "source c")
		save(p+"-d-v1-pending", 1, interfaces.KnowledgeDocumentPending, "source d")
		docs, err := b.ListKnowledgeDocumentsBelowChunkerVersion(ctx, orgID, 2, "", 0)
		if err != nil || len(docs) != 2 || docs[0].ID != p+"-a-v1-src" || !docs[0].HasSource || docs[1].ID != p+"-b-v1-nosrc" || docs[1].HasSource || docs[0].Content != "" {
			t.Fatalf("ListKnowledgeDocumentsBelowChunkerVersion(2) = %+v, %v; want the two ready v1 documents in id order, source flagged, no text", docs, err)
		}
		if docs, err := b.ListKnowledgeDocumentsBelowChunkerVersion(ctx, orgID, 2, p+"-a-v1-src", 1); err != nil || len(docs) != 1 || docs[0].ID != p+"-b-v1-nosrc" {
			t.Errorf("ListKnowledgeDocumentsBelowChunkerVersion after a = %+v, %v; want b", docs, err)
		}
		if docs, err := b.ListKnowledgeDocumentsBelowChunkerVersion(ctx, orgID, 1, "", 0); err != nil || len(docs) != 0 {
			t.Errorf("ListKnowledgeDocumentsBelowChunkerVersion(1) = %+v, %v; want none", docs, err)
		}
		if got, err := b.GetKnowledgeDocument(ctx, orgID, p+"-b-v1-nosrc"); err != nil || got.HasSource || got.Content != "" || got.State != interfaces.KnowledgeDocumentReady {
			t.Errorf("GetKnowledgeDocument without source = %+v, %v; want ready, no source", got, err)
		}
	})

	run("Knowledge_legacy_sample_and_per_document_stamp", func(t *testing.T) {
		admin, orgID := seed(t, "contract-kbs")
		p := "contract-kbs-" + runID
		d1, d2, d3, d4 := p+"-d1", p+"-d2", p+"-d3", p+"-d4"
		docChunks := map[string][]interfaces.KnowledgeChunk{
			d1: {{ID: d1 + "-a", Content: "d1 a", Embedding: vec}, {ID: d1 + "-b", Content: "d1 b", Embedding: vec}, {ID: d1 + "-c", Content: "d1 c", Embedding: vec}},
			d2: {{ID: d2 + "-a", Content: "d2 a", Embedding: vec}},
			d3: {{ID: d3 + "-a", Content: "d3 a", Embedding: vec, EmbeddingIdentity: "A"}, {ID: d3 + "-b", Content: "d3 b", Embedding: vec, EmbeddingIdentity: "A"}},
			d4: {{ID: d4 + "-a", Content: "d4 a", Pending: true}},
		}
		for _, id := range []string{d1, d2, d3, d4} {
			chunks := docChunks[id]
			for i := range chunks {
				chunks[i].DocID, chunks[i].OrgID = id, orgID
			}
			doc := &interfaces.KnowledgeDocument{ID: id, OrgID: orgID, Filename: id + ".md", ChunkerVersion: 1}
			if id == d4 {
				doc.State = interfaces.KnowledgeDocumentPending
			}
			if err := b.SaveKnowledgeDocumentWithChunks(ctx, admin, doc, chunks); err != nil {
				t.Fatalf("save %s: %v", id, err)
			}
		}
		sample, err := b.SampleLegacyKnowledgeChunkEmbeddings(ctx, orgID, 10)
		if err != nil || ids(sample) != d1+"-a,"+d1+"-c,"+d2+"-a" {
			t.Fatalf("SampleLegacyKnowledgeChunkEmbeddings(10) = %s, %v; want d1's first and last legacy chunk, then d2's only one", ids(sample), err)
		}
		for _, c := range sample {
			if len(c.Embedding) != len(vec) || c.Content == "" || c.DocID == "" {
				t.Errorf("sampled chunk %+v lacks its embedding, text or document", c)
			}
		}
		if sample, err := b.SampleLegacyKnowledgeChunkEmbeddings(ctx, orgID, 1); err != nil || ids(sample) != d1+"-a,"+d1+"-c" {
			t.Errorf("SampleLegacyKnowledgeChunkEmbeddings(1) = %s, %v; want d1's two ends", ids(sample), err)
		}

		n, err := b.StampKnowledgeDocumentIdentity(ctx, admin, orgID, []string{d1, d4}, "A")
		if err != nil || n != 3 {
			t.Fatalf("StampKnowledgeDocumentIdentity(d1, d4) = %d, %v; want d1's 3 legacy rows (d4's chunk is pending)", n, err)
		}
		if counts, _ := b.CountKnowledgeChunksByIdentity(ctx, orgID, "A"); counts.Current != 5 || counts.Legacy() != 1 {
			t.Errorf("counts after the stamp = %+v; want 5 current and d2's one legacy row left alone", counts)
		}
		if rest, err := b.ListPendingKnowledgeChunks(ctx, orgID, d4, "", 0); err != nil || ids(rest) != d4+"-a" {
			t.Errorf("d4's pending chunk after the stamp = %s, %v; want still pending", ids(rest), err)
		}
		if sample, err := b.SampleLegacyKnowledgeChunkEmbeddings(ctx, orgID, 10); err != nil || ids(sample) != d2+"-a" {
			t.Errorf("SampleLegacyKnowledgeChunkEmbeddings after the stamp = %s, %v; want d2's alone", ids(sample), err)
		}
		if n, err := b.StampKnowledgeDocumentIdentity(ctx, admin, orgID, nil, "A"); err != nil || n != 0 {
			t.Errorf("StampKnowledgeDocumentIdentity of no document = %d, %v; want 0", n, err)
		}
	})
}
