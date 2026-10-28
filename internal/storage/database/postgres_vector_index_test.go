package database

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestKnowledgeVectorIndexDDL_Shape pins the index statement without a
// database: CONCURRENTLY (it runs outside the migration transaction), IF NOT
// EXISTS, the cast expression and the partial predicate the search statement
// repeats — the two must agree or the planner cannot use the index.
func TestKnowledgeVectorIndexDDL_Shape(t *testing.T) {
	ddl := knowledgeVectorIndexDDL(1536)
	for _, want := range []string{
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS knowledge_chunks_hnsw_1536 ON knowledge_chunks",
		"USING hnsw ((embedding_vec::vector(1536)) vector_cosine_ops)",
		"WHERE vector_dims(embedding_vec) = 1536",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL lacks %q: %s", want, ddl)
		}
	}
	q := oneLine(knowledgeVectorSearchSQL(1536))
	for _, want := range []string{
		"c.embedding_vec::vector(1536) <=> $2::vector(1536) AS dist",
		"FROM knowledge_chunks c WHERE c.org_id = $1 AND vector_dims(c.embedding_vec) = 1536 AND NOT c.pending AND ($5::text = '' OR c.embedding_identity IS NULL OR c.embedding_identity = $5::text) ORDER BY dist LIMIT $3",
		") nearest JOIN knowledge_documents d ON d.id = nearest.doc_id WHERE nearest.dist <= $4 ORDER BY nearest.dist",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("search SQL lacks %q: %s", want, q)
		}
	}
	if strings.Count(q, "<=>") != 1 {
		t.Errorf("the distance must be computed once, found %d <=>: %s", strings.Count(q, "<=>"), q)
	}
}

// vectorIndexFixture is the DATABASE_URL-gated corpus for the index tests:
// a backend configured for width 8, one user who is a member of org A only,
// and width-8 chunks in org A and org B (plus one width-3 chunk in org B,
// the leftover of an "earlier configuration").
type vectorIndexFixture struct {
	t      *testing.T
	b      *PostgresStorageBackend
	ctx    context.Context
	run    string
	user   string
	orgA   string
	orgB   string
	docA   string
	docB   string
	chunks map[string][]float32 // org A chunk id -> embedding
}

const vectorIndexTestDims = 8

// vectorFixtureOpts shapes the corpus: how many chunks each org gets and
// how their embeddings are generated (defaults: 1000 each, uniform random).
type vectorFixtureOpts struct {
	countA, countB int
	embA, embB     func(rng *rand.Rand, i int) []float32
}

func randomEmbedding(rng *rand.Rand, _ int) []float32 {
	emb := make([]float32, vectorIndexTestDims)
	for j := range emb {
		emb[j] = rng.Float32()*2 - 1
	}
	return emb
}

func newVectorIndexFixture(t *testing.T) *vectorIndexFixture {
	t.Helper()
	return newVectorIndexFixtureWith(t, vectorFixtureOpts{})
}

