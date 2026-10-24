package rag

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	tu "pad-analyzer/internal/testutil"
)

// contentRecordingStore keeps every chunk a re-index hands to UpdateKnowledgeChunkEmbeddings: the text the service
// asks the store to keep, whatever the store's update statement does with it.
type contentRecordingStore struct {
	*tu.FakeBackend
	updated []interfaces.KnowledgeChunk
}

func (s *contentRecordingStore) UpdateKnowledgeChunkEmbeddings(ctx context.Context, userID string, chunks []interfaces.KnowledgeChunk) error {
	s.updated = append(s.updated, chunks...)
	return s.FakeBackend.UpdateKnowledgeChunkEmbeddings(ctx, userID, chunks)
}

// TestReindexOrg_LegacyPairCutThroughASecretIsMaskedWithItsNeighbour: two chunks stored raw before uploads were
// masked, cut by chunkText so the carried overlap begins inside a connection-string password (the key in the first
// chunk, the value carried into the second's head). Scrubbed on its own the second chunk has no key to match, so a
// re-index must read it beside its predecessor: no fragment of the value reaches the embedding provider or the
// store, both chunks count as masked, and the pair still shares an exact overlap so stripOverlapPrefix de-duplicates
// it when both are retrieved — in whichever order the (doc_id, id) walk lists them.
func TestReindexOrg_LegacyPairCutThroughASecretIsMaskedWithItsNeighbour(t *testing.T) {
	secret := testSecret()
	fragment := secret[len(secret)/2:]
	_, raw, at := secretStraddlingOverlap(t)
	pred, succ := raw[at], raw[at+1]
	if !strings.Contains(succ, fragment) || strings.Contains(succ, "Password="+secret) {
		t.Fatalf("fixture rot: the successor must carry the value without its key: %q", carriedHead(succ))
	}
	for name, order := range map[string][]string{"predecessor first": {pred, succ}, "successor first": {succ, pred}} {
		t.Run(name, func(t *testing.T) {
			store := &contentRecordingStore{FakeBackend: &tu.FakeBackend{EmbeddingDim: 1}}
			seedCorpus(store.FakeBackend, []float32{1}, order...) // legacy: no identity
			emb := &recordingEmbedder{}
			svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))
			before := scrubbedChunks("reindex")

			res, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
			if err != nil {
				t.Fatalf("ReindexOrg: %v", err)
			}
			if res.Chunks != 2 {
				t.Fatalf("re-indexed %d chunks, want 2", res.Chunks)
			}
			for _, in := range emb.inputs {
				if strings.Contains(in, fragment) {
					t.Errorf("re-index embed request carried a fragment of the secret: %q", in)
				}
			}
			if len(store.updated) != 2 {
				t.Fatalf("the store was handed %d chunks, want 2", len(store.updated))
			}
			trimmed := 0
			for _, c := range store.updated {
				if strings.Contains(c.Content, fragment) {
					t.Errorf("the store was handed a fragment of the secret: %q", c.Content)
				}
				if !strings.Contains(c.Content, "Password=[REDACTED]") {
					t.Errorf("the store was handed a chunk without the mask: %q", c.Content)
				}
				if stripOverlapPrefix(c.Content, c.DocID, store.updated) != c.Content {
					trimmed++
				}
			}
			if trimmed != 1 {
				t.Errorf("chunks stripOverlapPrefix trims when the pair is retrieved together: %d, want 1 (the successor still begins with the predecessor's exact tail)", trimmed)
			}
			if d := scrubbedChunks("reindex") - before; d != 2 {
				t.Errorf(`pad_rag_scrubbed_chunks_total{source="reindex"} rose by %v, want 2`, d)
			}
		})
	}
}

// legacyPair builds a predecessor and the successor chunkText would cut after it: the successor begins with the
// predecessor's exact carried tail and continues with rest.
func legacyPair(t *testing.T, pred, rest string) []interfaces.KnowledgeChunk {
	t.Helper()
	tail, ok := tailOverlap(pred)
	if !ok {
		t.Fatalf("fixture rot: the predecessor must be longer than the carried overlap (%d runes)", chunkOverlapRunes)
	}
	return []interfaces.KnowledgeChunk{{ID: "a", DocID: "d", Content: pred}, {ID: "b", DocID: "d", Content: tail + rest}}
}

