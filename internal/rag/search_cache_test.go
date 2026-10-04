package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// countStore counts CountKnowledgeChunks reads; onCount (when set) runs
// inside the read, before it answers.
type countStore struct {
	*testutil.FakeBackend
	counts  atomic.Int32
	onCount func()
}

func (c *countStore) CountKnowledgeChunks(ctx context.Context, orgID string) (int, int, error) {
	c.counts.Add(1)
	total, indexed, err := c.FakeBackend.CountKnowledgeChunks(ctx, orgID)
	if c.onCount != nil {
		c.onCount()
	}
	return total, indexed, err
}

// identityEmbedder embeds like stubEmbedder, names an embedding identity
// (so Search may cache its query vectors) and records every input.
type identityEmbedder struct {
	ai.Provider
	identity string
	mu       sync.Mutex
	inputs   []string
}

func (e *identityEmbedder) ID() string                { return "identity-embed" }
func (e *identityEmbedder) EmbeddingIdentity() string { return e.identity }
func (e *identityEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inputs = append(e.inputs, text...)
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{1}
	}
	return out, nil
}

func (e *identityEmbedder) embedded(text string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, in := range e.inputs {
		if in == text {
			n++
		}
	}
	return n
}

// An org with nothing indexed has nothing to retrieve: without the skip,
// every org-flow chat turn would pay a billed query embedding (and wait for
// it). The count behind the skip is cached, so turns in a row read it once.
func TestSearch_EmptyCorpusSkipsEmbedding(t *testing.T) {
	store := &countStore{FakeBackend: &testutil.FakeBackend{}}
	emb := &identityEmbedder{identity: "id-1"}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))

	for turn := 0; turn < 3; turn++ {
		got, err := svc.Search(context.Background(), "u1", "org-1", "what is the retry policy?")
		if err != nil || got != "" {
			t.Fatalf("turn %d: Search = %q, %v; want empty", turn, got, err)
		}
	}
	if n := len(emb.inputs); n != 0 {
		t.Errorf("embedded %d queries for an org with no chunks, want 0", n)
	}
	if n := store.counts.Load(); n != 1 {
		t.Errorf("counted the corpus %d times in 3 turns, want 1 (cached)", n)
	}
}

// An upload invalidates the org's cached count at once: the next turn
// searches the new document instead of waiting out the cache.
func TestSearch_UploadInvalidatesCorpusCount(t *testing.T) {
	store := &countStore{FakeBackend: &testutil.FakeBackend{}}
	emb := &identityEmbedder{identity: "id-1"}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))
	const q = "what is the retry policy?"

	if _, err := svc.Search(context.Background(), "u1", "org-1", q); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddDocument(context.Background(), "u1", "org-1", "policy.md", "Retry three times."); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}
	if _, err := svc.Search(context.Background(), "u1", "org-1", q); err != nil {
		t.Fatal(err)
	}
	if n := emb.embedded(q); n != 1 {
		t.Errorf("query embedded %d times after the upload, want 1", n)
	}

	// After the settle window the count is read again (and is non-zero).
	before := store.counts.Load()
	svc.caches().counts.Delete(context.Background(), "org-1")
	if svc.corpusEmpty(context.Background(), "org-1") {
		t.Error("an org with an indexed document reads as empty")
	}
	if store.counts.Load() != before+1 {
		t.Error("an expired entry must be counted again")
	}
}

// A count read while an upload lands (before its invalidation) must not
// overwrite that invalidation with the stale "empty".
func TestCorpusEmpty_StaleFillLosesToInvalidation(t *testing.T) {
	store := &countStore{FakeBackend: &testutil.FakeBackend{}}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(&identityEmbedder{}))
	store.onCount = func() { svc.InvalidateCorpus("org-1") } // the upload commits mid-read

	if !svc.corpusEmpty(context.Background(), "org-1") {
		t.Fatal("the read itself saw an empty corpus")
	}
	store.onCount = nil
	if svc.corpusEmpty(context.Background(), "org-1") {
		t.Error("the stale empty count replaced the upload's invalidation")
	}
}

