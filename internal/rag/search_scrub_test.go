package rag

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// recordingEmbedder keeps every input it was asked to embed.
type recordingEmbedder struct {
	ai.Provider
	inputs []string
}

func (r *recordingEmbedder) ID() string { return "recording-embed" }
func (r *recordingEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	r.inputs = append(r.inputs, text...)
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{1}
	}
	return out, nil
}

// TestSearch_ScrubsQueryBeforeEmbedding (AZSEC-1): the chat path hands
// Search the RAW user message while every chat provider call gets the
// scrubbed one — the embeddings request must not be the one place a pasted
// credential reaches a provider verbatim.
func TestSearch_ScrubsQueryBeforeEmbedding(t *testing.T) {
	// Assembled so no credential scanner matches the test source.
	secret := "sk_" + "live_" + strings.Repeat("abcdefgh", 3)
	emb := &recordingEmbedder{}
	// One indexed chunk: an org with nothing indexed skips the embedding.
	store := &testutil.FakeBackend{}
	store.SeedOrgs("org-1") // the knowledge base needs its organisation
	seedCorpus(store, []float32{1}, "retry policy: three attempts")
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(emb))

	if _, err := svc.Search(context.Background(), "scope", "org-1", "deploy with api_key="+secret+" and tell me the retry policy"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(emb.inputs) != 1 {
		t.Fatalf("embedded %d inputs, want 1", len(emb.inputs))
	}
	got := emb.inputs[0]
	if strings.Contains(got, secret) || strings.Contains(got, "sk_live") {
		t.Errorf("the embedding request carried the secret: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, "retry policy") {
		t.Errorf("query = %q, want the secret redacted and the rest of the question kept", got)
	}

	// Scrubbing precedes the rune cap: a secret straddling the cap cannot
	// be cut into an unrecognisable fragment.
	long := "api_key=" + secret + " " + strings.Repeat("x", maxQueryRunes)
	emb.inputs = nil
	if _, err := svc.Search(context.Background(), "scope", "org-1", long); err != nil {
		t.Fatalf("Search (long): %v", err)
	}
	if got := emb.inputs[0]; strings.Contains(got, secret) || len([]rune(got)) > maxQueryRunes {
		t.Errorf("long query: secret leaked or cap exceeded (%d runes)", len([]rune(got)))
	}
}
