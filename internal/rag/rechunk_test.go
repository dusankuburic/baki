package rag

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// rechunkSource is a document whose v1 split alters its text ("3.5 mm" → "3. 5 mm") and v2 does not.
func rechunkSource(rules int) string {
	var sb strings.Builder
	sb.WriteString("# Margins\n\n")
	for i := range rules {
		fmt.Fprintf(&sb, "Rule %d keeps a 3.5 mm margin in flow.pad. ", i)
	}
	return sb.String()
}

// seedV1Document stores a ready document cut by chunker v1 and embedded under identity A (vec), with its source text
// kept (source "" = a document stored before the content column).
func seedV1Document(t *testing.T, store *testutil.FakeBackend, id, filename, text, source string, vec []float32) {
	t.Helper()
	parts := chunkDocument(1, text, filename)
	chunks := make([]interfaces.KnowledgeChunk, len(parts))
	for i, p := range parts {
		chunks[i] = interfaces.KnowledgeChunk{ID: fmt.Sprintf("%s-%04d", id, i), DocID: id, OrgID: "org-1", Content: p, Embedding: append([]float32(nil), vec...), EmbeddingIdentity: "A"}
	}
	if err := store.SaveKnowledgeDocumentWithChunks(context.Background(), "", &interfaces.KnowledgeDocument{ID: id, OrgID: "org-1", Filename: filename, ChunkerVersion: 1, Content: source}, chunks); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func docsByName(t *testing.T, store *testutil.FakeBackend) map[string]*interfaces.KnowledgeDocument {
	t.Helper()
	docs, err := store.ListKnowledgeDocuments(context.Background(), "org-1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*interfaces.KnowledgeDocument{}
	for _, d := range docs {
		if out[d.Filename] != nil {
			t.Fatalf("two documents named %s", d.Filename)
		}
		out[d.Filename] = d
	}
	return out
}

func chunkTexts(store *testutil.FakeBackend, docID string) string {
	var out []string
	for _, c := range store.KnowledgeChunks {
		if c.DocID == docID {
			out = append(out, c.Content)
		}
	}
	return strings.Join(out, "\n")
}

// TestRechunkOrg_RechunksTheDocumentsBelowTheVersion (R-knowledge-9): a plain re-index re-embeds v1 documents as they
// are; the rechunk re-index cuts the ones that kept their source text again at the current version — which then list
// chunkerVersion 2, their text no longer altered — skips (and reports) the one stored before the source was kept,
// and leaves a current document alone.
func TestRechunkOrg_RechunksTheDocumentsBelowTheVersion(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1")
	text := rechunkSource(60)
	seedV1Document(t, store, "doc-src", "margins.md", text, text, modelA)
	seedV1Document(t, store, "doc-old", "legacy.md", text, "", modelA)
	emb := &namedEmbedder{identity: "A", vec: modelA}
	svc := guardService(store, emb)
	if err := svc.AddDocument(context.Background(), "u1", "org-1", "current.md", "# Current\n\nAlready cut at the current version."); err != nil {
		t.Fatal(err)
	}
	current := docsByName(t, store)["current.md"]

	if _, err := svc.ReindexOrg(context.Background(), "u1", "org-1", true, interfaces.KnowledgeChunkCursor{}); err != nil {
		t.Fatalf("plain re-index: %v", err)
	}
	if d := docsByName(t, store)["margins.md"]; d.ID != "doc-src" || d.ChunkerVersion != 1 || !strings.Contains(chunkTexts(store, "doc-src"), "3. 5 mm") {
		t.Fatalf("after a plain re-index margins.md = %+v; want the v1 document intact", d)
	}

	res, err := svc.RechunkOrg(context.Background(), "u1", "org-1")
	if err != nil {
		t.Fatalf("RechunkOrg: %v", err)
	}
	byName := docsByName(t, store)
	re := byName["margins.md"]
	if re == nil || re.ChunkerVersion != chunkerVersion || re.State != interfaces.KnowledgeDocumentReady || re.ID == "doc-src" {
		t.Fatalf("margins.md after the rechunk = %+v; want a ready version-%d document in place of the v1 one", re, chunkerVersion)
	}
	if got := chunkTexts(store, re.ID); strings.Contains(got, "3. 5 mm") || strings.Contains(got, "flow. pad") {
		t.Errorf("rechunked text still altered:\n%.300s", got)
	}
	for _, c := range store.KnowledgeChunks {
		if c.DocID == re.ID && !strings.HasPrefix(c.Content, "Margins\n\n") {
			t.Errorf("rechunked chunk without its heading path: %.60q", c.Content)
		}
	}
	if want := len(chunkDocument(chunkerVersion, text, "margins.md")); re.ChunkCount != want {
		t.Errorf("rechunked into %d chunks, want %d", re.ChunkCount, want)
	}
	if old := byName["legacy.md"]; old == nil || old.ID != "doc-old" || old.ChunkerVersion != 1 {
		t.Errorf("legacy.md = %+v; want it skipped, still v1", old)
	}
	if d := byName["current.md"]; d == nil || d.ID != current.ID {
		t.Errorf("current.md = %+v; want it untouched (%s)", d, current.ID)
	}
	if res.Rechunked != 1 || res.SkippedNoSource != 1 || res.NextCursor != nil || res.Chunks != re.ChunkCount {
		t.Errorf("result = %+v; want 1 rechunked (%d chunks), 1 skipped without source, nothing left", res, re.ChunkCount)
	}

	// Nothing left below the version but the document without a source.
	again, err := svc.RechunkOrg(context.Background(), "u1", "org-1")
	if err != nil || again.Rechunked != 0 || again.Chunks != 0 || again.SkippedNoSource != 1 {
		t.Errorf("second rechunk = %+v, %v; want nothing to do", again, err)
	}
}

// TestRechunkOrg_ResumesAcrossRequests: a rechunk cut by the request budget answers a cursor; the next request
// resumes the document it left unfinished instead of buying its embedded chunks again.
func TestRechunkOrg_ResumesAcrossRequests(t *testing.T) {
	lowerReindexLimits(t, 100, 10000)
	lowerUploadBudget(t, 0)
	store := &testutil.FakeBackend{EmbeddingDim: 2}
	store.SeedOrgs("org-1")
	text := manyChunkDocument(250)
	seedV1Document(t, store, "doc-src", "big.txt", text, text, modelA)
	emb := &inputCountingEmbedder{vec: modelA}
	svc := guardService(store, emb)

	var requests int
	for {
		if requests++; requests > 10 {
			t.Fatal("the rechunk never finished")
		}
		res, err := svc.RechunkOrg(context.Background(), "u1", "org-1")
		if err != nil {
			t.Fatalf("request %d: %v", requests, err)
		}
		if res.NextCursor == nil {
			if res.Rechunked != 1 {
				t.Errorf("last request = %+v; want the document finished", res)
			}
			break
		}
		if !res.Truncated || res.Chunks == 0 {
			t.Errorf("request %d = %+v; want progress and a cursor", requests, res)
		}
	}
	if requests != 3 {
		t.Errorf("finished in %d requests, want 3 (100 + 100 + 50 chunks)", requests)
	}
	if got := emb.billed(); got != 250 {
		t.Errorf("bought %d inputs, want each of the 250 new chunks once", got)
	}
	if d := docsByName(t, store)["big.txt"]; d.ChunkerVersion != chunkerVersion || d.State != interfaces.KnowledgeDocumentReady {
		t.Errorf("big.txt = %+v; want ready at version %d", d, chunkerVersion)
	}
}
