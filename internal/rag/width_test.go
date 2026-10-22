package rag

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
)

// widthLogProbe records every slog record while installed.
type widthLogProbe struct {
	mu      sync.Mutex
	records []slog.Record
}

func (p *widthLogProbe) Enabled(context.Context, slog.Level) bool { return true }
func (p *widthLogProbe) WithAttrs([]slog.Attr) slog.Handler       { return p }
func (p *widthLogProbe) WithGroup(string) slog.Handler            { return p }
func (p *widthLogProbe) Handle(_ context.Context, r slog.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, r.Clone())
	return nil
}

// warnings returns the WARN-or-worse records whose message contains substr.
func (p *widthLogProbe) warnings(substr string) []slog.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []slog.Record
	for _, r := range p.records {
		if r.Level >= slog.LevelWarn && strings.Contains(r.Message, substr) {
			out = append(out, r)
		}
	}
	return out
}

func installWidthLogProbe(t *testing.T) *widthLogProbe {
	t.Helper()
	p := &widthLogProbe{}
	prev := slog.Default()
	slog.SetDefault(slog.New(p))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return p
}

// recordAttrs flattens a record's attributes to strings.
func recordAttrs(r slog.Record) map[string]string {
	out := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.String()
		return true
	})
	return out
}

// widthService is a knowledge service whose deployment width is configured — the factory's, as DI sets it from
// PAD_EMBEDDING_DIM — and whose embedder is emb.
func widthService(store *tu.FakeBackend, configured int, emb ai.Provider) *KnowledgeService {
	factory := ai.NewProviderFactory(nil, nil, nil, nil)
	factory.SetEmbeddingDimensions(configured)
	return NewKnowledgeService(store, factory, nil, WithEmbedder(emb))
}

// The two reports of a width-2 deployment over width-3 vectors: the embedder's own, and the stored chunks'.
var (
	embedderAt3 = EmbeddingWidthMismatch{Configured: 2, Observed: 3, Source: WidthSourceEmbedder}
	storedAt3   = EmbeddingWidthMismatch{Configured: 2, Observed: 3, Source: WidthSourceStored}
)

// TestKnowledge_WidthMismatchWarnsOncePerOrg (R-config-8): an embedder whose vectors are not PAD_EMBEDDING_DIM wide
// is logged once per org, with both widths — not per chunk, not per search, not per re-index page — and another
// org is logged on its own.
func TestKnowledge_WidthMismatchWarnsOncePerOrg(t *testing.T) {
	probe := installWidthLogProbe(t)
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1", "org-2") // the knowledge base needs its organisation
	svc := widthService(store, 2, &namedEmbedder{identity: "wide", vec: []float32{1, 0, 0}})
	ctx := context.Background()
	for _, name := range []string{"a.md", "b.md"} {
		if err := svc.AddDocument(ctx, "u1", "org-1", name, "retry every call"); err != nil {
			t.Fatalf("AddDocument %s: %v", name, err)
		}
	}
	for range 2 {
		if _, err := svc.Search(ctx, "u1", "org-1", "retries"); err != nil {
			t.Fatalf("Search: %v", err)
		}
	}
	if _, err := svc.ReindexOrg(ctx, "u1", "org-1", true, interfaces.KnowledgeChunkCursor{}); err != nil {
		t.Fatalf("ReindexOrg: %v", err)
	}
	warns := probe.warnings("PAD_EMBEDDING_DIM")
	if len(warns) != 1 {
		t.Fatalf("got %d width warnings over 2 uploads, 2 searches and a re-index of org-1, want exactly 1", len(warns))
	}
	if attrs := recordAttrs(warns[0]); attrs["org_id"] != "org-1" || attrs["configured_dim"] != "2" || attrs["observed_dim"] != "3" {
		t.Errorf("warning attrs = %v, want org-1 with configured_dim 2 and observed_dim 3", attrs)
	}

	if err := svc.AddDocument(ctx, "u1", "org-2", "a.md", "name flows by team"); err != nil {
		t.Fatalf("AddDocument org-2: %v", err)
	}
	warns = probe.warnings("PAD_EMBEDDING_DIM")
	if len(warns) != 2 || recordAttrs(warns[1])["org_id"] != "org-2" {
		t.Errorf("after org-2's upload: %d width warnings, want 2 with the second naming org-2", len(warns))
	}

	// At the configured width nothing is logged.
	store3 := &tu.FakeBackend{EmbeddingDim: 2}
	store3.SeedOrgs("org-3")
	same := widthService(store3, 2, &namedEmbedder{identity: "fits", vec: []float32{1, 0}})
	if err := same.AddDocument(ctx, "u1", "org-3", "a.md", "retry every call"); err != nil {
		t.Fatalf("AddDocument org-3: %v", err)
	}
	if _, err := same.Search(ctx, "u1", "org-3", "retries"); err != nil {
		t.Fatalf("Search org-3: %v", err)
	}
	if warns = probe.warnings("PAD_EMBEDDING_DIM"); len(warns) != 2 {
		t.Errorf("an embedder at the configured width was logged: %d width warnings, want still 2", len(warns))
	}

	// What the embedder produced is what the listing reports, as the embedder's own width.
	docs, err := svc.ListDocuments(ctx, "u1", "org-1", 0, 0)
	if err != nil || len(docs) != 2 {
		t.Fatalf("ListDocuments = %d docs, %v; want 2", len(docs), err)
	}
	if m := svc.EmbeddingWidthMismatch(ctx, "org-1", docs); m == nil || *m != embedderAt3 {
		t.Errorf("EmbeddingWidthMismatch(org-1) = %+v, want %+v", m, embedderAt3)
	}
	docs, _ = same.ListDocuments(ctx, "u1", "org-3", 0, 0)
	if m := same.EmbeddingWidthMismatch(ctx, "org-3", docs); m != nil {
		t.Errorf("EmbeddingWidthMismatch(org-3) = %+v, want nil at the configured width", m)
	}
}

