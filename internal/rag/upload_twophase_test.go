package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// inputCountingEmbedder counts the inputs of every Embed call that returned vectors — what the provider bills — and
// waits sleep per call; cancelAfter > 0 cancels the request on the call that returns vectors for the cancelAfter-th
// time (the client going away right after a batch was bought).
type inputCountingEmbedder struct {
	ai.Provider
	mu          sync.Mutex
	sleep       time.Duration
	calls       int
	inputs      int
	cancelAfter int
	cancel      context.CancelFunc
	vec         []float32
}

func (e *inputCountingEmbedder) ID() string { return "input-counting-embed" }
func (e *inputCountingEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	if e.sleep > 0 {
		time.Sleep(e.sleep)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.inputs += len(text)
	if e.cancelAfter > 0 && e.calls == e.cancelAfter && e.cancel != nil {
		e.cancel()
	}
	vec := e.vec
	if vec == nil {
		vec = []float32{1, 0}
	}
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = append([]float32(nil), vec...)
	}
	return out, nil
}

func (e *inputCountingEmbedder) billed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inputs
}

// manyChunkDocument is a plain-text document that chunker v2 cuts into exactly n chunks: n paragraphs too long to
// share a chunk with the carried overlap.
func manyChunkDocument(n int) string {
	paras := make([]string, n)
	for i := range paras {
		paras[i] = fmt.Sprintf("Paragraph %05d. ", i) + strings.Repeat("w", 900)
	}
	return strings.Join(paras, "\n\n")
}

// lowerUploadBudget shrinks the request budget for one test.
func lowerUploadBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	prev := reindexRequestBudget
	reindexRequestBudget = budget
	t.Cleanup(func() { reindexRequestBudget = prev })
}