// The query embedding is reused for the same query by the same embedder,
// caller and org — and never across callers, orgs or embedders. The cache
// key holds a hash of the query, never its text.
func TestSearch_QueryEmbeddingCached(t *testing.T) {
	store := &testutil.FakeBackend{}
	seedCorpus(store, []float32{1}, "retry policy: three attempts")
	emb := &identityEmbedder{identity: "openai|https://example.invalid|text-embedding-3-small|1536"}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))
	const q = "what is our retry policy for the SAP connector?"

	for i := 0; i < 3; i++ {
		if _, err := svc.Search(context.Background(), "u1", "org-1", q); err != nil {
			t.Fatal(err)
		}
	}
	if n := emb.embedded(q); n != 1 {
		t.Fatalf("same query embedded %d times, want 1", n)
	}
	if _, err := svc.Search(context.Background(), "u2", "org-1", q); err != nil {
		t.Fatal(err)
	}
	if n := emb.embedded(q); n != 2 {
		t.Errorf("another caller reused the first caller's embedding (embeds=%d, want 2)", n)
	}
	other := &identityEmbedder{identity: "openai|https://example.invalid|text-embedding-3-large|3072"}
	svc.embedOverride = other
	if _, err := svc.Search(context.Background(), "u1", "org-1", q); err != nil {
		t.Fatal(err)
	}
	if n := other.embedded(q); n != 1 {
		t.Errorf("another embedding model reused a cached vector (embeds=%d, want 1)", n)
	}

	// An embedder that names no identity is never cached.
	anon := &identityEmbedder{}
	svc.embedOverride = anon
	for i := 0; i < 2; i++ {
		if _, err := svc.Search(context.Background(), "u1", "org-1", q); err != nil {
			t.Fatal(err)
		}
	}
	if n := anon.embedded(q); n != 2 {
		t.Errorf("an anonymous embedder was cached (embeds=%d, want 2)", n)
	}

	if k := queryEmbeddingKey("id", "u1", "org-1", q); strings.Contains(k, "retry") || len(k) != 64 {
		t.Errorf("cache key %q is not a bare hash", k)
	}
}

// concurrencyEmbedder records the peak number of Embed calls in flight. Each
// call waits (briefly) for a peer so concurrent batches actually overlap.
// concurrent is what it answers to ai.EmbedsConcurrently.
type concurrencyEmbedder struct {
	ai.Provider
	inFlight, peak atomic.Int32
	calls          atomic.Int32
	failCall       int32
	concurrent     bool
}

func (c *concurrencyEmbedder) EmbedsConcurrently() bool { return c.concurrent }

func (c *concurrencyEmbedder) Embed(ctx context.Context, text []string) ([][]float32, error) {
	call := c.calls.Add(1)
	n := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if call == c.failCall {
		return nil, errors.New("provider rejected batch")
	}
	select {
	case <-time.After(30 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]float32, len(text))
	for i := range text {
		var idx int
		_, _ = fmt.Sscanf(text[i], "item-%d", &idx)
		out[i] = []float32{float32(idx)}
	}
	return out, nil
}

// Upload batches run embedConcurrency at a time — one at a time would make
// a 2500-chunk upload 25 sequential round trips — with the results still in
// item order.
func TestEmbedBatched_RunsBatchesConcurrently(t *testing.T) {
	const n = 10*embedBatchSize + 7
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("item-%d", i)
	}
	emb := &concurrencyEmbedder{concurrent: true}
	out, err := embedBatched(context.Background(), emb, items)
	if err != nil {
		t.Fatalf("embedBatched: %v", err)
	}
	if p := emb.peak.Load(); p < 2 || p > embedConcurrency {
		t.Errorf("peak in-flight batches = %d, want 2..%d (concurrent, bounded)", p, embedConcurrency)
	}
	if len(out) != n {
		t.Fatalf("got %d vectors for %d items", len(out), n)
	}
	for i := range out {
		if int(out[i][0]) != i {
			t.Fatalf("vector %d belongs to item %v: order lost", i, out[i][0])
		}
	}
}

// A provider that does not declare concurrent embedding (a WithEmbedder
// stand-in) is called one batch at a time, in order.
func TestEmbedBatched_SequentialUnlessProviderOptsIn(t *testing.T) {
	items := make([]string, 4*embedBatchSize)
	for i := range items {
		items[i] = fmt.Sprintf("item-%d", i)
	}
	emb := &concurrencyEmbedder{}
	out, err := embedBatched(context.Background(), emb, items)
	if err != nil || len(out) != len(items) {
		t.Fatalf("embedBatched = %d vectors, %v", len(out), err)
	}
	if p := emb.peak.Load(); p != 1 {
		t.Errorf("peak in-flight batches = %d for a provider that did not opt in, want 1", p)
	}
	failing := &concurrencyEmbedder{failCall: 2}
	if _, err := embedBatched(context.Background(), failing, items); err == nil {
		t.Fatal("want the second batch's error")
	}
	if c := failing.calls.Load(); c != 2 {
		t.Errorf("%d calls, want 2 (stop at the first failure)", c)
	}
}

// The first failing batch is the error, nothing partial is returned, and no
// further batches are launched after it.
func TestEmbedBatched_FailureStopsTheRest(t *testing.T) {
	items := make([]string, 20*embedBatchSize)
	for i := range items {
		items[i] = fmt.Sprintf("item-%d", i)
	}
	emb := &concurrencyEmbedder{failCall: 1, concurrent: true}
	out, err := embedBatched(context.Background(), emb, items)
	if err == nil || !strings.Contains(err.Error(), "rejected batch") || out != nil {
		t.Fatalf("embedBatched = %d vectors, %v; want the batch's error and nothing else", len(out), err)
	}
	if c := emb.calls.Load(); c > embedConcurrency+1 {
		t.Errorf("%d batches were sent after the first one failed", c-1)
	}
}