// TestEmbeddingWidthMismatch_Pure (R-config-8): the width check is a mismatch only when both widths are known and
// differ — a service without a configured width, or an empty vector, has nothing to compare — and carries the side
// it was asked about.
func TestEmbeddingWidthMismatch_Pure(t *testing.T) {
	for _, tc := range []struct {
		configured, observed int
		want                 bool
	}{
		{1536, 3072, true},
		{1536, 768, true},
		{1536, 1536, false},
		{1536, 0, false},
		{0, 3072, false},
		{0, 0, false},
	} {
		for _, source := range []string{WidthSourceEmbedder, WidthSourceStored} {
			m, got := embeddingWidthMismatch(tc.configured, tc.observed, source)
			if got != tc.want {
				t.Errorf("embeddingWidthMismatch(%d, %d, %s) = %v, want %v", tc.configured, tc.observed, source, got, tc.want)
			}
			if got && (m.Configured != tc.configured || m.Observed != tc.observed || m.Source != source) {
				t.Errorf("embeddingWidthMismatch(%d, %d, %s) = %+v, want both widths and the source", tc.configured, tc.observed, source, m)
			}
		}
	}
}

// TestEmbeddingWidthMismatch_ListingProbesStrandedCorpus (R-config-8): a corpus stranded before this process
// started — nothing embedded or searched here yet — is measured from its stored chunks outside the vector index
// when the listing shows some, reported as the stored side, logged once like an embed, and not probed when every
// chunk is indexed or the deployment's width is unknown.
func TestEmbeddingWidthMismatch_ListingProbesStrandedCorpus(t *testing.T) {
	probe := installWidthLogProbe(t)
	ctx := context.Background()
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1, 0, 0}, "wide", "wide")
	svc := widthService(store, 2, &namedEmbedder{identity: "wide", vec: []float32{1, 0, 0}})
	docs, err := svc.ListDocuments(ctx, "u1", "org-1", 0, 0)
	if err != nil || len(docs) != 1 || docs[0].VectorIndexed != 0 {
		t.Fatalf("ListDocuments = %+v, %v; want one document with nothing indexed", docs, err)
	}
	for range 2 {
		if m := svc.EmbeddingWidthMismatch(ctx, "org-1", docs); m == nil || *m != storedAt3 {
			t.Errorf("EmbeddingWidthMismatch = %+v, want %+v from the stored chunks", m, storedAt3)
		}
	}
	if warns := probe.warnings("PAD_EMBEDDING_DIM"); len(warns) != 1 {
		t.Errorf("got %d width warnings from two listings, want 1", len(warns))
	}

	fits := &tu.FakeBackend{EmbeddingDim: 2}
	fits.SeedOrgs("org-1")
	seedIdentityChunks(fits, []float32{1, 0}, "fits", "fits")
	indexed := widthService(fits, 2, &namedEmbedder{identity: "fits", vec: []float32{1, 0}})
	docs, _ = indexed.ListDocuments(ctx, "u1", "org-1", 0, 0)
	if m := indexed.EmbeddingWidthMismatch(ctx, "org-1", docs); m != nil {
		t.Errorf("EmbeddingWidthMismatch of an indexed corpus = %+v, want nil", m)
	}
	unknown := NewKnowledgeService(store, nil, nil, WithEmbedder(&namedEmbedder{identity: "wide", vec: []float32{1, 0, 0}}))
	docs, _ = unknown.ListDocuments(ctx, "u1", "org-1", 0, 0)
	if m := unknown.EmbeddingWidthMismatch(ctx, "org-1", docs); m != nil {
		t.Errorf("EmbeddingWidthMismatch without a configured width = %+v, want nil", m)
	}
}