func listedDoc(t *testing.T, store *testutil.FakeBackend, id string) *interfaces.KnowledgeDocument {
	t.Helper()
	docs, err := store.ListKnowledgeDocuments(context.Background(), "org-1", 0, 0)
	if err != nil {
		t.Fatalf("ListKnowledgeDocuments: %v", err)
	}
	for _, d := range docs {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("document %s not listed", id)
	return nil
}

// TestUploadDocument_LargeUploadAnswersWithinTheBudgetAndResumes (R-knowledge-2): an upload of the largest document
// one upload may index, against an embedder that sleeps per batch, answers within the request budget with the
// document stored pending and a nextCursor; following it completes the document, the listing's pendingChunks
// falling with every request, and every chunk is bought exactly once.
func TestUploadDocument_LargeUploadAnswersWithinTheBudgetAndResumes(t *testing.T) {
	lowerUploadBudget(t, 40*time.Millisecond)
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1")
	emb := &inputCountingEmbedder{sleep: 10 * time.Millisecond}
	svc := guardService(store, emb)
	content := manyChunkDocument(maxKnowledgeChunks)
	if n := len(chunkDocument(chunkerVersion, content, "big.txt")); n != maxKnowledgeChunks {
		t.Fatalf("fixture rot: %d chunks, want %d", n, maxKnowledgeChunks)
	}

	began := time.Now()
	res, err := svc.UploadDocument(context.Background(), "scope", "org-1", "big.txt", content)
	if err != nil {
		t.Fatalf("UploadDocument: %v", err)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Errorf("the upload took %v; want it to stop at the request budget", elapsed)
	}
	if res.ID == "" || res.State != interfaces.KnowledgeDocumentPending || res.NextCursor == nil || !res.Truncated ||
		res.Chunks != maxKnowledgeChunks || res.PendingChunks != maxKnowledgeChunks-res.Embedded || res.Embedded == 0 || res.ChunkerVersion != chunkerVersion {
		t.Fatalf("first answer = %+v; want the document pending with part of it embedded and a cursor", res)
	}
	if d := listedDoc(t, store, res.ID); d.PendingChunks != res.PendingChunks || d.State != interfaces.KnowledgeDocumentPending {
		t.Errorf("listed = %+v; want pendingChunks %d", d, res.PendingChunks)
	}

	pending, requests := res.PendingChunks, 1
	for res.NextCursor != nil {
		if requests++; requests > 50 {
			t.Fatal("the upload never completed")
		}
		res, err = svc.ContinueDocument(context.Background(), "scope", "org-1", res.ID, *res.NextCursor)
		if err != nil {
			t.Fatalf("ContinueDocument (request %d): %v", requests, err)
		}
		if res.PendingChunks >= pending {
			t.Errorf("request %d: pendingChunks %d, not below %d", requests, res.PendingChunks, pending)
		}
		pending = res.PendingChunks
		if d := listedDoc(t, store, res.ID); d.PendingChunks != pending {
			t.Errorf("request %d: listed pendingChunks %d, want %d", requests, d.PendingChunks, pending)
		}
	}
	if res.State != interfaces.KnowledgeDocumentReady || res.PendingChunks != 0 || requests < 3 {
		t.Errorf("last answer = %+v after %d requests; want the document ready, over several requests", res, requests)
	}
	if got := emb.billed(); got != maxKnowledgeChunks {
		t.Errorf("embedded %d inputs, want each of the %d chunks once", got, maxKnowledgeChunks)
	}
	if total, _, _ := store.CountKnowledgeChunks(context.Background(), "org-1"); total != maxKnowledgeChunks {
		t.Errorf("corpus = %d chunks after the finish, want %d", total, maxKnowledgeChunks)
	}
}

// TestUploadDocument_CancelledMidUploadKeepsWhatItBought: the client going away mid-upload leaves the document
// pending with every batch it bought committed; a continuation embeds the rest, and no chunk is bought twice.
func TestUploadDocument_CancelledMidUploadKeepsWhatItBought(t *testing.T) {
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emb := &inputCountingEmbedder{cancelAfter: 3, cancel: cancel}
	svc := guardService(store, emb)
	const chunks = 650
	res, err := svc.UploadDocument(ctx, "scope", "org-1", "sop.txt", manyChunkDocument(chunks))
	if err != nil {
		t.Fatalf("UploadDocument cut by the client: %v; want the stored progress", err)
	}
	if res.ID == "" || res.State != interfaces.KnowledgeDocumentPending || res.Embedded != 3*embedBatchSize || res.PendingChunks != chunks-3*embedBatchSize || res.NextCursor == nil {
		t.Fatalf("answer = %+v; want the 3 bought batches kept and the rest pending", res)
	}
	if total, _, _ := store.CountKnowledgeChunks(context.Background(), "org-1"); total != 0 {
		t.Errorf("corpus = %d chunks with the upload unfinished, want none", total)
	}

	// A fresh request resumes it — without the cursor, which a client cut off never received.
	res, err = svc.ContinueDocument(context.Background(), "scope", "org-1", res.ID, interfaces.KnowledgeChunkCursor{})
	if err != nil || res.State != interfaces.KnowledgeDocumentReady || res.PendingChunks != 0 || res.Embedded != chunks-3*embedBatchSize {
		t.Fatalf("continuation = %+v, %v; want the rest embedded and the document ready", res, err)
	}
	if got := emb.billed(); got != chunks {
		t.Errorf("embedded %d inputs, want each of the %d chunks once", got, chunks)
	}
}

// TestUploadDocument_PendingDocumentIsNeverSearched: neither the chunks still to embed nor the ones already embedded
// of an unfinished upload are search hits; the version it replaces answers alone until it is finished, and then
// not at all.
func TestUploadDocument_PendingDocumentIsNeverSearched(t *testing.T) {
	lowerReindexLimits(t, 100, 10000)
	lowerUploadBudget(t, 0)
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1")
	emb := &inputCountingEmbedder{}
	svc := guardService(store, emb)
	if err := svc.AddDocument(context.Background(), "scope", "org-1", "sop.txt", "the old version of the retry rules"); err != nil {
		t.Fatalf("AddDocument(old): %v", err)
	}
	oldDocs, _ := store.ListKnowledgeDocuments(context.Background(), "org-1", 0, 0)
	old := map[string]bool{}
	for _, d := range oldDocs {
		old[d.ID] = true
	}
	res, err := svc.UploadDocument(context.Background(), "scope", "org-1", "sop.txt", manyChunkDocument(250))
	if err != nil || res.NextCursor == nil || res.Embedded != 100 {
		t.Fatalf("UploadDocument = %+v, %v; want one page embedded and the rest pending", res, err)
	}
	hits, err := store.SearchKnowledge(context.Background(), "org-1", []float32{1, 0}, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.DocID == res.ID {
			t.Fatalf("a chunk of the unfinished upload was a search hit: %s", h.ID)
		}
	}
	if len(hits) != 1 || !old[hits[0].DocID] {
		t.Errorf("hits = %+v; want the old version's one chunk", hits)
	}
	text, err := svc.Search(context.Background(), "scope", "org-1", "retry rules")
	if err != nil || !strings.Contains(text, "old version") || strings.Contains(text, "Paragraph") {
		t.Errorf("Search = %q, %v; want the old version only", text, err)
	}

	lowerUploadBudget(t, time.Hour)
	if res, err = svc.ContinueDocument(context.Background(), "scope", "org-1", res.ID, *res.NextCursor); err != nil || res.State != interfaces.KnowledgeDocumentReady {
		t.Fatalf("ContinueDocument = %+v, %v", res, err)
	}
	hits, _ = store.SearchKnowledge(context.Background(), "org-1", []float32{1, 0}, 1000, "")
	if len(hits) != 250 {
		t.Errorf("hits after the finish = %d, want the new version's 250 chunks alone", len(hits))
	}
}

// TestUploadDocument_ReuploadSupersedesAPendingUpload: a re-upload of the same name with other text replaces an
// unfinished upload at once (it never served), keeping the ready version until the new one is finished; the same
// text again resumes the unfinished upload instead of buying its embedded chunks twice.
func TestUploadDocument_ReuploadSupersedesAPendingUpload(t *testing.T) {
	lowerReindexLimits(t, 100, 10000)
	lowerUploadBudget(t, 0)
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1")
	emb := &inputCountingEmbedder{}
	svc := guardService(store, emb)
	ctx := context.Background()
	if err := svc.AddDocument(ctx, "scope", "org-1", "sop.txt", "ready version"); err != nil {
		t.Fatal(err)
	}
	first, err := svc.UploadDocument(ctx, "scope", "org-1", "sop.txt", manyChunkDocument(300))
	if err != nil || first.NextCursor == nil {
		t.Fatalf("first = %+v, %v", first, err)
	}
	billed := emb.billed()

	again, err := svc.UploadDocument(ctx, "scope", "org-1", "sop.txt", manyChunkDocument(300))
	if err != nil || again.ID != first.ID || !again.Resumed || again.Embedded != 100 || again.PendingChunks != 100 {
		t.Fatalf("the same text again = %+v, %v; want the pending upload resumed (one more page)", again, err)
	}
	if got := emb.billed() - billed; got != 100 {
		t.Errorf("resuming bought %d inputs, want the one page not yet embedded", got)
	}

	other, err := svc.UploadDocument(ctx, "scope", "org-1", "sop.txt", manyChunkDocument(150)+"\n\nA changed ending.")
	if err != nil || other.ID == first.ID || other.Resumed {
		t.Fatalf("other text = %+v, %v; want a new upload", other, err)
	}
	if _, err := store.GetKnowledgeDocument(ctx, "org-1", first.ID); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("the superseded pending upload: %v, want it gone", err)
	}
	docs, _ := store.ListKnowledgeDocuments(ctx, "org-1", 0, 0)
	states := map[string]int{}
	for _, d := range docs {
		states[d.State]++
	}
	if len(docs) != 2 || states[interfaces.KnowledgeDocumentReady] != 1 || states[interfaces.KnowledgeDocumentPending] != 1 {
		t.Errorf("documents = %d (%v); want the ready version and the new pending upload", len(docs), states)
	}
}

