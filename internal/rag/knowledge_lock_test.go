package rag

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/platform/lock"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// memLeaser is the deployment's cross-replica lock in memory: every KnowledgeService handed the same one
// competes for its keys the way replicas do on the database's advisory locks.
type memLeaser struct {
	mu    sync.Mutex
	held  map[int64]*memLease
	asked atomic.Int32
	keys  []int64 // every key a lease was granted on, in order
	err   error   // answers every TryLease while set
}

func newMemLeaser() *memLeaser { return &memLeaser{held: map[int64]*memLease{}} }

type memLease struct {
	l        *memLeaser
	key      int64
	lost     chan struct{}
	lostOnce sync.Once
	released atomic.Int32
}

func (l *memLease) Lost() <-chan struct{} { return l.lost }
func (l *memLease) lose()                 { l.lostOnce.Do(func() { close(l.lost) }) }
func (l *memLease) Release() {
	if l.released.Add(1) > 1 {
		return
	}
	l.l.mu.Lock()
	if l.l.held[l.key] == l {
		delete(l.l.held, l.key)
	}
	l.l.mu.Unlock()
}

func (m *memLeaser) TryLease(_ context.Context, key int64) (lock.Lease, bool, error) {
	m.asked.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, false, m.err
	}
	if _, taken := m.held[key]; taken {
		return nil, false, nil
	}
	l := &memLease{l: m, key: key, lost: make(chan struct{})}
	m.held[key] = l
	m.keys = append(m.keys, key)
	return l, true, nil
}

func (m *memLeaser) holding(key int64) *memLease {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held[key]
}

func (m *memLeaser) heldCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.held)
}

// replica is one KnowledgeService of the deployment: its own process-local lock, the shared store and leaser.
func replica(store *testutil.FakeBackend, leaser lock.Leaser, emb ai.Provider, opts ...Option) *KnowledgeService {
	return NewKnowledgeService(store, nil, nil, append([]Option{WithEmbedder(emb), WithJobLeaser(leaser)}, opts...)...)
}

// asyncUpload runs AddDocument in the background and returns its error channel.
func asyncUpload(ctx context.Context, svc *KnowledgeService, org, filename string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- svc.AddDocument(ctx, "scope", org, filename, "some policy text to index") }()
	return done
}

func asyncReindex(ctx context.Context, svc *KnowledgeService, org string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := svc.ReindexOrg(ctx, "scope", org, true, interfaces.KnowledgeChunkCursor{})
		done <- err
	}()
	return done
}

func awaitErr(t *testing.T, what string, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// erroringEmbedder fails every call with err.
type erroringEmbedder struct {
	ai.Provider
	err error
}

func (f *erroringEmbedder) ID() string { return "erroring-embed" }
func (f *erroringEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, f.err
}

// TestKnowledgeLock_UploadOnOneReplicaRefusesBothOnAnother is the survey's hazard: replica A passes the model
// check and embeds (seconds to minutes, billed) while replica B's upload or re-index would convert or extend the
// same corpus. The process-local lock cannot see the other replica; the lease does, and the answer is the
// existing busy (409).
func TestKnowledgeLock_UploadOnOneReplicaRefusesBothOnAnother(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1", "org-2") // the knowledge base needs its organisation
	leaser := newMemLeaser()
	release := make(chan struct{})
	blocked := &blockingEmbedder{release: release}
	a := replica(store, leaser, blocked)
	b := replica(store, leaser, &namedEmbedder{identity: "B", vec: []float32{1}})

	upload := asyncUpload(context.Background(), a, "org-1", "a.md")
	waitFor(t, "replica A to reach the embedder", blocked.started)

	if err := b.AddDocument(context.Background(), "scope", "org-1", "b.md", "another upload"); !errors.Is(err, ErrKnowledgeBusy) {
		t.Errorf("upload on replica B while A embeds: %v, want ErrKnowledgeBusy", err)
	}
	if _, err := b.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{}); !errors.Is(err, ErrKnowledgeBusy) {
		t.Errorf("re-index on replica B while A embeds: %v, want ErrKnowledgeBusy", err)
	}
	// Another org is unaffected.
	if err := b.AddDocument(context.Background(), "scope", "org-2", "c.md", "an upload of another org"); err != nil {
		t.Errorf("upload of another org on replica B: %v", err)
	}

	close(release)
	if err := awaitErr(t, "replica A's upload", upload); err != nil {
		t.Fatalf("replica A's upload: %v", err)
	}
	if err := b.AddDocument(context.Background(), "scope", "org-1", "b.md", "now that A is done"); err != nil {
		t.Errorf("upload on replica B after A finished: %v", err)
	}
}

