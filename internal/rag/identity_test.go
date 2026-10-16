package rag

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
)

var updateChunkerGolden = flag.Bool("update-chunker-golden", false, "rewrite testdata/chunker_v1.golden (only with a chunkerVersion bump)")

// namedEmbedder answers every input with vec and names an embedding identity — "the model whose vectors point
// this way", distinguishable from another model by identity, and by direction when the identity is unknown.
type namedEmbedder struct {
	ai.Provider
	identity string
	vec      []float32
	calls    int
	inputs   []string
}

func (n *namedEmbedder) ID() string                { return "named-embed" }
func (n *namedEmbedder) EmbeddingIdentity() string { return n.identity }
func (n *namedEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	n.calls++
	n.inputs = append(n.inputs, text...)
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = append([]float32(nil), n.vec...)
	}
	return out, nil
}

// seedIdentityChunks stores, in one document of org-1, chunks whose stored vector is vec and whose identity is
// each of identities ("" = legacy, no identity), ids chunk-0, chunk-1, …
func seedIdentityChunks(store *tu.FakeBackend, vec []float32, identities ...string) {
	ctx := context.Background()
	_ = store.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: "doc-1", OrgID: "org-1", Filename: "policy.md"})
	chunks := make([]interfaces.KnowledgeChunk, len(identities))
	for i, id := range identities {
		chunks[i] = interfaces.KnowledgeChunk{ID: fmt.Sprintf("chunk-%d", i), DocID: "doc-1", Content: fmt.Sprintf("guideline %d", i), Embedding: append([]float32(nil), vec...), EmbeddingIdentity: id}
	}
	_ = store.SaveKnowledgeChunks(ctx, "", chunks)
}

func identitiesOf(store *tu.FakeBackend) map[string]string {
	out := map[string]string{}
	for id, c := range store.KnowledgeChunks {
		out[id] = c.EmbeddingIdentity
	}
	return out
}

// TestChunkText_GoldenGeometry pins the chunker: a fixed text splits into exactly the chunks in
// testdata/chunker_v1.golden under chunkerVersion 1. A geometry change (size, overlap, boundaries) changes the
// split of every document uploaded from then on while the stored ones keep the old one, so it must come with a
// chunkerVersion bump and a new golden — stripOverlapPrefix assumes the version's overlap at retrieval time.
func TestChunkText_GoldenGeometry(t *testing.T) {
	if chunkerVersion != 1 {
		t.Fatalf("chunkerVersion = %d; this golden pins version 1 — add chunker_v%d.golden and keep this test for the stored documents", chunkerVersion, chunkerVersion)
	}
	var sb strings.Builder
	for p := range 9 {
		for s := range 7 {
			fmt.Fprintf(&sb, "Paragraph %d sentence %d states the rule about retries, naming and logging in flows. ", p, s)
		}
		sb.WriteString("Ünïcödé runes count once each — not per byte.\n\n")
	}
	sb.WriteString(strings.Repeat("x", 2300))
	sb.WriteString("\n\nA short closing paragraph.")
	chunks := chunkText(sb.String(), knowledgeChunkRunes)
	var got strings.Builder
	fmt.Fprintf(&got, "chunkerVersion %d\n", chunkerVersion)
	for i, c := range chunks {
		fmt.Fprintf(&got, "===== chunk %d =====\n%s\n", i, c)
	}
	path := filepath.Join("testdata", "chunker_v1.golden")
	if *updateChunkerGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (create it with -update-chunker-golden)", err)
	}
	if want := strings.ReplaceAll(string(raw), "\r\n", "\n"); got.String() != want {
		t.Errorf("chunkText output changed under chunkerVersion %d: bump the version and refresh with -update-chunker-golden.\n--- golden\n%s--- now\n%s", chunkerVersion, want, got.String())
	}
}

