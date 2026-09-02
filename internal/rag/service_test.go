package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

func TestChunkText_Empty(t *testing.T) {
	if chunks := chunkText("", 10); len(chunks) != 0 {
		t.Errorf("empty input: want 0 chunks, got %d (%q)", len(chunks), chunks)
	}
}

func TestChunkText_ShorterThanSize(t *testing.T) {
	chunks := chunkText("hi", 10)
	if len(chunks) != 1 || chunks[0] != "hi" {
		t.Errorf("short input: want [\"hi\"], got %q", chunks)
	}
}

func TestChunkText_ExactSize(t *testing.T) {
	in := strings.Repeat("x", 10)
	chunks := chunkText(in, 10)
	if len(chunks) != 1 || chunks[0] != in {
		t.Errorf("exact size: want 1 chunk, got %d (%q)", len(chunks), chunks)
	}
}

func TestChunkText_RespectsParagraphBoundaries(t *testing.T) {
	in := "First paragraph.\n\nSecond paragraph.\n\nThird paragraph."
	chunks := chunkText(in, 1000)
	if len(chunks) != 1 {
		t.Errorf("all paragraphs fit: want 1 chunk, got %d", len(chunks))
	}
}

func TestChunkText_SplitsLongParagraphsAtSentences(t *testing.T) {
	sentences := make([]string, 20)
	for i := range sentences {
		sentences[i] = "This is sentence number " + string(rune('0'+i)) + "."
	}
	in := strings.Join(sentences, " ")
	chunks := chunkText(in, 100)
	if len(chunks) < 2 {
		t.Errorf("long input should split into multiple chunks, got %d", len(chunks))
	}
}

func TestChunkText_SplitsByRunesNotBytes(t *testing.T) {
	in := strings.Repeat("☃", 25)
	size := 10
	chunks := chunkText(in, size)
	if len(chunks) < 2 {
		t.Errorf("want multiple chunks for 25 snowmen with size 10, got %d", len(chunks))
	}
	// Strict size invariant (overlap carrying never appends past the limit —
	// when the next unit doesn't fit alongside the carry, the carry drops).
	for i, c := range chunks {
		if n := len([]rune(c)); n > size {
			t.Errorf("chunk %d exceeds size (%d): %d", i, size, n)
		}
	}
}

func TestChunkText_PacksMultiByteParagraphsByRunes(t *testing.T) {
	para := strings.Repeat("ж", 20)
	in := para + "\n\n" + para + "\n\n" + strings.Repeat("ж", 60)
	chunks := chunkText(in, 50)
	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
	// Two 20-rune paragraphs fit a 50-rune chunk together (20+2+20); counting
	// their 40-byte UTF-8 length instead would wrongly split them.
	if first := chunks[0]; first != para+"\n\n"+para {
		t.Errorf("want first chunk to pack both short paragraphs, got %q", first)
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > 50 {
			t.Errorf("chunk %d exceeds size in runes: %d", i, n)
		}
	}
}

func TestChunkText_NoContentLoss(t *testing.T) {
	in := "First paragraph with some content.\n\nSecond paragraph with more content.\n\nThird one here."
	chunks := chunkText(in, 50)
	allContent := strings.Join(chunks, " ")
	for _, word := range []string{"First", "paragraph", "Second", "Third"} {
		if !strings.Contains(allContent, word) {
			t.Errorf("content lost: %q not found in chunks", word)
		}
	}
}

