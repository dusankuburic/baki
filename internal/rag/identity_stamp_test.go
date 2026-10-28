package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
)

// recordingAudit collects the entries the service records.
type recordingAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (r *recordingAudit) Record(_ context.Context, e audit.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
}

func (r *recordingAudit) stamps() []audit.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []audit.Entry
	for _, e := range r.entries {
		if e.Action == audit.ActionKnowledgeIdentityStamp {
			out = append(out, e)
		}
	}
	return out
}

// seedLegacyDocs stores, in org-1, one legacy document (no identity) per entry of vecs — doc-1, doc-2, … — each with
// two chunks whose stored vector is that entry.
func seedLegacyDocs(store *tu.FakeBackend, vecs ...[]float32) {
	ctx := context.Background()
	for i, vec := range vecs {
		doc := fmt.Sprintf("doc-%d", i+1)
		_ = store.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: doc, OrgID: "org-1", Filename: doc + ".md"})
		_ = store.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{
			{ID: doc + "-a", DocID: doc, Content: doc + " first rule", Embedding: append([]float32(nil), vec...)},
			{ID: doc + "-b", DocID: doc, Content: doc + " second rule", Embedding: append([]float32(nil), vec...)},
		})
	}
}

// identityByDoc is each document's chunk identities ("" legacy), joined.
func identityByDoc(store *tu.FakeBackend) map[string]string {
	out := map[string]string{}
	for _, c := range store.KnowledgeChunks {
		if out[c.DocID] == "" || out[c.DocID] == c.EmbeddingIdentity {
			out[c.DocID] = c.EmbeddingIdentity
		} else {
			out[c.DocID] += "," + c.EmbeddingIdentity
		}
	}
	return out
}

var (
	modelA = []float32{1, 0}
	modelB = []float32{0, 1}
)

// TestCheckCorpusModel_StampsOnlyTheProvenDocuments (R-knowledge-3): a legacy corpus of 3 documents model A
// reproduces and 2 it does not. An upload by A proves and stamps the documents its sample covered and no other —
// never the B documents — and records the stamp, naming the proven documents, in the audit trail; the next upload's
// sample reaches the B documents and refuses.
func TestCheckCorpusModel_StampsOnlyTheProvenDocuments(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1")
	seedLegacyDocs(store, modelA, modelA, modelA, modelB, modelB)
	trail := &recordingAudit{}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(&namedEmbedder{identity: "A", vec: modelA}), WithAuditRecorder(trail))

	if err := svc.AddDocument(context.Background(), "u1", "org-1", "new.md", "a new guideline"); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	ids := identityByDoc(store)
	for _, doc := range []string{"doc-1", "doc-2", "doc-3"} {
		if ids[doc] != "A" {
			t.Errorf("%s identity = %q, want A (proven by the sample)", doc, ids[doc])
		}
	}
	for _, doc := range []string{"doc-4", "doc-5"} {
		if ids[doc] != "" {
			t.Errorf("%s identity = %q, want it still legacy (never proven)", doc, ids[doc])
		}
	}
	stamps := trail.stamps()
	if len(stamps) != 1 || stamps[0].OrgID != "org-1" || stamps[0].Meta["documents"] != "doc-1,doc-2,doc-3" || stamps[0].Meta["identity"] != "A" || stamps[0].Meta["chunks"] != "6" {
		t.Fatalf("audit stamps = %+v; want one naming doc-1..3 (6 chunks) under A", stamps)
	}

	if err := svc.AddDocument(context.Background(), "u1", "org-1", "another.md", "another guideline"); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Errorf("the next upload, whose sample reaches the B documents: %v, want ErrEmbeddingModelChanged", err)
	}
	for _, doc := range []string{"doc-4", "doc-5"} {
		if got := identityByDoc(store)[doc]; got != "" {
			t.Errorf("%s stamped %q by a refused upload", doc, got)
		}
	}
	if n := len(trail.stamps()); n != 1 {
		t.Errorf("%d stamps recorded, want the first one only", n)
	}
}