// pageTexts is reindexTexts over a page that holds its documents whole.
func pageTexts(t *testing.T, page []interfaces.KnowledgeChunk) ([]string, int) {
	t.Helper()
	texts, masked, err := reindexTexts(page, asIs)
	if err != nil {
		t.Fatalf("reindexTexts: %v", err)
	}
	return texts, masked
}

// tokenRuns reports whether s holds any 8-character stretch of token.
func tokenRuns(s, token string) bool {
	for i := 0; i+8 <= len(token); i++ {
		if strings.Contains(s, token[i:i+8]) {
			return true
		}
	}
	return false
}

// TestReindexTexts_CleanSuccessorOfAMaskedPredecessorIsEmbeddedAsStored: a secret in the predecessor outside the
// carried tail leaves the tail as it is once masked, so the successor is scrubbed on its own and, holding nothing,
// goes as stored; only the predecessor counts.
func TestReindexTexts_CleanSuccessorOfAMaskedPredecessorIsEmbeddedAsStored(t *testing.T) {
	secret := testSecret()
	pair := legacyPair(t, "Connect with Password="+secret+"; then retry. "+strings.Repeat("Guideline text. ", 12), " More guidance follows.")
	texts, masked := pageTexts(t, pair)
	if strings.Contains(texts[0], secret) || !strings.Contains(texts[0], "Password=[REDACTED]") {
		t.Errorf("predecessor text = %q, want the password masked", texts[0])
	}
	if texts[1] != pair[1].Content {
		t.Errorf("successor text = %q, want it as stored (%q)", texts[1], pair[1].Content)
	}
	if masked != 1 {
		t.Errorf("masked = %d, want 1", masked)
	}
}

// TestReindexTexts_ValueRunningPastTheCarriedHeadIsMaskedWhole pins WHY the pair is scrubbed as one stretch rather
// than the chunk alone behind the predecessor's masked tail: a bearer token longer than the carried overlap, its
// key in the predecessor, runs through the successor's head into its own text, where no pattern can match the rest
// of it on its own. Scrubbed as the stretch of the document, the token is matched whole; the successor then begins
// with the masked predecessor's exact tail.
func TestReindexTexts_ValueRunningPastTheCarriedHeadIsMaskedWhole(t *testing.T) {
	token := strings.Repeat("eyJhbGciOi", 22) // 220 base64url characters, as a JWT
	pred := strings.Repeat("Guideline text. ", 56) + "Call it with Authorization: Bearer " + token[:160]
	pair := legacyPair(t, pred, token[160:]+" and retry on 401.")
	if !tokenRuns(pair[1].Content[chunkOverlapRunes:], token) {
		t.Fatal("fixture rot: the token must run past the successor's carried head")
	}
	texts, masked := pageTexts(t, pair)
	for i, text := range texts {
		if tokenRuns(text, token) {
			t.Errorf("text %d carries a stretch of the token: %q", i, text)
		}
	}
	// The bearer class masks the scheme word with the token: "Authorization: [REDACTED]".
	if !strings.HasSuffix(texts[1], "Authorization: [REDACTED] and retry on 401.") {
		t.Errorf("successor text = %q, want the masked token and its own text", texts[1])
	}
	if tail, _ := tailOverlap(texts[0]); !strings.HasPrefix(texts[1], tail) {
		t.Errorf("successor text does not begin with the masked predecessor's tail:\n%q\n%q", texts[1], tail)
	}
	if masked != 2 {
		t.Errorf("masked = %d, want 2", masked)
	}
}

// TestReindexTexts_SecretCutAtTheChunkEndStillRejoins: a second secret cut at the predecessor's far end — the key
// and three characters of the value in the predecessor, the rest in the successor — is left as a stub by the
// predecessor's own scrub and matched whole in the stretch's, so the two scrubs disagree and the stretch cannot be
// cut; the successor is then scrubbed behind the masked predecessor's tail, which rejoins that secret while the one
// the carried head begins with stays masked.
func TestReindexTexts_SecretCutAtTheChunkEndStillRejoins(t *testing.T) {
	secret := testSecret()
	fragment := secret[len(secret)/2:]
	tail := "word=" + secret + ";Encrypt=true. " + strings.Repeat("Guideline ", 9) + "Then use Password=hun"
	if n := len([]rune(tail)); n != chunkOverlapRunes {
		t.Fatalf("fixture rot: the tail is %d runes, want %d", n, chunkOverlapRunes)
	}
	pair := legacyPair(t, strings.Repeat("Guideline text. ", 50)+"Set Pass"+tail, "ter2xQ9longvalue; then retry.")
	texts, masked := pageTexts(t, pair)
	for i, text := range texts {
		if strings.Contains(text, fragment) {
			t.Errorf("text %d carries a fragment of the secret: %q", i, text)
		}
	}
	// The head is the masked predecessor's last chunkOverlapRunes runes, which begin inside the first key
	// ("ssword=[REDACTED]"); the second secret is matched whole behind it.
	if strings.Count(texts[1], "[REDACTED]") != 2 || !strings.HasSuffix(texts[1], "Then use Password=[REDACTED]; then retry.") {
		t.Errorf("successor text = %q, want both secrets masked", texts[1])
	}
	if masked != 2 {
		t.Errorf("masked = %d, want 2", masked)
	}
}