// TestChunkText_SizeInvariantAllInputs pins the chunk-size contract across the
// shapes that historically broke it: a terminator-free long paragraph (no '.'),
// and a paragraph whose FIRST sentence is short but a LATER run is oversized —
// splitSentences used to return the oversized run whole and the caller appended
// it verbatim, blowing the limit (embedding providers cap per-item length).
func TestChunkText_SizeInvariantAllInputs(t *testing.T) {
	const size = 100
	longRun := strings.Repeat("w", 500) // no sentence terminator
	cases := []struct {
		name string
		in   string
	}{
		{"terminator-free paragraph", longRun},
		{"short sentence then oversized run", "Intro sentence. " + longRun},
		{"two oversized runs", strings.Repeat("a", 300) + ". " + strings.Repeat("b", 250)},
		{"oversized paragraph between normal ones", "Before.\n\n" + longRun + "\n\nAfter paragraph content here."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := chunkText(tc.in, size)
			if len(chunks) == 0 {
				t.Fatal("expected chunks, got none")
			}
			for i, c := range chunks {
				if n := len([]rune(c)); n > size {
					t.Errorf("chunk %d has %d runes, exceeds size %d", i, n, size)
				}
			}
			// No content loss: every input marker must appear in some chunk.
			for _, marker := range []string{"w", "Intro", "a", "b", "Before", "After"} {
				if strings.Contains(tc.in, marker) && !strings.Contains(strings.Join(chunks, "\n"), marker) {
					t.Errorf("marker %q lost", marker)
				}
			}
		})
	}
}

// TestChunkText_NoPureOverlapTailChunk: when the final flush leaves only the
// carried overlap tail (nothing new was appended after the last emit), that
// "chunk" duplicates the previous chunk's ending verbatim — pure retrieval
// noise. It must not be emitted.
func TestChunkText_NoPureOverlapTailChunk(t *testing.T) {
	const size = 100
	// Distinguishable paragraphs of exactly `size` runes: the first fills a
	// chunk, the second triggers flush, and the loop ends with current =
	// carry only.
	para1 := strings.Repeat("p", size)
	para2 := strings.Repeat("q", size)
	chunks := chunkText(para1+"\n\n"+para2, size)

	for i, c := range chunks {
		if len([]rune(c)) > size {
			t.Errorf("chunk %d exceeds size: %d runes", i, len([]rune(c)))
		}
	}
	for i := 1; i < len(chunks); i++ {
		if c := chunks[i]; strings.HasSuffix(chunks[i-1], c) && c != para2 {
			t.Errorf("chunk %d (%q…) is a pure tail duplicate of chunk %d", i, c[:min(20, len(c))], i-1)
		}
	}
	// Both paragraphs' content must survive.
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, strings.Repeat("p", 50)) || !strings.Contains(joined, strings.Repeat("q", 50)) {
		t.Errorf("paragraph content lost: %q", joined)
	}
}

func TestAddDocument_EmptyContent(t *testing.T) {
	svc := NewKnowledgeService(nil, nil, nil)
	err := svc.AddDocument(context.Background(), "scope", "org", "f.txt", "   \n\t ")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("want 'document is empty' error, got %v", err)
	}
}

func TestAddDocument_NoProvider(t *testing.T) {
	svc := NewKnowledgeService(nil, nil, nil)
	err := svc.AddDocument(context.Background(), "scope", "org", "f.txt", "real content")
	if err == nil || !strings.Contains(err.Error(), "embedding provider") {
		t.Errorf("want embedding-provider error, got %v", err)
	}
	// The sentinel lets the HTTP layer map this to a machine-readable code
	// (EMBEDDING_NOT_CONFIGURED) the frontend branches on.
	if !errors.Is(err, ErrEmbeddingNotConfigured) {
		t.Errorf("want errors.Is(err, ErrEmbeddingNotConfigured), got %v", err)
	}
}

func TestSearch_NoProvider(t *testing.T) {
	svc := NewKnowledgeService(nil, nil, nil)
	out, err := svc.Search(context.Background(), "scope", "org", "query")
	if err == nil {
		t.Errorf("want error when no provider configured, got nil (out=%q)", out)
	}
}

// stubSettings satisfies SettingsProvider with a fixed snapshot.
type stubSettings struct{ s *models.AppSettings }

func (st stubSettings) Get() *models.AppSettings { return st.s }