// TestCheckCorpusModel_ProofCoversBothEndsOfADocument: a document a re-index on the old release stopped inside holds
// the new model's vectors up to the cut and the old one's after it, all without an identity. The proof samples both
// ends of the document's legacy chunks, so the document is not proven by its converted head.
func TestCheckCorpusModel_ProofCoversBothEndsOfADocument(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1")
	ctx := context.Background()
	_ = store.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: "doc-1", OrgID: "org-1", Filename: "split.md"})
	_ = store.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{
		{ID: "c-1", DocID: "doc-1", Content: "converted head", Embedding: modelA},
		{ID: "c-2", DocID: "doc-1", Content: "converted head too", Embedding: modelA},
		{ID: "c-3", DocID: "doc-1", Content: "old tail", Embedding: modelB},
	})
	trail := &recordingAudit{}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(&namedEmbedder{identity: "A", vec: modelA}), WithAuditRecorder(trail))
	if err := svc.AddDocument(ctx, "u1", "org-1", "new.md", "a new guideline"); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Errorf("upload into a half-converted legacy document: %v, want ErrEmbeddingModelChanged", err)
	}
	for id, identity := range identitiesOf(store) {
		if identity != "" {
			t.Errorf("chunk %s stamped %q", id, identity)
		}
	}
	if len(trail.stamps()) != 0 {
		t.Errorf("a stamp was recorded: %+v", trail.stamps())
	}
}

// TestReindexOrg_UnforcedRestartNeverStampsTheUnconvertedTail (R-knowledge-3): a legacy model-B corpus whose forced
// re-index to model A stopped after converting 3 documents (each converted chunk carries A) is restarted unforced.
// The org-wide stamp proved the converted head and labelled the unconverted B tail A; the proof now samples the
// documents still holding legacy rows — the B tail — and refuses, so no B row is ever stamped A.
func TestReindexOrg_UnforcedRestartNeverStampsTheUnconvertedTail(t *testing.T) {
	lowerReindexLimits(t, 6, 10000) // one page = the 3 first documents (2 chunks each)
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1")
	seedLegacyDocs(store, modelB, modelB, modelB, modelB, modelB)
	ctx := context.Background()

	// The forced run converts the first page, then the provider fails.
	forced := &stoppingEmbedder{namedEmbedder: namedEmbedder{identity: "A", vec: modelA}, failAt: 2}
	res, err := guardService(store, forced).ReindexOrg(ctx, "u1", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if err == nil || res.Chunks != 6 {
		t.Fatalf("forced re-index = %+v, %v; want it stopped after the first page", res, err)
	}
	ids := identityByDoc(store)
	if ids["doc-1"] != "A" || ids["doc-3"] != "A" || ids["doc-4"] != "" || ids["doc-5"] != "" {
		t.Fatalf("identities after the stopped run = %v; want doc-1..3 A, doc-4..5 legacy", ids)
	}

	trail := &recordingAudit{}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(&namedEmbedder{identity: "A", vec: modelA}), WithAuditRecorder(trail))
	if _, err := svc.ReindexOrg(ctx, "u1", "org-1", false, interfaces.KnowledgeChunkCursor{}); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Errorf("unforced restart: %v, want ErrEmbeddingModelChanged (the tail is another model)", err)
	}
	for _, doc := range []string{"doc-4", "doc-5"} {
		if got := identityByDoc(store)[doc]; got != "" {
			t.Errorf("%s stamped %q by the unforced restart", doc, got)
		}
		for _, c := range store.KnowledgeChunks {
			if c.DocID == doc && (len(c.Embedding) != 2 || c.Embedding[1] != 1) {
				t.Errorf("%s's vectors changed: %v", c.ID, c.Embedding)
			}
		}
	}
	if len(trail.stamps()) != 0 {
		t.Errorf("a stamp was recorded: %+v", trail.stamps())
	}
	// Forced, the run completes the conversion.
	if res, err := guardService(store, &namedEmbedder{identity: "A", vec: modelA}).ReindexOrg(ctx, "u1", "org-1", true, interfaces.KnowledgeChunkCursor{}); err != nil || res.NextCursor != nil {
		t.Fatalf("forced restart = %+v, %v", res, err)
	}
	if ids := identityByDoc(store); strings.Count(fmt.Sprint(ids), ":A") != 5 {
		t.Errorf("identities after the forced restart = %v; want every document A", ids)
	}
}

// stoppingEmbedder is namedEmbedder failing on its failAt-th call.
type stoppingEmbedder struct {
	namedEmbedder
	failAt int
}

func (s *stoppingEmbedder) Embed(ctx context.Context, text []string) ([][]float32, error) {
	if s.calls+1 == s.failAt {
		s.calls++
		return nil, errors.New("provider unavailable")
	}
	return s.namedEmbedder.Embed(ctx, text)
}
