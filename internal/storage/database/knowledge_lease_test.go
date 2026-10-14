package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage/interfaces"
)

// The knowledge base's cross-replica lease on a real Postgres: two KnowledgeService instances on two pools (two
// replicas) race an upload and a re-index of one org. Gated on DATABASE_URL like the other integration tests.

// secondReplica opens another pool on the DATABASE_URL database: a replica's own connections and sessions.
func secondReplica(t *testing.T) *PostgresStorageBackend {
	t.Helper()
	cfg := DefaultConfig(os.Getenv("DATABASE_URL"))
	cfg.EmbeddingDim = 3
	b, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New (second replica): %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// gateEmbedder embeds every input as vec; with a gate, a call announces itself on entered and waits for the gate
// to close (or its context to end).
type gateEmbedder struct {
	ai.Provider
	vec      []float32
	identity string
	gate     chan struct{}
	entered  chan struct{}
	err      error
}

func (g *gateEmbedder) ID() string                { return "gate-embed" }
func (g *gateEmbedder) EmbeddingIdentity() string { return g.identity }
func (g *gateEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if g.err != nil {
		return nil, g.err
	}
	if g.gate != nil {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-g.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = append([]float32(nil), g.vec...)
	}
	return out, nil
}

func (g *gateEmbedder) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the embedder to be reached")
	}
}

