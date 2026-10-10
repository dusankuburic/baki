package rag

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// waitFor polls cond up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReindexInBackground_RunsToCompletion: the background re-embed answers the job name at once and converts
// the whole corpus — across several ReindexOrg requests when it is larger than one — under the caller's
// identity, as the forced synchronous path would.
func TestReindexInBackground_RunsToCompletion(t *testing.T) {
	old := maxReindexChunks
	maxReindexChunks = 4
	t.Cleanup(func() { maxReindexChunks = old })
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	seedManyChunks(store, 2, 10) // width 3, stranded at the fake's width 1
	emb := &namedEmbedder{identity: "A", vec: []float32{1}}
	svc := guardService(store, emb)
	t.Cleanup(svc.StopJobs)

	job, err := svc.ReindexInBackground(context.Background(), "u-admin", "org-1", false)
	if err != nil || job != "knowledge_reembed" {
		t.Fatalf("ReindexInBackground = %q, %v; want the job name", job, err)
	}
	waitFor(t, "every chunk converted", func() bool {
		counts, err := store.CountKnowledgeChunksByIdentity(context.Background(), "org-1", "A")
		return err == nil && counts.Total == 10 && counts.Current == 10 && counts.VectorIndexed == 10
	})
	waitFor(t, "the job to end", func() bool {
		last, _ := store.JobLastSuccess(context.Background(), "knowledge_reembed")
		return !last.IsZero()
	})
}

// TestReindexInBackground_BusyWhileRunning: a second start while the job runs (here, or on another replica) is
// ErrKnowledgeBusy; once it ends a new run starts.
func TestReindexInBackground_BusyWhileRunning(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	seedManyChunks(store, 1, 3)
	release := make(chan struct{})
	emb := &blockingEmbedder{release: release}
	svc := guardService(store, emb)
	t.Cleanup(svc.StopJobs)
	if _, err := svc.ReindexInBackground(context.Background(), "u-admin", "org-1", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run to reach the embedder", func() bool { return emb.started() })
	if _, err := svc.ReindexInBackground(context.Background(), "u-admin", "org-1", true); !errors.Is(err, ErrKnowledgeBusy) {
		t.Errorf("second start while running: %v, want ErrKnowledgeBusy", err)
	}
	close(release)
	waitFor(t, "the first run to end", func() bool {
		last, _ := store.JobLastSuccess(context.Background(), "knowledge_reembed")
		return !last.IsZero()
	})
}

// blockingEmbedder embeds width 1 after release is closed.
type blockingEmbedder struct {
	ai.Provider
	release <-chan struct{}
	mu      sync.Mutex
	seen    bool
}

func (b *blockingEmbedder) ID() string { return "blocking-embed" }
func (b *blockingEmbedder) started() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seen
}
func (b *blockingEmbedder) Embed(ctx context.Context, text []string) ([][]float32, error) {
	b.mu.Lock()
	b.seen = true
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{1}
	}
	return out, nil
}

// TestReindexInBackground_RefusesPersonalKey: the background run spends under a credential the admin is not
// present to vouch for, so it runs on the organisation's shared credential only; a member's own key is refused
// with ErrEmbeddingOrgCredentialRequired, and the synchronous path stays open to them.
func TestReindexInBackground_RefusesPersonalKey(t *testing.T) {
	store := &testutil.FakeBackend{}
	seedManyChunks(store, 1, 2)
	memberCalls, orgCalls := 0, 0
	personal := orgKeysFactory(map[string]string{"openai": "sk-member"}, map[string]string{}, &memberCalls, &orgCalls)
	svc := NewKnowledgeService(store, personal, stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("openai"))
	t.Cleanup(svc.StopJobs)
	if _, err := svc.ReindexInBackground(context.Background(), "u-member", "org-1", true); !errors.Is(err, ErrEmbeddingOrgCredentialRequired) {
		t.Errorf("background re-index on a personal key: %v, want ErrEmbeddingOrgCredentialRequired", err)
	}
	if last, _ := store.JobLastSuccess(context.Background(), "knowledge_reembed"); !last.IsZero() {
		t.Error("a refused start recorded a run")
	}

	shared := orgKeysFactory(map[string]string{}, map[string]string{"openai": "sk-org"}, &memberCalls, &orgCalls)
	svc = NewKnowledgeService(store, shared, stubSettings{&models.AppSettings{}}, WithEmbeddingProvider("openai"), WithEmbedder(nil))
	t.Cleanup(svc.StopJobs)
	// The start itself is accepted on the org's credential (the run then fails against the fake key's endpoint,
	// which is the provider's business, not the start's).
	if _, err := svc.ReindexInBackground(context.Background(), "u-member", "org-1", true); err != nil {
		t.Errorf("background re-index on the org's credential refused: %v", err)
	}
}

// The job is declared once, with a derived key, on demand.
func TestReembedJob_Declared(t *testing.T) {
	for _, j := range lifecycle.Jobs() {
		if j.Name == "knowledge_reembed" {
			if j.Legacy || j.Key != lifecycle.DerivedJobKey("knowledge_reembed") {
				t.Errorf("knowledge_reembed key = %#x legacy=%v, want the derived key", j.Key, j.Legacy)
			}
			return
		}
	}
	t.Fatal("knowledge_reembed is not declared")
}

var _ = interfaces.KnowledgeChunkCursor{}