// TestKnowledgeLock_ReindexOnOneReplicaRefusesUploadOnAnother: the re-index's whole page loop holds the lease.
func TestKnowledgeLock_ReindexOnOneReplicaRefusesUploadOnAnother(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	store.SeedOrgs("org-1", "org-2") // the knowledge base needs its organisation
	seedManyChunks(store, 1, 3)
	leaser := newMemLeaser()
	release := make(chan struct{})
	blocked := &blockingEmbedder{release: release}
	a := replica(store, leaser, blocked)
	b := replica(store, leaser, &namedEmbedder{identity: "B", vec: []float32{1}})

	reindex := asyncReindex(context.Background(), a, "org-1")
	waitFor(t, "replica A to reach the embedder", blocked.started)

	if err := b.AddDocument(context.Background(), "scope", "org-1", "b.md", "an upload mid re-index"); !errors.Is(err, ErrKnowledgeBusy) {
		t.Errorf("upload on replica B during A's re-index: %v, want ErrKnowledgeBusy", err)
	}
	if _, err := b.ReindexOrg(context.Background(), "scope", "org-1", true, interfaces.KnowledgeChunkCursor{}); !errors.Is(err, ErrKnowledgeBusy) {
		t.Errorf("second re-index on replica B: %v, want ErrKnowledgeBusy", err)
	}
	close(release)
	if err := awaitErr(t, "replica A's re-index", reindex); err != nil {
		t.Fatalf("replica A's re-index: %v", err)
	}
	if leaser.heldCount() != 0 {
		t.Errorf("%d lease(s) still held after the re-index", leaser.heldCount())
	}
}

// TestKnowledgeLock_LeaseReleasedOnEveryExit: success, a failed embed, a refused model switch and a cancelled
// context all give the lease back; a lease left held would lock the org out on every replica until the
// session died.
func TestKnowledgeLock_LeaseReleasedOnEveryExit(t *testing.T) {
	ctx := context.Background()
	t.Run("success", func(t *testing.T) {
		store := &testutil.FakeBackend{EmbeddingDim: 1}
		store.SeedOrgs("org-1", "org-2") // the knowledge base needs its organisation
		leaser := newMemLeaser()
		svc := replica(store, leaser, &namedEmbedder{identity: "A", vec: []float32{1}})
		if err := svc.AddDocument(ctx, "scope", "org-1", "a.md", "text"); err != nil {
			t.Fatal(err)
		}
		if leaser.asked.Load() == 0 {
			t.Fatal("the upload never asked for the lease")
		}
		if leaser.heldCount() != 0 {
			t.Error("lease still held after a successful upload")
		}
	})
	t.Run("embedding fails", func(t *testing.T) {
		store := &testutil.FakeBackend{EmbeddingDim: 1}
		store.SeedOrgs("org-1", "org-2") // the knowledge base needs its organisation
		leaser := newMemLeaser()
		svc := replica(store, leaser, &erroringEmbedder{err: ai.ErrProviderDown})
		if err := svc.AddDocument(ctx, "scope", "org-1", "a.md", "text"); err == nil {
			t.Fatal("upload succeeded with a failing embedder")
		}
		if leaser.asked.Load() == 0 || leaser.heldCount() != 0 {
			t.Errorf("asked %d, held %d after a failed embed; want asked and none held", leaser.asked.Load(), leaser.heldCount())
		}
	})
	t.Run("model switch refused", func(t *testing.T) {
		store := &testutil.FakeBackend{EmbeddingDim: 1}
		store.SeedOrgs("org-1", "org-2") // the knowledge base needs its organisation
		seedIdentityChunks(store, []float32{1}, "A", "A")
		leaser := newMemLeaser()
		svc := replica(store, leaser, &namedEmbedder{identity: "B", vec: []float32{0}})
		if _, err := svc.ReindexOrg(ctx, "scope", "org-1", false, interfaces.KnowledgeChunkCursor{}); !errors.Is(err, ErrEmbeddingModelChanged) {
			t.Fatalf("re-index: %v, want ErrEmbeddingModelChanged", err)
		}
		if leaser.asked.Load() == 0 || leaser.heldCount() != 0 {
			t.Errorf("asked %d, held %d after a refused re-index", leaser.asked.Load(), leaser.heldCount())
		}
	})
	t.Run("context cancelled", func(t *testing.T) {
		store := &testutil.FakeBackend{EmbeddingDim: 1}
		store.SeedOrgs("org-1", "org-2") // the knowledge base needs its organisation
		leaser := newMemLeaser()
		blocked := &blockingEmbedder{release: make(chan struct{})}
		svc := replica(store, leaser, blocked)
		cctx, cancel := context.WithCancel(ctx)
		done := asyncUpload(cctx, svc, "org-1", "a.md")
		waitFor(t, "the embedder", blocked.started)
		if leaser.heldCount() != 1 {
			t.Fatalf("held %d leases mid-upload, want 1", leaser.heldCount())
		}
		cancel()
		if err := awaitErr(t, "the cancelled upload", done); err == nil {
			t.Fatal("cancelled upload succeeded")
		}
		if leaser.heldCount() != 0 {
			t.Error("lease still held after the context was cancelled")
		}
		// The process-local lock was given back too: the org can be written again.
		if err := replica(store, leaser, &namedEmbedder{identity: "A", vec: []float32{1}}).AddDocument(ctx, "scope", "org-1", "b.md", "text"); err != nil {
			t.Errorf("upload after the cancelled one: %v", err)
		}
	})
}