// TestAddDocument_StampsIdentityAndChunkerVersion: every chunk an upload stores carries the embedder's identity,
// and the document the chunker version it was split with.
func TestAddDocument_StampsIdentityAndChunkerVersion(t *testing.T) {
	store := &tu.FakeBackend{}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	emb := &namedEmbedder{identity: "openai|https://api.openai.com/v1|text-embedding-3-small|1536", vec: []float32{1}}
	svc := guardService(store, emb)
	if err := svc.AddDocument(context.Background(), "u1", "org-1", "guide.md", "a guideline about retries"); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	for id, identity := range identitiesOf(store) {
		if identity != emb.identity {
			t.Errorf("chunk %s identity = %q, want the embedder's", id, identity)
		}
	}
	docs, _ := store.ListKnowledgeDocuments(context.Background(), "org-1", 0, 0)
	if len(docs) != 1 || docs[0].ChunkerVersion != chunkerVersion {
		t.Errorf("documents = %+v, want one at chunkerVersion %d", docs, chunkerVersion)
	}
}

// TestSearch_OtherIdentityChunksNotReturned (R1): a corpus embedded under another identity at the same width is
// not searched — the search answers nothing and counts a stranded search, not a width mismatch.
func TestSearch_OtherIdentityChunksNotReturned(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1}, "B", "B")
	emb := &namedEmbedder{identity: "A", vec: []float32{1}}
	svc := guardService(store, emb)
	stranded, dim := testutil.ToFloat64(ragStrandedSearchTotal), testutil.ToFloat64(ragDimMismatchTotal)
	got, err := svc.Search(context.Background(), "u1", "org-1", "retries?")
	if err != nil || got != "" {
		t.Fatalf("Search = %q, %v; want nothing from a corpus of another identity", got, err)
	}
	if d := testutil.ToFloat64(ragStrandedSearchTotal) - stranded; d != 1 {
		t.Errorf("pad_rag_stranded_search_total rose by %v, want 1", d)
	}
	if d := testutil.ToFloat64(ragDimMismatchTotal) - dim; d != 0 {
		t.Errorf("pad_rag_dim_mismatch_total rose by %v on an identity mismatch, want 0", d)
	}
}

// TestSearch_LegacyNullIdentityMatchesAnyIdentity: rows from before the column (no identity) are searched by
// every caller, as they were.
func TestSearch_LegacyNullIdentityMatchesAnyIdentity(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1}, "", "A")
	svc := guardService(store, &namedEmbedder{identity: "A", vec: []float32{1}})
	got, err := svc.Search(context.Background(), "u1", "org-1", "retries?")
	if err != nil || !strings.Contains(got, "guideline 0") || !strings.Contains(got, "guideline 1") {
		t.Errorf("Search = %q, %v; want the legacy and the matching chunk", got, err)
	}
	other := guardService(store, &namedEmbedder{identity: "C", vec: []float32{1}})
	got, err = other.Search(context.Background(), "u1", "org-1", "retries?")
	if err != nil || !strings.Contains(got, "guideline 0") || strings.Contains(got, "guideline 1") {
		t.Errorf("Search under C = %q, %v; want the legacy chunk only", got, err)
	}
}

// TestSearch_EmptyResultClassification (R4): a legitimate miss — chunks at the caller's identity and width, none
// above the relevance floor — bumps neither metric; a corpus of another width bumps the width metric only.
func TestSearch_EmptyResultClassification(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1, 0}, "A")
	stranded, dim := testutil.ToFloat64(ragStrandedSearchTotal), testutil.ToFloat64(ragDimMismatchTotal)
	svc := guardService(store, &namedEmbedder{identity: "A", vec: []float32{0, 1}})
	if got, err := svc.Search(context.Background(), "u1", "org-1", "unrelated"); err != nil || got != "" {
		t.Fatalf("orthogonal query: Search = %q, %v", got, err)
	}
	if s, d := testutil.ToFloat64(ragStrandedSearchTotal)-stranded, testutil.ToFloat64(ragDimMismatchTotal)-dim; s != 0 || d != 0 {
		t.Errorf("a legitimate miss moved the metrics (stranded +%v, dim +%v)", s, d)
	}
	// The width changed under the same identity string (R3): nothing is in the index at the configured width.
	store.EmbeddingDim = 3
	wide := guardService(store, &namedEmbedder{identity: "A", vec: []float32{1, 0, 0}})
	if got, err := wide.Search(context.Background(), "u1", "org-1", "retries"); err != nil || got != "" {
		t.Fatalf("other width: Search = %q, %v", got, err)
	}
	if s, d := testutil.ToFloat64(ragStrandedSearchTotal)-stranded, testutil.ToFloat64(ragDimMismatchTotal)-dim; s != 0 || d != 1 {
		t.Errorf("a width mismatch: stranded +%v, dim +%v; want 0 and 1", s, d)
	}
}