// leaseTestOrg is an org id of its own, with its rows removed at cleanup.
func leaseTestOrg(t *testing.T, b *PostgresStorageBackend, name string) string {
	t.Helper()
	org := fmt.Sprintf("lease-%s-%d", name, time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = b.DB().ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id IN (SELECT id FROM knowledge_documents WHERE org_id = $1)`, org)
		_, _ = b.DB().ExecContext(ctx, `DELETE FROM knowledge_documents WHERE org_id = $1`, org)
	})
	return org
}

func identitiesOf(t *testing.T, b *PostgresStorageBackend, org string) map[string]int {
	t.Helper()
	rows, err := b.DB().QueryContext(context.Background(), `
		SELECT COALESCE(c.embedding_identity, ''), COUNT(*) FROM knowledge_chunks c
		JOIN knowledge_documents d ON d.id = c.doc_id WHERE d.org_id = $1 GROUP BY 1`, org)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func errAsync(f func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	return ch
}

func awaitDone(t *testing.T, what string, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func upload(svc *rag.KnowledgeService, org, filename string) error {
	return svc.AddDocument(context.Background(), "scope", org, filename, "a policy text to index")
}

func reindex(svc *rag.KnowledgeService, org string) error {
	_, err := svc.ReindexOrg(context.Background(), "scope", org, true, interfaces.KnowledgeChunkCursor{})
	return err
}

// TestKnowledgeLease_KeySpaceIsDisjointFromHashtext: the real hashtext of an org (the key of the chunk-write
// transaction lock) never equals the lease key, and the advisory lock Postgres records for the lease has the
// fixed high word, which no int4 promoted to the bigint keyspace can have.
func TestKnowledgeLease_KeySpaceIsDisjointFromHashtext(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	for i := range 500 {
		org := fmt.Sprintf("org-%d-%d", i, time.Now().UnixNano())
		var h int64
		if err := b.DB().QueryRowContext(ctx, `SELECT hashtext($1)::bigint`, org).Scan(&h); err != nil {
			t.Fatal(err)
		}
		if hi := h >> 32; hi != 0 && hi != -1 {
			t.Fatalf("hashtext(%q) = %#x has high word %d: the premise (an int4 promoted to bigint) is wrong", org, h, hi)
		}
		if key := rag.KnowledgeLockKey(org); key == h || key>>32 == 0 || key>>32 == -1 {
			t.Fatalf("lease key %#x of %q collides with or sits in the hashtext space (hashtext %#x)", uint64(key), org, h)
		}
	}

	org := leaseTestOrg(t, b, "keyspace")
	lease, ok, err := b.TryLease(ctx, rag.KnowledgeLockKey(org))
	if err != nil || !ok {
		t.Fatalf("TryLease = %v, %v", ok, err)
	}
	defer lease.Release()
	var classid int64
	if err := b.DB().QueryRowContext(ctx, `SELECT classid::bigint FROM pg_locks WHERE locktype = 'advisory' AND granted AND objsubid = 1 AND classid = ($1::bigint >> 32)::oid`, rag.KnowledgeLockKey(org)).Scan(&classid); err != nil {
		t.Fatalf("the lease's lock is not in pg_locks: %v", err)
	}
	if classid != 0x6b6e6f77 {
		t.Errorf("lock high word = %#x, want 0x6b6e6f77", classid)
	}
}

// TestKnowledgeLease_DoesNotBlockTheOperationsOwnWrites is the self-deadlock the survey describes, from both
// sides. A lease on the key a chunk write's transaction lock takes (pg_advisory_xact_lock(hashtext(org)), the
// naive choice) blocks that write forever, because the lease is held on another session of the same operation;
// the lease on KnowledgeLockKey does not.
func TestKnowledgeLease_DoesNotBlockTheOperationsOwnWrites(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	org := leaseTestOrg(t, b, "selfdeadlock")
	doc := org + "-doc"
	if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: doc, OrgID: org, Filename: "d.md"}); err != nil {
		t.Fatal(err)
	}
	write := func(wait time.Duration, id string) error {
		wctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		return b.SaveKnowledgeChunks(wctx, "", []interfaces.KnowledgeChunk{{ID: id, DocID: doc, OrgID: org, Content: "x", Embedding: []float32{1, 0, 0}}})
	}

	// The naive key: the hashtext of the org, as the bigint pg_advisory_xact_lock(hashtext(..)) takes it.
	var naive int64
	if err := b.DB().QueryRowContext(ctx, `SELECT hashtext($1)::bigint`, org).Scan(&naive); err != nil {
		t.Fatal(err)
	}
	bad, ok, err := b.TryLease(ctx, naive)
	if err != nil || !ok {
		t.Fatalf("TryLease(naive) = %v, %v", ok, err)
	}
	if err := write(700*time.Millisecond, org+"-c0"); err == nil {
		t.Error("a chunk write finished while a lease held the hashtext key; the scenario this test guards is not reproduced")
	}
	bad.Release()
	if err := write(10*time.Second, org+"-c1"); err != nil {
		t.Fatalf("chunk write after the naive lease was released: %v", err)
	}

	good, ok, err := b.TryLease(ctx, rag.KnowledgeLockKey(org))
	if err != nil || !ok {
		t.Fatalf("TryLease(KnowledgeLockKey) = %v, %v", ok, err)
	}
	defer good.Release()
	if err := write(10*time.Second, org+"-c2"); err != nil {
		t.Errorf("a chunk write blocked behind the org's own lease (self-deadlock): %v", err)
	}
}

// TestKnowledgeLease_TwoReplicasRaceAnUploadAndAReindex: replica A uploads (check -> embed -> write) and replica
// B tries an upload and a re-index of the same org meanwhile: both are refused with the existing busy, nothing of
// B reaches the corpus (it stays one identity), and once A is done B proceeds. Then the roles swap for a re-index
// held by A.
func TestKnowledgeLease_TwoReplicasRaceAnUploadAndAReindex(t *testing.T) {
	ba := newKnowledgeIntegrationBackend(t)
	bb := secondReplica(t)
	org := leaseTestOrg(t, ba, "race")

	gateA := &gateEmbedder{vec: []float32{1, 0, 0}, identity: "model-A", gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	embB := &gateEmbedder{vec: []float32{0, 1, 0}, identity: "model-B"}
	a := rag.NewKnowledgeService(ba, nil, nil, rag.WithEmbedder(gateA), rag.WithJobLeaser(ba))
	bsvc := rag.NewKnowledgeService(bb, nil, nil, rag.WithEmbedder(embB), rag.WithJobLeaser(bb))

	up := errAsync(func() error { return upload(a, org, "a.md") })
	gateA.waitEntered(t)

	if err := upload(bsvc, org, "b.md"); !errors.Is(err, rag.ErrKnowledgeBusy) {
		t.Errorf("upload on replica B while A embeds: %v, want ErrKnowledgeBusy", err)
	}
	if err := reindex(bsvc, org); !errors.Is(err, rag.ErrKnowledgeBusy) {
		t.Errorf("re-index on replica B while A embeds: %v, want ErrKnowledgeBusy", err)
	}
	// Another org is not held up by it.
	other := leaseTestOrg(t, ba, "race-other")
	if err := upload(bsvc, other, "o.md"); err != nil {
		t.Errorf("upload of another org on replica B: %v", err)
	}

	close(gateA.gate)
	if err := awaitDone(t, "replica A's upload", up); err != nil {
		t.Fatalf("replica A's upload: %v", err)
	}
	if got := identitiesOf(t, ba, org); len(got) != 1 || got["model-A"] == 0 {
		t.Fatalf("corpus identities after A's upload = %v, want only model-A (a mixed corpus is the hazard)", got)
	}

	// Roles swap: A's re-index holds the org while B uploads.
	gateA2 := &gateEmbedder{vec: []float32{1, 0, 0}, identity: "model-A", gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	a2 := rag.NewKnowledgeService(ba, nil, nil, rag.WithEmbedder(gateA2), rag.WithJobLeaser(ba))
	re := errAsync(func() error { return reindex(a2, org) })
	gateA2.waitEntered(t)
	if err := upload(bsvc, org, "b.md"); !errors.Is(err, rag.ErrKnowledgeBusy) {
		t.Errorf("upload on replica B during A's re-index: %v, want ErrKnowledgeBusy", err)
	}
	close(gateA2.gate)
	if err := awaitDone(t, "replica A's re-index", re); err != nil {
		t.Fatalf("replica A's re-index: %v", err)
	}

	// Free again: B's re-index converts the corpus to its model.
	if err := reindex(bsvc, org); err != nil {
		t.Fatalf("re-index on replica B once the org is free: %v", err)
	}
	if got := identitiesOf(t, ba, org); len(got) != 1 || got["model-B"] == 0 {
		t.Errorf("corpus identities after B's re-index = %v, want only model-B", got)
	}
}

// TestKnowledgeLease_ReleasedOnErrorAndCancel: a failed embed and a cancelled request give the org back to every
// replica, and the session that carried the lock goes back to the pool unlocked.
func TestKnowledgeLease_ReleasedOnErrorAndCancel(t *testing.T) {
	ba := newKnowledgeIntegrationBackend(t)
	bb := secondReplica(t)
	org := leaseTestOrg(t, ba, "release")
	key := rag.KnowledgeLockKey(org)
	free := func(what string) {
		t.Helper()
		lease, ok, err := bb.TryLease(context.Background(), key)
		if err != nil || !ok {
			t.Fatalf("after %s the org's lease is still held (%v, %v)", what, ok, err)
		}
		lease.Release()
	}

	failing := rag.NewKnowledgeService(ba, nil, nil, rag.WithEmbedder(&gateEmbedder{err: ai.ErrProviderDown}), rag.WithJobLeaser(ba))
	if err := upload(failing, org, "a.md"); err == nil {
		t.Fatal("upload with a failing embedder succeeded")
	}
	free("a failed embed")

	gate := &gateEmbedder{vec: []float32{1, 0, 0}, identity: "model-A", gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	svc := rag.NewKnowledgeService(ba, nil, nil, rag.WithEmbedder(gate), rag.WithJobLeaser(ba))
	ctx, cancel := context.WithCancel(context.Background())
	done := errAsync(func() error { return svc.AddDocument(ctx, "scope", org, "a.md", "text") })
	gate.waitEntered(t)
	if lease, ok, _ := bb.TryLease(context.Background(), key); ok {
		lease.Release()
		t.Fatal("the org was free while an upload was embedding")
	}
	cancel()
	if err := awaitDone(t, "the cancelled upload", done); err == nil {
		t.Fatal("cancelled upload succeeded")
	}
	free("a cancelled request")
}

// TestKnowledgeLease_CapsPinnedConnections: with a cap of one, a second org on the same replica is told busy while
// the first holds its lease, and Postgres sees exactly one lease lock of the knowledge-base space from the
// replica.
func TestKnowledgeLease_CapsPinnedConnections(t *testing.T) {
	ba := newKnowledgeIntegrationBackend(t)
	org1, org2 := leaseTestOrg(t, ba, "cap1"), leaseTestOrg(t, ba, "cap2")
	gate := &gateEmbedder{vec: []float32{1, 0, 0}, identity: "model-A", gate: make(chan struct{}), entered: make(chan struct{}, 2)}
	svc := rag.NewKnowledgeService(ba, nil, nil, rag.WithEmbedder(gate), rag.WithJobLeaser(ba), rag.WithLeaseCap(1))

	one := errAsync(func() error { return upload(svc, org1, "a.md") })
	gate.waitEntered(t)
	if err := upload(svc, org2, "a.md"); !errors.Is(err, rag.ErrKnowledgeBusy) {
		t.Errorf("second org beyond the cap: %v, want ErrKnowledgeBusy", err)
	}
	var held int
	if err := ba.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND classid = ($1::bigint)::oid`, int64(0x6b6e6f77)).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held < 1 {
		t.Errorf("no knowledge-base lease lock visible in pg_locks while an upload runs")
	}
	close(gate.gate)
	if err := awaitDone(t, "the first upload", one); err != nil {
		t.Fatal(err)
	}
	if err := upload(svc, org2, "a.md"); err != nil {
		t.Errorf("second org once the slot is free: %v", err)
	}
}