func newVectorIndexFixtureWith(t *testing.T, opts vectorFixtureOpts) *vectorIndexFixture {
	t.Helper()
	if opts.countA == 0 {
		opts.countA = 1000
	}
	if opts.countB == 0 {
		opts.countB = 1000
	}
	if opts.embA == nil {
		opts.embA = randomEmbedding
	}
	if opts.embB == nil {
		opts.embB = randomEmbedding
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping pgvector index test")
	}
	ctx := context.Background()
	cfg := DefaultConfig(dsn)
	cfg.EmbeddingDim = vectorIndexTestDims
	b, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	if !b.HasPgvector() {
		t.Skip("pgvector not installed")
	}
	f := &vectorIndexFixture{t: t, b: b, ctx: ctx, run: time.Now().Format("150405.000000000"), chunks: map[string][]float32{}}
	f.user, f.orgA, f.orgB = f.id("user"), f.id("orgA"), f.id("orgB")
	f.docA, f.docB = f.id("docA"), f.id("docB")
	db := b.db
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DROP INDEX IF EXISTS `+knowledgeVectorIndexName(vectorIndexTestDims))
		_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = ANY($1)`, []string{f.docA, f.docB})
		_, _ = db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = ANY($1)`, []string{f.docA, f.docB})
		_, _ = db.ExecContext(ctx, `DELETE FROM org_members WHERE org_id = ANY($1)`, []string{f.orgA, f.orgB})
		_, _ = db.ExecContext(ctx, `DELETE FROM organisations WHERE id = ANY($1)`, []string{f.orgA, f.orgB})
		_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, f.user)
	})
	// The index under test must not pre-exist (a previous run's leftover).
	if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS `+knowledgeVectorIndexName(vectorIndexTestDims)); err != nil {
		t.Fatalf("drop stale index: %v", err)
	}
	if err := b.CreateUser(ctx, &interfaces.User{
		ID: f.user, Email: f.user + "@test.local", Password: "$2a$12$testhash", Role: "member",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: f.orgA, Name: "A", OwnerID: f.user,
		Members: []interfaces.OrgMember{{UserID: f.user, Role: "owner"}}}); err != nil {
		t.Fatalf("SaveOrg A: %v", err)
	}
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: f.orgB, Name: "B", OwnerID: f.user}); err != nil {
		t.Fatalf("SaveOrg B: %v", err)
	}
	for _, d := range []struct{ id, org string }{{f.docA, f.orgA}, {f.docB, f.orgB}} {
		if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: d.id, OrgID: d.org, Filename: d.id + ".txt"}); err != nil {
			t.Fatalf("SaveKnowledgeDocument %s: %v", d.id, err)
		}
	}
	rng := rand.New(rand.NewSource(8))
	var batch []interfaces.KnowledgeChunk
	for i := 0; i < opts.countA; i++ {
		id := fmt.Sprintf("%s-a%04d", f.id("c"), i)
		emb := opts.embA(rng, i)
		f.chunks[id] = emb
		batch = append(batch, interfaces.KnowledgeChunk{ID: id, DocID: f.docA, Content: id, Embedding: emb})
	}
	for i := 0; i < opts.countB; i++ {
		id := fmt.Sprintf("%s-b%04d", f.id("c"), i)
		batch = append(batch, interfaces.KnowledgeChunk{ID: id, DocID: f.docB, Content: id, Embedding: opts.embB(rng, i)})
	}
	if err := b.SaveKnowledgeChunks(ctx, "", batch); err != nil {
		t.Fatalf("SaveKnowledgeChunks: %v", err)
	}
	// A chunk of another width in org B: the partial index and the search's
	// vector_dims predicate must both leave it alone.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO knowledge_chunks (id, doc_id, content, embedding, embedding_vec)
		VALUES ($1, $2, 'width3', '[1,0,0]'::jsonb, '[1,0,0]'::vector)`, f.id("c-width3"), f.docB); err != nil {
		t.Fatalf("seed width-3 chunk: %v", err)
	}
	var indexed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_chunks WHERE doc_id = ANY($1) AND vector_dims(embedding_vec) = $2`,
		[]string{f.docA, f.docB}, vectorIndexTestDims).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if want := opts.countA + opts.countB; indexed != want {
		t.Fatalf("%d width-%d chunks have embedding_vec, want %d", indexed, vectorIndexTestDims, want)
	}
	return f
}

func (f *vectorIndexFixture) id(label string) string { return "vidx-" + label + "-" + f.run }

// indexState returns (exists, valid) for the width-8 index.
func (f *vectorIndexFixture) indexState() (bool, bool) {
	f.t.Helper()
	var valid bool
	err := f.b.db.QueryRowContext(f.ctx, `
		SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`,
		knowledgeVectorIndexName(vectorIndexTestDims)).Scan(&valid)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return false, false
		}
		f.t.Fatalf("pg_index: %v", err)
	}
	return true, valid
}

// rls returns a context running as the fixture user under RLS; rolled back
// at cleanup.
func (f *vectorIndexFixture) rls() context.Context {
	f.t.Helper()
	tx, err := f.b.BeginRLS(f.ctx, f.user)
	if err != nil {
		f.t.Fatalf("BeginRLS: %v", err)
	}
	f.t.Cleanup(func() { _ = tx.Rollback() })
	return WithRLSTx(f.ctx, tx)
}

func cosineDistance(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// exactTopK is the brute-force answer: org A's k nearest chunks within the
// relevance cutoff, nearest first.
func (f *vectorIndexFixture) exactTopK(q []float32, k int) []string {
	type hit struct {
		id   string
		dist float64
	}
	var hits []hit
	for id, emb := range f.chunks {
		if d := cosineDistance(q, emb); d <= knowledgeMaxCosineDistance {
			hits = append(hits, hit{id, d})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].dist < hits[j].dist || (hits[i].dist == hits[j].dist && hits[i].id < hits[j].id)
	})
	out := make([]string, 0, k)
	for i := 0; i < len(hits) && i < k; i++ {
		out = append(out, hits[i].id)
	}
	return out
}

// vecPlanNode is one EXPLAIN (FORMAT JSON) node.
type vecPlanNode struct {
	NodeType  string        `json:"Node Type"`
	IndexName string        `json:"Index Name"`
	Plans     []vecPlanNode `json:"Plans"`
}

func (n vecPlanNode) usesIndex(name string) bool {
	if n.NodeType == "Index Scan" && n.IndexName == name {
		return true
	}
	for _, c := range n.Plans {
		if c.usesIndex(name) {
			return true
		}
	}
	return false
}