// TestAddDocument_StampsLegacyOnProvenMatch: the sample guard's proof that the caller's model produced the
// legacy rows stamps them with the caller's identity, so the next upload needs no billed sample at all.
func TestAddDocument_StampsLegacyOnProvenMatch(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1, 0}, "", "", "")
	emb := &namedEmbedder{identity: "A", vec: []float32{1, 0}}
	svc := guardService(store, emb)
	if err := svc.AddDocument(context.Background(), "u1", "org-1", "new.md", "a new guideline"); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if emb.calls != 2 {
		t.Errorf("embed calls = %d, want 2 (the sample proof, then the document)", emb.calls)
	}
	for id, identity := range identitiesOf(store) {
		if identity != "A" {
			t.Errorf("chunk %s identity = %q after the proof, want A", id, identity)
		}
	}
	// Proven and stamped: the next upload skips the sample.
	if err := svc.AddDocument(context.Background(), "u1", "org-1", "another.md", "another guideline"); err != nil {
		t.Fatalf("second AddDocument: %v", err)
	}
	if emb.calls != 3 {
		t.Errorf("embed calls = %d, want 3 (no sample once every row is stamped)", emb.calls)
	}
	// A model that does not reproduce the legacy vectors is still refused, and stamps nothing.
	legacy := &tu.FakeBackend{EmbeddingDim: 2}
	legacy.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(legacy, []float32{1, 0}, "", "")
	other := guardService(legacy, &namedEmbedder{identity: "B", vec: []float32{0, 1}})
	if err := other.AddDocument(context.Background(), "u1", "org-1", "new.md", "text"); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Errorf("upload by another model into a legacy corpus: %v, want ErrEmbeddingModelChanged", err)
	}
	for id, identity := range identitiesOf(legacy) {
		if identity != "" {
			t.Errorf("chunk %s stamped %q by a refused upload", id, identity)
		}
	}
}

// TestCheckCorpusModel_SkippedWhenIdentityMatches (R5): a corpus stamped with the caller's identity is accepted
// without a billed sample call, whatever the vectors say.
func TestCheckCorpusModel_SkippedWhenIdentityMatches(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1, 0}, "A", "A")
	// The vectors point elsewhere: a sample would have refused this model.
	emb := &namedEmbedder{identity: "A", vec: []float32{0, 1}}
	svc := guardService(store, emb)
	if err := svc.AddDocument(context.Background(), "u1", "org-1", "new.md", "a new guideline"); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if emb.calls != 1 {
		t.Errorf("embed calls = %d, want 1 (the document only; no sample)", emb.calls)
	}
}

// TestCheckCorpusModel_StrandedIdentityRefuses: rows stamped with another identity refuse the upload even when
// the sample happens to agree (the identity knows what the sample cannot).
func TestCheckCorpusModel_StrandedIdentityRefuses(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1, 0}, "B", "B")
	emb := &namedEmbedder{identity: "A", vec: []float32{1, 0}}
	svc := guardService(store, emb)
	if err := svc.AddDocument(context.Background(), "u1", "org-1", "new.md", "a new guideline"); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Errorf("upload into a corpus of another identity: %v, want ErrEmbeddingModelChanged", err)
	}
	if _, err := svc.ReindexOrg(context.Background(), "u1", "org-1", false, interfaces.KnowledgeChunkCursor{}); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Errorf("unforced re-index of a corpus of another identity: %v, want ErrEmbeddingModelChanged", err)
	}
}