// newTestFactory builds a real ProviderFactory whose key lookup is a stub map,
// so embedder()'s construction path (and its fallback scan) runs without any
// network or keyring dependency. Construction never dials the provider.
func newTestFactory(keys map[string]bool) *ai.ProviderFactory {
	return ai.NewProviderFactory(func(_ string, provider string) (string, error) {
		if keys[provider] {
			return "test-key", nil
		}
		return "", nil
	}, nil, nil, nil)
}

func enabledProviders(ids ...string) map[string]models.AIProviderConfig {
	m := make(map[string]models.AIProviderConfig, len(ids))
	for _, id := range ids {
		m[id] = models.AIProviderConfig{Enabled: true}
	}
	return m
}

// TestEmbedder_ExplicitChatOnlyProvider_FailsFast locks the fail-fast path: a
// deployer who pointed EmbeddingProvider at a chat-only provider (claude) gets
// the machine-readable ErrEmbeddingUnavailable sentinel immediately — not a
// successfully-constructed provider that would blow up later at Embed time
// with an opaque wrapped 500 (the pre-fix behavior).
func TestEmbedder_ExplicitChatOnlyProvider_FailsFast(t *testing.T) {
	settings := &models.AppSettings{}
	settings.AI.EmbeddingProvider = "claude"
	settings.AI.Providers = enabledProviders("claude")
	svc := NewKnowledgeService(nil, newTestFactory(map[string]bool{"claude": true}), stubSettings{settings})

	_, err := svc.embedder("scope")
	if err == nil {
		t.Fatal("want error for chat-only embedding provider, got nil")
	}
	if !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Errorf("want errors.Is(err, ErrEmbeddingUnavailable), got %v", err)
	}
	if !strings.Contains(err.Error(), "does not support embeddings") {
		t.Errorf("want 'does not support embeddings' hint, got %v", err)
	}
}

// TestEmbedder_ClaudeOnlyNoFallback proves the capability filter: with only
// chat-only providers keyed+enabled, the fallback returns the sentinel instead
// of constructing a provider that can't embed (the pre-fix code returned the
// keyed Claude provider here).
func TestEmbedder_ClaudeOnlyNoFallback(t *testing.T) {
	settings := &models.AppSettings{} // EmbeddingProvider "" → default openai (no key)
	settings.AI.Providers = enabledProviders("claude")
	svc := NewKnowledgeService(nil, newTestFactory(map[string]bool{"claude": true}), stubSettings{settings})

	_, err := svc.embedder("scope")
	if !errors.Is(err, ErrEmbeddingUnavailable) {
		t.Errorf("want ErrEmbeddingUnavailable on Claude-only deploy, got %v", err)
	}
}

// TestEmbedder_FallbackPicksEmbeddingCapable_Deterministic proves both the
// capability filter and the determinism of the pick: with claude, gemini and
// glm all keyed+enabled and the (default) openai provider keyless, the
// fallback must always choose gemini (fixed priority: openai > gemini > glm >
// github-models) — never claude, and never randomly glm on another call.
func TestEmbedder_FallbackPicksEmbeddingCapable_Deterministic(t *testing.T) {
	settings := &models.AppSettings{}
	settings.AI.Providers = enabledProviders("claude", "gemini", "glm")
	svc := NewKnowledgeService(nil, newTestFactory(map[string]bool{"claude": true, "gemini": true, "glm": true}), stubSettings{settings})

	for i := 0; i < 25; i++ {
		p, err := svc.embedder("scope")
		if err != nil {
			t.Fatalf("iteration %d: want fallback provider, got error: %v", i, err)
		}
		if p.ID() != "gemini" {
			t.Fatalf("iteration %d: want fallback provider gemini, got %q", i, p.ID())
		}
	}
}

