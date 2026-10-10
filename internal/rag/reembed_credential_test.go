package rag

import (
	"context"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// credentialEmbedder is the reembed-stub provider: a width-1 embedder of identity "A" that remembers the key
// it was built with and counts the embeds made under every key, so a test can tell whose credential a page ran on.
type credentialEmbedder struct {
	ai.Provider
	key string
	rec *credentialEmbeds
}

type credentialEmbeds struct {
	mu      sync.Mutex
	byKey   map[string]int
	onFirst func()
}

func (e *credentialEmbedder) ID() string                { return "reembed-stub" }
func (e *credentialEmbedder) EmbeddingIdentity() string { return "A" }
func (e *credentialEmbedder) Embed(_ context.Context, text []string) ([][]float32, error) {
	e.rec.mu.Lock()
	if e.rec.byKey == nil {
		e.rec.byKey = map[string]int{}
	}
	first := len(e.rec.byKey) == 0
	e.rec.byKey[e.key]++
	hook := e.rec.onFirst
	e.rec.mu.Unlock()
	if first && hook != nil {
		hook()
	}
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{1}
	}
	return out, nil
}

func (r *credentialEmbeds) embeds(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byKey[key]
}

var registerCredentialEmbedderOnce sync.Once

// credentialKeys is a member + org key store whose org key a test can remove mid-run.
type credentialKeys struct {
	mu     sync.Mutex
	member string
	org    string
}

func (k *credentialKeys) factory() *ai.ProviderFactory {
	f := ai.NewProviderFactory(func(_, _ string) (string, error) {
		k.mu.Lock()
		defer k.mu.Unlock()
		return k.member, nil
	}, nil, nil, nil)
	f.SetOrgKeyLookup(func(_, _ string) (string, error) {
		k.mu.Lock()
		defer k.mu.Unlock()
		return k.org, nil
	})
	return f
}

// TestReindexInBackground_StopsWhenTheOrgCredentialGoes (Phase 4b review T1): every page of a background run
// re-resolves the embedder, so the "runs on the org's credential" rule is checked per page, not only at the start.
// When the organisation's shared key is removed mid-run, the next page is refused with
// ErrEmbeddingOrgCredentialRequired and the run fails; the starting admin's personal key is never spent.
func TestReindexInBackground_StopsWhenTheOrgCredentialGoes(t *testing.T) {
	old := maxReindexChunks
	maxReindexChunks = 4
	t.Cleanup(func() { maxReindexChunks = old })
	rec := &credentialEmbeds{}
	registerCredentialEmbedderOnce.Do(func() {
		ai.RegisterProvider("reembed-stub", func(key string) ai.Provider {
			return &credentialEmbedder{Provider: &testutil.FakeProvider{}, key: key, rec: rec}
		})
	})
	keys := &credentialKeys{member: "sk-member", org: "sk-org"}
	// The first page's embed is the moment the org credential disappears.
	rec.onFirst = func() {
		keys.mu.Lock()
		keys.org = ""
		keys.mu.Unlock()
	}

	store := &testutil.FakeBackend{EmbeddingDim: 1}
	seedManyChunks(store, 2, 10) // width 3, stranded at the fake's width 1: three pages of four
	svc := NewKnowledgeService(store, keys.factory(), stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("reembed-stub"))
	t.Cleanup(svc.StopJobs)

	if _, err := svc.ReindexInBackground(context.Background(), "u-admin", "org-1", true); err != nil {
		t.Fatalf("start on the org's credential: %v", err)
	}
	waitFor(t, "the run to end", func() bool {
		run, err := store.GetJobRun(context.Background(), "knowledge_reembed")
		return err == nil && run != nil && (!run.LastErrorAt.IsZero() || !run.LastSuccessAt.IsZero())
	})
	run, _ := store.GetJobRun(context.Background(), "knowledge_reembed")
	if !run.LastSuccessAt.IsZero() || !strings.Contains(run.LastError, ErrEmbeddingOrgCredentialRequired.Error()) {
		t.Errorf("the run ended %+v, want a failed run naming ErrEmbeddingOrgCredentialRequired", run)
	}
	if n := rec.embeds("sk-member"); n != 0 {
		t.Errorf("the admin's personal key embedded %d batches after the org credential went — a background run must never fall back to a personal key", n)
	}
	if n := rec.embeds("sk-org"); n != 1 {
		t.Errorf("the org credential embedded %d batches, want the first page only", n)
	}
	counts, _ := store.CountKnowledgeChunksByIdentity(context.Background(), "org-1", "A")
	if counts.Current != 4 {
		t.Errorf("converted chunks = %d, want the first page (4) kept and nothing after it", counts.Current)
	}
}