// TestReindexOrg_PredecessorOnThePreviousPage: the predecessor of the first chunk on a page was the last chunk of
// the page before — the walk hands pages of reindexPageSize in (doc_id, id) order, so a document's chunks split
// across pages and the predecessor must be carried from one page to the next within a request.
func TestReindexOrg_PredecessorOnThePreviousPage(t *testing.T) {
	secret := testSecret()
	fragment := secret[len(secret)/2:]
	_, raw, at := secretStraddlingOverlap(t)
	store := &tu.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1")
	ctx := context.Background()
	_ = store.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: "doc-1", OrgID: "org-1", Filename: "runbook.md"})
	chunks := make([]interfaces.KnowledgeChunk, 0, reindexPageSize+1)
	for i := range reindexPageSize - 1 {
		chunks = append(chunks, interfaces.KnowledgeChunk{ID: fmt.Sprintf("chunk-%06d", i), DocID: "doc-1", Content: fmt.Sprintf("guideline number %d", i), Embedding: []float32{1}})
	}
	// Ids are zero-padded, so the predecessor closes the first page and the successor opens the second.
	chunks = append(chunks,
		interfaces.KnowledgeChunk{ID: fmt.Sprintf("chunk-%06d", reindexPageSize-1), DocID: "doc-1", Content: raw[at], Embedding: []float32{1}},
		interfaces.KnowledgeChunk{ID: fmt.Sprintf("chunk-%06d", reindexPageSize), DocID: "doc-1", Content: raw[at+1], Embedding: []float32{1}},
	)
	if err := store.SaveKnowledgeChunks(ctx, "", chunks); err != nil {
		t.Fatal(err)
	}
	emb := &recordingEmbedder{}
	svc := guardService(store, emb)

	res, err := svc.ReindexOrg(ctx, "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
	if err != nil {
		t.Fatalf("ReindexOrg: %v", err)
	}
	if res.Chunks != reindexPageSize+1 {
		t.Fatalf("re-indexed %d chunks, want %d", res.Chunks, reindexPageSize+1)
	}
	if len(emb.inputs) != reindexPageSize+1 {
		t.Fatalf("embedded %d inputs, want %d", len(emb.inputs), reindexPageSize+1)
	}
	for _, in := range emb.inputs {
		if strings.Contains(in, fragment) {
			t.Errorf("re-index embed request carried a fragment of the secret across the page boundary: %q", in)
		}
	}
}

// asIs is the whole-document lookup of a page that holds every chunk of its documents.
func asIs(doc []interfaces.KnowledgeChunk) ([]interfaces.KnowledgeChunk, error) { return doc, nil }

// fragmentFree fails the test when any embed input carries a fragment of the secret or when no input begins with
// the masked predecessor's exact tail (the overlap stripOverlapPrefix de-duplicates).
func fragmentFree(t *testing.T, inputs []string, fragment, predMasked string) {
	t.Helper()
	for _, in := range inputs {
		if strings.Contains(in, fragment) {
			t.Errorf("embed request carried a fragment of the secret: %q", in)
		}
	}
	tail, _ := tailOverlap(predMasked)
	for _, in := range inputs {
		if strings.HasPrefix(in, tail) {
			return
		}
	}
	t.Errorf("no embed input begins with the masked predecessor's tail %q:\n%q", tail, inputs)
}