// TestEmbedder_DisabledFallbackIsSkipped proves the enabled filter: a keyed,
// embedding-capable provider that is disabled in settings is not chosen.
func TestEmbedder_DisabledFallbackIsSkipped(t *testing.T) {
	settings := &models.AppSettings{}
	settings.AI.Providers = map[string]models.AIProviderConfig{
		"gemini": {Enabled: false},
		"glm":    {Enabled: true},
	}
	// Both providers have keys; gemini is higher priority but disabled.
	svc := NewKnowledgeService(nil, newTestFactory(map[string]bool{"gemini": true, "glm": true}), stubSettings{settings})

	p, err := svc.embedder("scope")
	if err != nil {
		t.Fatalf("want glm fallback, got error: %v", err)
	}
	if p.ID() != "glm" {
		t.Errorf("want disabled gemini skipped → glm, got %q", p.ID())
	}
}

// embedCountingStub counts inputs per Embed call so batching tests can assert
// both the batch shape and the order of the concatenated results. Only Embed
// is reachable from embedBatched; the embedded interface keeps the stub tiny.
type embedCountingStub struct {
	ai.Provider
	calls  [][]string
	failOn int // 1-based call index that returns an error
}

func (e *embedCountingStub) Embed(_ context.Context, text []string) ([][]float32, error) {
	e.calls = append(e.calls, text)
	if e.failOn > 0 && len(e.calls) == e.failOn {
		return nil, errors.New("provider rejected batch")
	}
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{float32(len(e.calls)), float32(i)} // call#, input#
	}
	return out, nil
}

// TestEmbedBatched_ProviderSafeBatches: documents with more chunks than a
// provider's per-request input cap (OpenAI/Azure reject oversized batches)
// used to fail indexing wholesale — one Embed call with everything. Now the
// inputs go out in batches of embedBatchSize, results concatenated in order.
func TestEmbedBatched_ProviderSafeBatches(t *testing.T) {
	const n = 3*embedBatchSize + 50 // 350 → batches of 100, 100, 100, 50
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("item-%d", i)
	}

	stub := &embedCountingStub{}
	out, err := embedBatched(context.Background(), stub, items)
	if err != nil {
		t.Fatalf("embedBatched: %v", err)
	}

	wantBatches := (n + embedBatchSize - 1) / embedBatchSize
	if len(stub.calls) != wantBatches {
		t.Fatalf("want %d Embed calls, got %d", wantBatches, len(stub.calls))
	}
	for i, call := range stub.calls {
		want := embedBatchSize
		if i == wantBatches-1 {
			want = n - embedBatchSize*(wantBatches-1)
		}
		if len(call) != want {
			t.Errorf("call %d: want %d inputs, got %d", i, want, len(call))
		}
	}
	if len(out) != n {
		t.Fatalf("want %d concatenated results, got %d", n, len(out))
	}
	// Order preserved: result j carries its batch index and position.
	for j := range out {
		wantBatch := j/embedBatchSize + 1
		if int(out[j][0]) != wantBatch || int(out[j][1]) != j%embedBatchSize {
			t.Errorf("result %d out of order: %v", j, out[j])
		}
	}
}

func TestEmbedBatched_ErrorPropagates(t *testing.T) {
	stub := &embedCountingStub{failOn: 2}
	items := make([]string, 2*embedBatchSize)
	_, err := embedBatched(context.Background(), stub, items)
	if err == nil || !strings.Contains(err.Error(), "rejected batch") {
		t.Fatalf("want propagated error, got %v", err)
	}
}

func TestEmbedBatched_CountMismatchIsError(t *testing.T) {
	short := &shortEmbedStub{}
	if _, err := embedBatched(context.Background(), short, []string{"a", "b", "c"}); err == nil {
		t.Fatal("want count-mismatch error, got nil")
	}
}

// shortEmbedStub returns one embedding regardless of input count.
type shortEmbedStub struct{ ai.Provider }

func (s *shortEmbedStub) Embed(_ context.Context, text []string) ([][]float32, error) {
	return [][]float32{{1}}, nil
}