// TestKnowledge_WidthMismatchForgottenWhenEmbedderIsBackAtConfiguredWidth (R-config-8): the listing's memo of a
// mismatch is what the embedder produced LAST — a vector back at the configured width forgets it, so the listing
// measures the stored chunks again (a corpus still to re-index reports their width as the stored side; a
// re-indexed one reports nothing), while the warning stays once per org: a return to the wrong width is not logged
// twice, a change of the observed width is.
func TestKnowledge_WidthMismatchForgottenWhenEmbedderIsBackAtConfiguredWidth(t *testing.T) {
	probe := installWidthLogProbe(t)
	ctx := context.Background()
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	emb := &namedEmbedder{identity: "model", vec: []float32{1, 0, 0}}
	svc := widthService(store, 2, emb)
	if err := svc.AddDocument(ctx, "u1", "org-1", "a.md", "retry every call"); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	docs, err := svc.ListDocuments(ctx, "u1", "org-1", 0, 0)
	if err != nil || len(docs) != 1 {
		t.Fatalf("ListDocuments = %d docs, %v; want 1", len(docs), err)
	}
	if m := svc.EmbeddingWidthMismatch(ctx, "org-1", docs); m == nil || *m != embedderAt3 {
		t.Fatalf("EmbeddingWidthMismatch after the wide upload = %+v, want %+v", m, embedderAt3)
	}

	// The model now fits (PAD_EMBEDDING_DIM honoured, or another model): the stored chunks are still wide.
	emb.vec = []float32{1, 0}
	if _, err := svc.Search(ctx, "u1", "org-1", "retries"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	docs, _ = svc.ListDocuments(ctx, "u1", "org-1", 0, 0)
	if m := svc.EmbeddingWidthMismatch(ctx, "org-1", docs); m == nil || *m != storedAt3 {
		t.Errorf("EmbeddingWidthMismatch of the still-wide corpus = %+v, want %+v from the stored chunks", m, storedAt3)
	}
	if _, err := svc.ReindexOrg(ctx, "u1", "org-1", false, interfaces.KnowledgeChunkCursor{}); err != nil {
		t.Fatalf("ReindexOrg: %v", err)
	}
	docs, _ = svc.ListDocuments(ctx, "u1", "org-1", 0, 0)
	if len(docs) != 1 || docs[0].VectorIndexed != docs[0].ChunkCount {
		t.Fatalf("after the re-index: %+v, want every chunk indexed", docs)
	}
	if m := svc.EmbeddingWidthMismatch(ctx, "org-1", docs); m != nil {
		t.Errorf("EmbeddingWidthMismatch of the re-indexed corpus = %+v, want nil: the memo outlived the mismatch", m)
	}
	if warns := probe.warnings("PAD_EMBEDDING_DIM"); len(warns) != 1 {
		t.Errorf("got %d width warnings over an upload, a search, two listings and a re-index, want 1", len(warns))
	}

	// Another wrong width is news; the same one again is not.
	emb.vec = []float32{1, 0, 0, 0}
	if err := svc.AddDocument(ctx, "u1", "org-1", "b.md", "name flows by team"); err != nil {
		t.Fatalf("AddDocument b.md: %v", err)
	}
	warns := probe.warnings("PAD_EMBEDDING_DIM")
	if len(warns) != 2 || recordAttrs(warns[1])["observed_dim"] != "4" {
		t.Fatalf("after the upload at width 4: %d width warnings, want 2 with the second naming observed_dim 4", len(warns))
	}
	emb.vec = []float32{1, 0}
	if _, err := svc.Search(ctx, "u1", "org-1", "retries"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	emb.vec = []float32{1, 0, 0, 0}
	if _, err := svc.Search(ctx, "u1", "org-1", "retries"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if warns := probe.warnings("PAD_EMBEDDING_DIM"); len(warns) != 2 {
		t.Errorf("the width 4 the org was already warned about was logged again: %d width warnings, want still 2", len(warns))
	}
}