// TestReindexOrg_DocumentReadWholeWhenThePageHoldsPartOfIt: a chunk is scrubbed beside its predecessor, which the
// (doc_id, id) walk does not promise to hand over with it — chunk ids are uuids, so the predecessor sorts after the
// successor half the time. When the page holds fewer chunks of a document than the document has, the document is
// read whole from the store: the predecessor on the NEXT page (a chunk cut that is also a page cut), before the
// cursor of a resumed request (converted by the request before), or skipped by an unforced walk as already under
// the caller's identity. In every case no fragment of the value reaches the embedding provider and the pair keeps
// its exact overlap.
func TestReindexOrg_DocumentReadWholeWhenThePageHoldsPartOfIt(t *testing.T) {
	secret := testSecret()
	fragment := secret[len(secret)/2:]
	_, raw, at := secretStraddlingOverlap(t)
	pred, succ := raw[at], raw[at+1]
	predMasked := scrubText(pred)
	seed := func(store *tu.FakeBackend, chunks ...interfaces.KnowledgeChunk) {
		t.Helper()
		store.SeedOrgs("org-1")
		ctx := context.Background()
		_ = store.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: "doc-1", OrgID: "org-1", Filename: "runbook.md"})
		for i := range chunks {
			chunks[i].DocID = "doc-1"
			if chunks[i].Embedding == nil {
				chunks[i].Embedding = []float32{1}
			}
		}
		if err := store.SaveKnowledgeChunks(ctx, "", chunks); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("predecessor on the next page", func(t *testing.T) {
		lowerReindexLimits(t, 2, 10000)
		store := &tu.FakeBackend{EmbeddingDim: 1}
		// Page 1 is a and b (the successor), page 2 is c (the predecessor).
		seed(store, interfaces.KnowledgeChunk{ID: "a", Content: "guideline one"}, interfaces.KnowledgeChunk{ID: "b", Content: succ}, interfaces.KnowledgeChunk{ID: "c", Content: pred})
		emb := &recordingEmbedder{}
		res, err := guardService(store, emb).ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
		if err != nil || res.Chunks != 3 {
			t.Fatalf("ReindexOrg = %+v, %v; want 3 chunks", res, err)
		}
		fragmentFree(t, emb.inputs, fragment, predMasked)
	})

	t.Run("predecessor before the cursor of a resumed request", func(t *testing.T) {
		lowerReindexLimits(t, 2, 2)
		store := &tu.FakeBackend{EmbeddingDim: 1}
		// The first request converts a (the predecessor) and b and stops at the ceiling; the resumed one gets c.
		seed(store, interfaces.KnowledgeChunk{ID: "a", Content: pred}, interfaces.KnowledgeChunk{ID: "b", Content: "guideline one"}, interfaces.KnowledgeChunk{ID: "c", Content: succ})
		svc := guardService(store, &recordingEmbedder{})
		first, err := svc.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{})
		if err != nil || first.NextCursor == nil || first.NextCursor.ID != "b" {
			t.Fatalf("first request = %+v, %v; want a cursor at b", first, err)
		}
		emb := &recordingEmbedder{}
		rest, err := guardService(store, emb).ReindexOrg(context.Background(), "scope", "org-1", true, *first.NextCursor)
		if err != nil || rest.Chunks != 1 {
			t.Fatalf("resumed request = %+v, %v; want the one chunk left", rest, err)
		}
		fragmentFree(t, emb.inputs, fragment, predMasked)
	})

	t.Run("predecessor skipped by an unforced walk", func(t *testing.T) {
		store := &tu.FakeBackend{EmbeddingDim: 1}
		// a is under the caller's identity and in the index; b was stored at another width, outside it, so the
		// unforced walk (the caller's own rows outside the index) lists b alone and a is read for the scrub only.
		seed(store, interfaces.KnowledgeChunk{ID: "a", Content: pred, EmbeddingIdentity: "model-x"}, interfaces.KnowledgeChunk{ID: "b", Content: succ, EmbeddingIdentity: "model-x", Embedding: []float32{1, 0}})
		emb := &namedEmbedder{identity: "model-x", vec: []float32{1}}
		res, err := guardService(store, emb).ReindexOrg(context.Background(), "scope", "org-1", false, interfaces.KnowledgeChunkCursor{})
		if err != nil || res.Chunks != 1 {
			t.Fatalf("ReindexOrg = %+v, %v; want the one legacy chunk", res, err)
		}
		fragmentFree(t, emb.inputs, fragment, predMasked)
	})
}