// mkChunk builds a knowledge chunk for assembly tests.
func mkChunk(docID, filename, content string) interfaces.KnowledgeChunk {
	return interfaces.KnowledgeChunk{ID: docID + "-" + content[:8], DocID: docID, Filename: filename, Content: content}
}

// TestAssembleGuidelines pins the assembled-context contract: grounding
// instruction header, per-chunk source attribution, and overlap de-duplication
// (chunk N+1 from the same document begins with the exact tail chunkText
// carried over — emitting both duplicated that passage).
func TestAssembleGuidelines(t *testing.T) {
	tail := strings.Repeat("t", 100)
	head := strings.Repeat("h", 100)
	chunks := []interfaces.KnowledgeChunk{
		mkChunk("doc1", "naming.md", head+tail),
		mkChunk("doc1", "naming.md", tail+strings.Repeat("n", 100)), // overlap-carry of doc1
		mkChunk("doc2", "retries.md", strings.Repeat("r", 100)),
		mkChunk("doc3", "", strings.Repeat("u", 100)), // unknown filename → bare bullet
	}
	out := assembleGuidelines(chunks)

	if !strings.Contains(out, "**Relevant Organizational Guidelines**") {
		t.Errorf("header missing: %q", out)
	}
	if !strings.Contains(out, "cite the source file") {
		t.Errorf("grounding instruction missing: %q", out)
	}
	if !strings.Contains(out, "- [naming.md]") || !strings.Contains(out, "- [retries.md]") {
		t.Errorf("source attribution missing: %q", out)
	}
	if !strings.Contains(out, "- "+strings.Repeat("u", 100)) {
		t.Errorf("unknown-filename chunk should render as bare bullet: %q", out)
	}
	// The carried overlap must appear at most once across the two doc1 chunks.
	if n := strings.Count(out, tail); n != 1 {
		t.Errorf("overlap tail appears %d times, want exactly 1 (de-duplicated): %q", n, out)
	}
}

func TestAssembleGuidelines_FullyDuplicatedChunkSkipped(t *testing.T) {
	dup := strings.Repeat("d", 200)
	chunks := []interfaces.KnowledgeChunk{
		mkChunk("doc1", "a.md", strings.Repeat("x", 300)+dup),
		mkChunk("doc1", "a.md", dup), // fully contained in the previous chunk
	}
	out := assembleGuidelines(chunks)
	if n := strings.Count(out, "- [a.md]"); n != 1 {
		t.Errorf("want exactly 1 bullet after full-duplicate skip, got %d: %q", n, out)
	}
}

func TestStripOverlapPrefix(t *testing.T) {
	tail := strings.Repeat("t", 80)
	fresh := strings.Repeat("f", 80)
	all := []interfaces.KnowledgeChunk{
		mkChunk("doc1", "a.md", strings.Repeat("x", 100)+tail),
		mkChunk("doc1", "a.md", tail+fresh),
		mkChunk("doc2", "b.md", tail+strings.Repeat("y", 100)), // same tail, OTHER doc
	}

	if got := stripOverlapPrefix(tail+fresh, "doc1", all); got != fresh {
		t.Errorf("same-doc overlap not stripped: %q", got)
	}
	// Different document: identical bytes must NOT be treated as carry-over.
	if got := stripOverlapPrefix(tail+strings.Repeat("y", 100), "doc2", all); got != tail+strings.Repeat("y", 100) {
		t.Errorf("cross-doc tail wrongly stripped: %q", got)
	}
	// Below the coincidence floor: no trim even on a same-doc match.
	short := strings.Repeat("s", 30)
	shortSet := []interfaces.KnowledgeChunk{
		mkChunk("d", "a.md", strings.Repeat("z", 40)+short),
		mkChunk("d", "a.md", short+"tail"),
	}
	if got := stripOverlapPrefix(short+"tail", "d", shortSet); got != short+"tail" {
		t.Errorf("below-floor overlap wrongly stripped: %q", got)
	}
}
