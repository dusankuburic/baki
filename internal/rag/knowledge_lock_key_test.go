package rag

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// TestKnowledgeLockKey_HighWordKeepsOutOfTheXactLockSpace: the chunk-write transactions take
// pg_advisory_xact_lock(hashtext(org_id)), an int4 promoted to the bigint keyspace, so every key of that space has
// a high word of 0 (positive hash) or -1 (negative hash, sign-extended). A lease on such a key would be held on
// one session while the operation's own write transaction waits for it on another: a self-deadlock. The lease key
// therefore carries a fixed non-zero high word, and no hashtext value can equal it. (The Postgres test beside the
// storage package proves the same against the real hashtext.)
func TestKnowledgeLockKey_HighWordKeepsOutOfTheXactLockSpace(t *testing.T) {
	orgs := []string{"", "org-1", "org-2", "0", "-1", "11111111-2222-3333-4444-555555555555", "ORG-1", "org-1 "}
	for i := range 2000 {
		orgs = append(orgs, fmt.Sprintf("org-%d", i), fmt.Sprintf("%08x-0000-4000-8000-%012x", i*7919, i))
	}
	seen := map[int64]string{}
	for _, org := range orgs {
		key := KnowledgeLockKey(org)
		if hi := key >> 32; hi == 0 || hi == -1 {
			t.Fatalf("KnowledgeLockKey(%q) = %#x: high word %d is in the hashtext space", org, uint64(key), hi)
		}
		if key>>32 != int64(knowledgeLockSpace) {
			t.Errorf("KnowledgeLockKey(%q) high word = %#x, want the fixed %#x", org, key>>32, knowledgeLockSpace)
		}
		if again := KnowledgeLockKey(org); again != key {
			t.Errorf("KnowledgeLockKey(%q) is not deterministic: %#x then %#x", org, uint64(key), uint64(again))
		}
		if other, dup := seen[key]; dup && other != org {
			t.Errorf("KnowledgeLockKey collides for %q and %q", org, other)
		}
		seen[key] = org
	}
}

// TestKnowledgeLockKey_NotAJobKey: the job leases share the advisory keyspace; none of the declared jobs' keys
// may be an org's.
func TestKnowledgeLockKey_NotAJobKey(t *testing.T) {
	jobKeys := map[int64]string{}
	for _, j := range lifecycle.Jobs() {
		jobKeys[j.Key] = j.Name
		if j.Key>>32 == int64(knowledgeLockSpace) {
			t.Errorf("job %s has a key in the knowledge-base lock space (%#x)", j.Name, uint64(j.Key))
		}
	}
	if len(jobKeys) == 0 {
		t.Skip("no jobs declared in this binary")
	}
	for i := range 5000 {
		if name, clash := jobKeys[KnowledgeLockKey(fmt.Sprintf("org-%d", i))]; clash {
			t.Fatalf("org-%d's lease key is the key of job %s", i, name)
		}
	}
}