// TestContinueDocument_RefusesAnotherModelAndUnknownDocuments: a continuation must embed with the model the upload
// started with (a document is never mixed), and a document that does not exist (or is another org's) is not found.
func TestContinueDocument_RefusesAnotherModelAndUnknownDocuments(t *testing.T) {
	lowerReindexLimits(t, 100, 10000)
	lowerUploadBudget(t, 0)
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1", "org-2")
	ctx := context.Background()
	a := guardService(store, &namedEmbedder{identity: "A", vec: []float32{1, 0}})
	res, err := a.UploadDocument(ctx, "scope", "org-1", "sop.txt", manyChunkDocument(250))
	if err != nil || res.NextCursor == nil {
		t.Fatalf("UploadDocument = %+v, %v", res, err)
	}
	b := guardService(store, &namedEmbedder{identity: "B", vec: []float32{1, 0}})
	if _, err := b.ContinueDocument(ctx, "scope", "org-1", res.ID, *res.NextCursor); !errors.Is(err, ErrEmbeddingModelChanged) {
		t.Errorf("continuation by another model: %v, want ErrEmbeddingModelChanged", err)
	}
	if _, err := a.ContinueDocument(ctx, "scope", "org-2", res.ID, interfaces.KnowledgeChunkCursor{}); !errors.Is(err, ErrKnowledgeDocumentNotFound) {
		t.Errorf("continuation in another org: %v, want ErrKnowledgeDocumentNotFound", err)
	}
	if _, err := a.ContinueDocument(ctx, "scope", "org-1", "no-such-doc", interfaces.KnowledgeChunkCursor{}); !errors.Is(err, ErrKnowledgeDocumentNotFound) {
		t.Errorf("continuation of a missing document: %v, want ErrKnowledgeDocumentNotFound", err)
	}
	// A finished document continues to nothing.
	lowerUploadBudget(t, time.Hour)
	if res, err = a.ContinueDocument(ctx, "scope", "org-1", res.ID, interfaces.KnowledgeChunkCursor{}); err != nil || res.State != interfaces.KnowledgeDocumentReady {
		t.Fatalf("finishing continuation = %+v, %v", res, err)
	}
	if again, err := a.ContinueDocument(ctx, "scope", "org-1", res.ID, interfaces.KnowledgeChunkCursor{}); err != nil || again.Embedded != 0 || again.NextCursor != nil || again.State != interfaces.KnowledgeDocumentReady {
		t.Errorf("continuing a ready document = %+v, %v; want nothing to do", again, err)
	}
}