// TestEnsureVectorIndex_BuildsAndPlannerUsesIt: EnsureVectorIndex replaces
// an invalid leftover with a valid partial HNSW index, is idempotent, and
// the search statement — run as the application role under RLS, exactly
// as SearchKnowledge runs it — is planned as an Index Scan on it.
func TestEnsureVectorIndex_BuildsAndPlannerUsesIt(t *testing.T) {
	f := newVectorIndexFixture(t)
	b, ctx := f.b, f.ctx
	name := knowledgeVectorIndexName(vectorIndexTestDims)

	// An interrupted build leaves an INVALID index. Provoke one: without the
	// partial predicate the cast raises on the width-3 chunk and the
	// CONCURRENTLY build fails, leaving the invalid index behind.
	badDDL := strings.Replace(knowledgeVectorIndexDDL(vectorIndexTestDims), " WHERE vector_dims(embedding_vec) = 8", "", 1)
	if _, err := b.db.ExecContext(ctx, badDDL); err == nil {
		t.Fatal("unfiltered build over mixed widths succeeded; the fixture should have made it fail")
	}
	if exists, valid := f.indexState(); !exists || valid {
		t.Fatalf("after the failed build: exists=%v valid=%v, want an invalid leftover", exists, valid)
	}

	if err := b.EnsureVectorIndex(ctx, vectorIndexTestDims); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}
	if exists, valid := f.indexState(); !exists || !valid {
		t.Fatalf("after EnsureVectorIndex: exists=%v valid=%v, want a valid index", exists, valid)
	}
	if err := b.EnsureVectorIndex(ctx, vectorIndexTestDims); err != nil {
		t.Fatalf("EnsureVectorIndex (again): %v", err)
	}
	var n int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d indexes named %s, want 1", n, name)
	}
	if _, err := b.db.ExecContext(ctx, `ANALYZE knowledge_chunks`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	rctx := f.rls()
	tx := b.query(rctx)
	for _, stmt := range []string{`SET LOCAL enable_seqscan = off`, `SET LOCAL hnsw.iterative_scan = relaxed_order`} {
		if _, err := tx.ExecContext(rctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	q := make([]float32, vectorIndexTestDims)
	for i := range q {
		q[i] = float32(i) / 10
	}
	var planJSON string
	if err := tx.QueryRowContext(rctx, `EXPLAIN (FORMAT JSON) `+knowledgeVectorSearchSQL(vectorIndexTestDims),
		f.orgA, FormatVector(q), 5, knowledgeMaxCosineDistance, "ident").Scan(&planJSON); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var plan []struct {
		Plan vecPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil || len(plan) != 1 {
		t.Fatalf("parse plan (%v): %s", err, planJSON)
	}
	if !plan[0].Plan.usesIndex(name) {
		t.Errorf("the search is not an Index Scan on %s; plan:\n%s", name, planJSON)
	}
}

// TestSearchKnowledgeVector_RecallMatchesExact: through the index, the
// search under RLS returns org A's nearest chunks only (never org B's,
// never the width-3 one) and, HNSW being approximate, at least 4 of the
// exact top-5 for every one of ten query vectors.
func TestSearchKnowledgeVector_RecallMatchesExact(t *testing.T) {
	f := newVectorIndexFixture(t)
	b, ctx := f.b, f.ctx
	if err := b.EnsureVectorIndex(ctx, vectorIndexTestDims); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}
	if exists, valid := f.indexState(); !exists || !valid {
		t.Fatalf("exists=%v valid=%v, want a valid index", exists, valid)
	}
	if _, err := b.db.ExecContext(ctx, `ANALYZE knowledge_chunks`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	t.Logf("hnsw.iterative_scan supported: %v", b.hnswIterativeScan)

	rctx := f.rls()
	rng := rand.New(rand.NewSource(99))
	total := 0
	for qi := 0; qi < 10; qi++ {
		q := make([]float32, vectorIndexTestDims)
		for j := range q {
			q[j] = rng.Float32()*2 - 1
		}
		want := f.exactTopK(q, 5)
		got, err := b.searchKnowledgeVector(rctx, f.orgA, q, 5, "")
		if err != nil {
			t.Fatalf("query %d: searchKnowledgeVector: %v", qi, err)
		}
		if len(got) != len(want) {
			t.Errorf("query %d: %d hits, want %d (%v vs %v)", qi, len(got), len(want), ids(got), want)
		}
		overlap := 0
		for _, c := range got {
			if c.DocID != f.docA {
				t.Errorf("query %d: hit %s belongs to doc %s, not org A's", qi, c.ID, c.DocID)
			}
			for _, w := range want {
				if c.ID == w {
					overlap++
				}
			}
		}
		total += overlap
		if overlap < 4 {
			t.Errorf("query %d: recall %d/5 (got %v, exact %v)", qi, overlap, ids(got), want)
		}
		if len(got) > 0 && len(want) > 0 && got[0].ID != want[0] {
			t.Logf("query %d: nearest differs (got %s, exact %s)", qi, got[0].ID, want[0])
		}
	}
	t.Logf("recall %d/50", total)
	if total < 40 {
		t.Errorf("recall %d/50 across 10 queries, want >= 40", total)
	}
}

// TestSearchKnowledgeVector_SmallTenantWithoutIterativeScan: the index
// orders every org's chunks, so without an iterative scan (pgvector < 0.8)
// HNSW hands over its ef_search (40) nearest candidates and stops — a
// small tenant whose chunks sit behind another org's gets nothing. The
// search must therefore not use the index when the scan cannot iterate.
// Org A: 2000 chunks tightly around u; org B: 5 chunks around v, with
// cos(u, v) = 0.7 (distance 0.3, inside the cutoff); query u as org B.
func TestSearchKnowledgeVector_SmallTenantWithoutIterativeScan(t *testing.T) {
	u := []float32{1, 0, 0, 0, 0, 0, 0, 0}
	v := []float32{0.7, float32(math.Sqrt(1 - 0.49)), 0, 0, 0, 0, 0, 0}
	around := func(center []float32, spread float32) func(rng *rand.Rand, i int) []float32 {
		return func(rng *rand.Rand, _ int) []float32 {
			emb := make([]float32, vectorIndexTestDims)
			for j := range emb {
				emb[j] = center[j] + (rng.Float32()*2-1)*spread
			}
			return emb
		}
	}
	f := newVectorIndexFixtureWith(t, vectorFixtureOpts{countA: 2000, countB: 5, embA: around(u, 0.05), embB: around(v, 0.01)})
	b, ctx := f.b, f.ctx
	if err := b.EnsureVectorIndex(ctx, vectorIndexTestDims); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}
	if exists, valid := f.indexState(); !exists || !valid {
		t.Fatalf("exists=%v valid=%v, want a valid index", exists, valid)
	}
	if _, err := b.db.ExecContext(ctx, `ANALYZE knowledge_chunks`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	// The fixture user must see org B's documents.
	if _, err := b.db.ExecContext(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, f.orgB, f.user); err != nil {
		t.Fatalf("join org B: %v", err)
	}

	check := func(label string) {
		t.Helper()
		got, err := b.searchKnowledgeVector(f.rls(), f.orgB, u, 5, "")
		if err != nil {
			t.Fatalf("%s: searchKnowledgeVector: %v", label, err)
		}
		if len(got) != 5 {
			t.Errorf("%s: org B got %d hits %v, want its 5 chunks", label, len(got), ids(got))
		}
		for _, c := range got {
			if c.DocID != f.docB {
				t.Errorf("%s: hit %s belongs to %s, not org B", label, c.ID, c.DocID)
			}
		}
	}
	// Which statement the backend picks, and how the planner runs it, as
	// org A (the tenant big enough that the planner does reach for the
	// index when offered the indexed form). The results check above cannot
	// tell the two apart here: for a one-document tenant the planner joins
	// from the document and sorts, so the ef_search cut-off never shows.
	explain := func(label string, wantIndexed bool) {
		t.Helper()
		stmt, indexed := b.knowledgeVectorStatement()
		if indexed != wantIndexed {
			t.Fatalf("%s: indexed statement = %v, want %v", label, indexed, wantIndexed)
		}
		args := []any{f.orgA, FormatVector(u), 5, knowledgeMaxCosineDistance}
		if !indexed {
			args = append(args, vectorIndexTestDims)
		}
		args = append(args, "ident")
		rctx := f.rls()
		tx := b.query(rctx)
		if _, err := tx.ExecContext(rctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		var planJSON string
		if err := tx.QueryRowContext(rctx, `EXPLAIN (FORMAT JSON) `+stmt, args...).Scan(&planJSON); err != nil {
			t.Fatalf("%s: EXPLAIN: %v", label, err)
		}
		var plan []struct {
			Plan vecPlanNode `json:"Plan"`
		}
		if err := json.Unmarshal([]byte(planJSON), &plan); err != nil || len(plan) != 1 {
			t.Fatalf("%s: parse plan (%v): %s", label, err, planJSON)
		}
		if got := plan[0].Plan.usesIndex(knowledgeVectorIndexName(vectorIndexTestDims)); got != wantIndexed {
			t.Errorf("%s: HNSW Index Scan in plan = %v, want %v; plan:\n%s", label, got, wantIndexed, planJSON)
		}
	}
	check("iterative scan available")
	explain("iterative scan available", true)
	b.hnswIterativeScan = false // pgvector < 0.8
	check("no iterative scan")
	explain("no iterative scan", false)
}

func ids(chunks []interfaces.KnowledgeChunk) []string {
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.ID)
	}
	return out
}