// TestKnowledgeLock_LeaseIsOnTheOrgsKey: an upload and a re-index lease the org's key, and nothing else.
func TestKnowledgeLock_LeaseIsOnTheOrgsKey(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	seedManyChunks(store, 1, 2)
	leaser := newMemLeaser()
	svc := replica(store, leaser, &namedEmbedder{identity: "A", vec: []float32{1}})
	ctx := context.Background()
	if _, err := svc.ReindexOrg(ctx, "scope", "org-1", true, interfaces.KnowledgeChunkCursor{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddDocument(ctx, "scope", "org-1", "a.md", "text"); err != nil {
		t.Fatal(err)
	}
	want := KnowledgeLockKey("org-1")
	if len(leaser.keys) != 2 || leaser.keys[0] != want || leaser.keys[1] != want {
		t.Errorf("leases taken on %#x, want two on %#x", leaser.keys, uint64(want))
	}
}

// TestKnowledgeLock_LostLeaseStopsTheOperation: when the lease reports lost (its session died, another replica may
// hold the org now) the upload stops at once and writes nothing; busy, because the org is being updated elsewhere.
func TestKnowledgeLock_LostLeaseStopsTheOperation(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	leaser := newMemLeaser()
	blocked := &blockingEmbedder{release: make(chan struct{})}
	svc := replica(store, leaser, blocked)

	upload := asyncUpload(context.Background(), svc, "org-1", "a.md")
	waitFor(t, "the embedder", blocked.started)
	leaser.holding(KnowledgeLockKey("org-1")).lose()

	err := awaitErr(t, "the upload to notice", upload)
	if !errors.Is(err, ErrKnowledgeBusy) {
		t.Fatalf("upload after the lease was lost: %v, want ErrKnowledgeBusy", err)
	}
	if docs, _ := store.ListKnowledgeDocuments(context.Background(), "org-1", 0, 0); len(docs) != 0 {
		t.Errorf("%d document(s) written after the lease was lost", len(docs))
	}
	if leaser.heldCount() != 0 {
		t.Error("lease not released after it was lost")
	}
	// The local lock was released as well.
	if err := replica(store, leaser, &namedEmbedder{identity: "A", vec: []float32{1}}).AddDocument(context.Background(), "scope", "org-1", "b.md", "text"); err != nil {
		t.Errorf("upload after the lost one: %v", err)
	}
}

// TestKnowledgeLock_LostLeaseStopsTheReindex: the same for the page loop; pages committed before stay converted
// and the answer is busy, with the partial result's cursor so the admin can resume.
func TestKnowledgeLock_LostLeaseStopsTheReindex(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	seedManyChunks(store, 1, 3)
	leaser := newMemLeaser()
	blocked := &blockingEmbedder{release: make(chan struct{})}
	svc := replica(store, leaser, blocked)

	reindex := asyncReindex(context.Background(), svc, "org-1")
	waitFor(t, "the embedder", blocked.started)
	leaser.holding(KnowledgeLockKey("org-1")).lose()
	if err := awaitErr(t, "the re-index to notice", reindex); !errors.Is(err, ErrKnowledgeBusy) {
		t.Fatalf("re-index after the lease was lost: %v, want ErrKnowledgeBusy", err)
	}
	if leaser.heldCount() != 0 {
		t.Error("lease not released after it was lost")
	}
}

// TestKnowledgeLock_LeaseUnavailable: a lease that cannot be asked for (the database is down) refuses the
// operation with a retryable error and leaves the process-local lock free; it never runs unprotected.
func TestKnowledgeLock_LeaseUnavailable(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	leaser := newMemLeaser()
	leaser.err = errors.New("connection refused")
	emb := &namedEmbedder{identity: "A", vec: []float32{1}}
	svc := replica(store, leaser, emb)

	err := svc.AddDocument(context.Background(), "scope", "org-1", "a.md", "text")
	if !errors.Is(err, ErrKnowledgeLockUnavailable) {
		t.Fatalf("upload with the lock unavailable: %v, want ErrKnowledgeLockUnavailable", err)
	}
	if errors.Is(err, ErrKnowledgeBusy) {
		t.Error("an unreachable lock reads as busy; it is a 503, not a 409")
	}
	if emb.calls != 0 {
		t.Errorf("embedded %d times without the lock", emb.calls)
	}
	leaser.err = nil
	if err := svc.AddDocument(context.Background(), "scope", "org-1", "a.md", "text"); err != nil {
		t.Errorf("upload once the lock is back: %v", err)
	}
}

// TestKnowledgeLock_NoLeaserKeepsTheLocalLock: without a leaser (the desktop, no database) the process-local lock
// alone answers busy, as before.
func TestKnowledgeLock_NoLeaserKeepsTheLocalLock(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	blocked := &blockingEmbedder{release: make(chan struct{})}
	svc := NewKnowledgeService(store, nil, nil, WithEmbedder(blocked))
	upload := asyncUpload(context.Background(), svc, "org-1", "a.md")
	waitFor(t, "the embedder", blocked.started)
	if err := svc.AddDocument(context.Background(), "scope", "org-1", "b.md", "text"); !errors.Is(err, ErrKnowledgeBusy) {
		t.Errorf("second upload in one process: %v, want ErrKnowledgeBusy", err)
	}
	if svc.leaseSlots != nil {
		t.Error("a service without a leaser built lease slots")
	}
	_ = upload
}

// TestKnowledgeLock_CapsPinnedConnectionsPerReplica: a lease pins a database connection for the whole operation,
// so a replica holds at most the cap at once; the next org is told busy rather than waiting for the pool, and the
// slot comes back when an operation ends.
func TestKnowledgeLock_CapsPinnedConnectionsPerReplica(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	leaser := newMemLeaser()
	release := make(chan struct{})
	blocked := &blockingEmbedder{release: release}
	svc := replica(store, leaser, blocked, WithLeaseCap(2))

	one := asyncUpload(context.Background(), svc, "org-1", "a.md")
	two := asyncUpload(context.Background(), svc, "org-2", "a.md")
	waitFor(t, "two operations in flight", func() bool { return leaser.heldCount() == 2 })

	if err := svc.AddDocument(context.Background(), "scope", "org-3", "a.md", "text"); !errors.Is(err, ErrKnowledgeBusy) {
		t.Fatalf("third org beyond the cap: %v, want ErrKnowledgeBusy", err)
	}
	if leaser.heldCount() != 2 {
		t.Errorf("held %d leases, want the two in flight (the refused one took none)", leaser.heldCount())
	}
	// Another replica has its own cap.
	other := replica(store, leaser, &namedEmbedder{identity: "A", vec: []float32{1}}, WithLeaseCap(2))
	if err := other.AddDocument(context.Background(), "scope", "org-3", "a.md", "text"); err != nil {
		t.Errorf("third org on another replica: %v", err)
	}

	close(release)
	for _, ch := range []<-chan error{one, two} {
		if err := awaitErr(t, "an in-flight upload", ch); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AddDocument(context.Background(), "scope", "org-3", "b.md", "text"); err != nil {
		t.Errorf("third org once a slot is free: %v", err)
	}
}

// TestKnowledgeLock_CapSlotReleasedOnRefusal: a refusal at any step (the lease held elsewhere, the lock
// unavailable) returns the slot it took.
func TestKnowledgeLock_CapSlotReleasedOnRefusal(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	leaser := newMemLeaser()
	emb := &namedEmbedder{identity: "A", vec: []float32{1}}
	svc := replica(store, leaser, emb, WithLeaseCap(1))

	held, _, _ := leaser.TryLease(context.Background(), KnowledgeLockKey("org-1"))
	for range 3 {
		if err := svc.AddDocument(context.Background(), "scope", "org-1", "a.md", "text"); !errors.Is(err, ErrKnowledgeBusy) {
			t.Fatalf("upload while another replica holds the org: %v, want ErrKnowledgeBusy", err)
		}
	}
	held.Release()
	leaser.err = errors.New("connection refused")
	for range 3 {
		if err := svc.AddDocument(context.Background(), "scope", "org-1", "a.md", "text"); !errors.Is(err, ErrKnowledgeLockUnavailable) {
			t.Fatalf("upload with the lock unavailable: %v", err)
		}
	}
	leaser.err = nil
	if err := svc.AddDocument(context.Background(), "scope", "org-1", "a.md", "text"); err != nil {
		t.Errorf("a refusal leaked the only slot: %v", err)
	}
}

// TestKnowledgeLock_DefaultCap: a service with a leaser and no explicit cap gets the default; a non-positive cap
// means the default as well.
func TestKnowledgeLock_DefaultCap(t *testing.T) {
	for _, n := range []int{-1, 0} {
		svc := replica(&testutil.FakeBackend{}, newMemLeaser(), &namedEmbedder{}, WithLeaseCap(n))
		if got := cap(svc.leaseSlots); got != DefaultLeaseCap {
			t.Errorf("WithLeaseCap(%d): %d slots, want the default %d", n, got, DefaultLeaseCap)
		}
	}
	svc := replica(&testutil.FakeBackend{}, newMemLeaser(), &namedEmbedder{})
	if got := cap(svc.leaseSlots); got != DefaultLeaseCap {
		t.Errorf("no WithLeaseCap: %d slots, want %d", got, DefaultLeaseCap)
	}
}

// TestKnowledgeLock_ConcurrentReplicasNeverOverlap: many replicas hammer one org; the operations never overlap
// (the embedder counts concurrent callers) and each either completes or is told busy.
func TestKnowledgeLock_ConcurrentReplicasNeverOverlap(t *testing.T) {
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	leaser := newMemLeaser()
	emb := &overlapEmbedder{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	done, busy := 0, 0
	for i := range 8 {
		svc := replica(store, leaser, emb)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 10 {
				err := svc.AddDocument(context.Background(), "scope", "org-1", fmt.Sprintf("doc-%d-%d.md", i, j), "text to index")
				mu.Lock()
				switch {
				case err == nil:
					done++
				case errors.Is(err, ErrKnowledgeBusy):
					busy++
				default:
					t.Errorf("upload: %v", err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if emb.maxConcurrent.Load() > 1 {
		t.Errorf("%d uploads of one org embedded at once, want 1", emb.maxConcurrent.Load())
	}
	if done == 0 || done+busy != 80 {
		t.Errorf("completed %d, busy %d of 80", done, busy)
	}
	if leaser.heldCount() != 0 {
		t.Errorf("%d lease(s) left held", leaser.heldCount())
	}
}

// overlapEmbedder records the most callers inside Embed at once.
type overlapEmbedder struct {
	ai.Provider
	inside, maxConcurrent atomic.Int32
}

func (o *overlapEmbedder) ID() string { return "overlap-embed" }
func (o *overlapEmbedder) Embed(ctx context.Context, text []string) ([][]float32, error) {
	n := o.inside.Add(1)
	defer o.inside.Add(-1)
	for {
		m := o.maxConcurrent.Load()
		if n <= m || o.maxConcurrent.CompareAndSwap(m, n) {
			break
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	out := make([][]float32, len(text))
	for i := range text {
		out[i] = []float32{1}
	}
	return out, nil
}

// stuckLeaser never answers until its context ends: the shape of TryLease with the pool exhausted, where it waits
// for a connection before it can ask the database.
type stuckLeaser struct{}

func (stuckLeaser) TryLease(ctx context.Context, _ int64) (lock.Lease, bool, error) {
	<-ctx.Done()
	return nil, false, ctx.Err()
}

// TestKnowledgeLock_AskingForTheLeaseIsBounded: a lease that cannot be asked for within leaseAcquireTimeout (the
// pool is exhausted) refuses with ErrKnowledgeLockUnavailable instead of holding the request until its deadline,
// and gives the slot and the local lock back.
func TestKnowledgeLock_AskingForTheLeaseIsBounded(t *testing.T) {
	old := leaseAcquireTimeout
	leaseAcquireTimeout = 50 * time.Millisecond
	t.Cleanup(func() { leaseAcquireTimeout = old })
	store := &testutil.FakeBackend{EmbeddingDim: 1}
	emb := &namedEmbedder{identity: "A", vec: []float32{1}}
	svc := replica(store, stuckLeaser{}, emb, WithLeaseCap(1))
	for range 2 { // the second proves the slot and the local lock came back
		start := time.Now()
		err := svc.AddDocument(context.Background(), "scope", "org-1", "a.md", "text")
		if !errors.Is(err, ErrKnowledgeLockUnavailable) {
			t.Fatalf("upload with a stuck lease: %v, want ErrKnowledgeLockUnavailable", err)
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("refusal took %v, want about the acquire timeout", time.Since(start))
		}
	}
}