// TestReindexOrg_StampsIdentity_WalksOnlyForeignChunks (R6): a forced re-index rewrites every chunk under the
// caller's identity; an unforced one afterwards finds nothing left to convert and buys no embeddings beyond
// the proof; a legacy corpus the caller's model reproduces is stamped by the proof and not re-bought.
func TestReindexOrg_StampsIdentity_WalksOnlyForeignChunks(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1, 0}, "A", "A", "B", "B", "")
	emb := &namedEmbedder{identity: "A", vec: []float32{1, 0}}
	svc := guardService(store, emb)
	res, err := svc.ReindexOrg(context.Background(), "u1", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if err != nil || res.Chunks != 5 {
		t.Fatalf("forced re-index = %+v, %v; want every chunk rewritten", res, err)
	}
	for id, identity := range identitiesOf(store) {
		if identity != "A" {
			t.Errorf("chunk %s identity = %q after a forced re-index, want A", id, identity)
		}
	}
	calls := emb.calls
	res, err = svc.ReindexOrg(context.Background(), "u1", "org-1", false, interfaces.KnowledgeChunkCursor{})
	if err != nil || res.Chunks != 0 || res.NextCursor != nil {
		t.Errorf("unforced re-index of a current corpus = %+v, %v; want nothing to do", res, err)
	}
	if emb.calls != calls {
		t.Errorf("an unforced re-index of a current corpus embedded (%d calls)", emb.calls-calls)
	}

	legacy := &tu.FakeBackend{EmbeddingDim: 2}
	legacy.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(legacy, []float32{1, 0}, "", "", "")
	emb = &namedEmbedder{identity: "A", vec: []float32{1, 0}}
	svc = guardService(legacy, emb)
	res, err = svc.ReindexOrg(context.Background(), "u1", "org-1", false, interfaces.KnowledgeChunkCursor{})
	if err != nil || res.Chunks != 0 {
		t.Errorf("unforced re-index of a reproduced legacy corpus = %+v, %v; want the proof to stamp it and nothing re-bought", res, err)
	}
	if emb.calls != 1 {
		t.Errorf("embed calls = %d, want 1 (the proof)", emb.calls)
	}
	for id, identity := range identitiesOf(legacy) {
		if identity != "A" {
			t.Errorf("legacy chunk %s identity = %q after the proof, want A", id, identity)
		}
	}
}

// TestReindexOrg_WidthRecoveryStampsNewIdentity: the stranded-corpus recovery (a width change, nothing in the
// vector index) still proceeds unforced and leaves every chunk under the new identity.
func TestReindexOrg_WidthRecoveryStampsNewIdentity(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1, 0}, "A|1536", "A|1536")
	emb := &namedEmbedder{identity: "A|3072", vec: []float32{1}}
	svc := guardService(store, emb)
	res, err := svc.ReindexOrg(context.Background(), "u1", "org-1", false, interfaces.KnowledgeChunkCursor{})
	if err != nil || res.Chunks != 2 {
		t.Fatalf("width recovery = %+v, %v; want both chunks converted unforced", res, err)
	}
	for id, c := range store.KnowledgeChunks {
		if c.EmbeddingIdentity != "A|3072" || len(c.Embedding) != 1 {
			t.Errorf("chunk %s = identity %q width %d, want A|3072 at width 1", id, c.EmbeddingIdentity, len(c.Embedding))
		}
	}
}

// TestListDocuments_CarriesEmbeddingStranded: the admin list counts, per document, the chunks at another
// identity than the caller's; a caller without an embedder sees no stranded count.
func TestListDocuments_CarriesEmbeddingStranded(t *testing.T) {
	store := &tu.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedIdentityChunks(store, []float32{1, 0}, "A", "B", "")
	svc := guardService(store, &namedEmbedder{identity: "A", vec: []float32{1, 0}})
	docs, err := svc.ListDocuments(context.Background(), "u1", "org-1", 0, 0)
	if err != nil || len(docs) != 1 || docs[0].EmbeddingStranded != 1 || docs[0].ChunkCount != 3 {
		t.Errorf("ListDocuments = %+v, %v; want one document with 3 chunks, 1 stranded", docs, err)
	}
	none := NewKnowledgeService(store, nil, nil)
	docs, err = none.ListDocuments(context.Background(), "u1", "org-1", 0, 0)
	if err != nil || len(docs) != 1 || docs[0].EmbeddingStranded != 0 {
		t.Errorf("ListDocuments without an embedder = %+v, %v; want no stranded count", docs, err)
	}
}