// TestKnowledgeLease_ManyReplicasOneOrg: several replicas upload to one org at the same time; the embedder never
// has two callers at once for it, every upload either completes or is told busy, and nothing is left locked.
func TestKnowledgeLease_ManyReplicasOneOrg(t *testing.T) {
	first := newKnowledgeIntegrationBackend(t)
	org := leaseTestOrg(t, first, "many")
	backends := []*PostgresStorageBackend{first, secondReplica(t), secondReplica(t)}

	var mu sync.Mutex
	inside, maxInside, done, busy := 0, 0, 0, 0
	track := &trackingEmbedder{enter: func() func() {
		mu.Lock()
		inside++
		maxInside = max(maxInside, inside)
		mu.Unlock()
		return func() { mu.Lock(); inside--; mu.Unlock() }
	}}
	var wg sync.WaitGroup
	for i, b := range backends {
		svc := rag.NewKnowledgeService(b, nil, nil, rag.WithEmbedder(track), rag.WithJobLeaser(b))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 6 {
				err := upload(svc, org, fmt.Sprintf("doc-%d-%d.md", i, j))
				mu.Lock()
				switch {
				case err == nil:
					done++
				case errors.Is(err, rag.ErrKnowledgeBusy):
					busy++
				default:
					t.Errorf("upload: %v", err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if maxInside > 1 {
		t.Errorf("%d uploads of one org embedded at once across replicas, want 1", maxInside)
	}
	if done == 0 || done+busy != 18 {
		t.Errorf("completed %d, busy %d of 18", done, busy)
	}
	lease, ok, err := first.TryLease(context.Background(), rag.KnowledgeLockKey(org))
	if err != nil || !ok {
		t.Fatalf("the org's lease is still held after every upload returned (%v, %v)", ok, err)
	}
	lease.Release()
}

// trackingEmbedder reports entry and exit of every Embed call, holding each briefly so overlaps would show.
type trackingEmbedder struct {
	ai.Provider
	enter func() func()
}

func (e *trackingEmbedder) ID() string                { return "tracking-embed" }
func (e *trackingEmbedder) EmbeddingIdentity() string { return "model-T" }
func (e *trackingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	exit := e.enter()
	defer exit()
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}
