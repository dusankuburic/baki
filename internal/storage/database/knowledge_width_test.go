package database

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// widthLogProbe records every slog record while installed.
type widthLogProbe struct {
	mu      sync.Mutex
	records []slog.Record
}

func (p *widthLogProbe) Enabled(context.Context, slog.Level) bool { return true }
func (p *widthLogProbe) WithAttrs([]slog.Attr) slog.Handler       { return p }
func (p *widthLogProbe) WithGroup(string) slog.Handler            { return p }
func (p *widthLogProbe) Handle(_ context.Context, r slog.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = append(p.records, r.Clone())
	return nil
}

// warnings returns the WARN-or-worse records whose message contains substr ("" = all of them).
func (p *widthLogProbe) warnings(substr string) []slog.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []slog.Record
	for _, r := range p.records {
		if r.Level >= slog.LevelWarn && strings.Contains(r.Message, substr) {
			out = append(out, r)
		}
	}
	return out
}

func installWidthLogProbe(t *testing.T) *widthLogProbe {
	t.Helper()
	p := &widthLogProbe{}
	prev := slog.Default()
	slog.SetDefault(slog.New(p))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return p
}

// recordAttrs flattens a record's attributes to strings.
func recordAttrs(r slog.Record) map[string]string {
	out := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.String()
		return true
	})
	return out
}

// storedVectorIndexed reports whether a chunk's embedding_vec is set.
func storedVectorIndexed(t *testing.T, b *PostgresStorageBackend, id string) bool {
	t.Helper()
	var indexed bool
	if err := b.db.QueryRowContext(context.Background(), `SELECT embedding_vec IS NOT NULL FROM knowledge_chunks WHERE id = $1`, id).Scan(&indexed); err != nil {
		t.Fatalf("read embedding_vec of %s: %v", id, err)
	}
	return indexed
}

// TestInVectorIndex (R-config-8): the one width rule of the write and search paths — a vector is in the index
// only at the configured width, and an empty vector never is.
func TestInVectorIndex(t *testing.T) {
	for _, tc := range []struct {
		configured, observed int
		want                 bool
	}{
		{1536, 1536, true},
		{1536, 3072, false},
		{1536, 768, false},
		{1536, 0, false},
		{3, 3, true},
		{0, 0, false},
	} {
		if got := inVectorIndex(tc.configured, tc.observed); got != tc.want {
			t.Errorf("inVectorIndex(%d, %d) = %v, want %v", tc.configured, tc.observed, got, tc.want)
		}
	}
}

// TestKnowledgeWidthMismatch_OutOfIndexAndOnFallback_NoPerChunkWarning (R-config-8, Postgres): chunks at a width
// other than the configured one are stored as JSON only (embedding_vec NULL, not counted as indexed), a search at
// their width still finds them (the Go fallback) while one at the configured width takes the pgvector path — told
// from the fallback by a chunk at the configured width in JSON alone, which only the fallback's JSON scan would
// return — and the storage layer logs none of it per chunk or per search: the rag layer warns once per org.
func TestKnowledgeWidthMismatch_OutOfIndexAndOnFallback_NoPerChunkWarning(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t) // configured width 3
	if !b.HasPgvector() {
		t.Skip("pgvector not installed — the vector column is the subject")
	}
	ctx := context.Background()
	const orgID, docID = "width-org", "width-doc"
	seedOrgRow(t, b.db, orgID)
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = $1`, docID)
		_, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID)
	})
	if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "width.md"}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	probe := installWidthLogProbe(t)
	if err := b.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{
		{ID: "width-w1", DocID: docID, OrgID: orgID, Content: "four wide", Embedding: []float32{1, 0, 0, 0}},
		{ID: "width-w2", DocID: docID, OrgID: orgID, Content: "four wide too", Embedding: []float32{0.9, 0.1, 0, 0}},
		{ID: "width-w3", DocID: docID, OrgID: orgID, Content: "four wide again", Embedding: []float32{0.8, 0.2, 0, 0}},
	}); err != nil {
		t.Fatalf("SaveKnowledgeChunks: %v", err)
	}
	if err := b.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{
		{ID: "width-n1", DocID: docID, OrgID: orgID, Content: "three wide", Embedding: []float32{1, 0, 0}},
	}); err != nil {
		t.Fatalf("SaveKnowledgeChunks (configured width): %v", err)
	}
	if err := b.UpdateKnowledgeChunkEmbeddings(ctx, "", []interfaces.KnowledgeChunk{
		{ID: "width-w1", DocID: docID, OrgID: orgID, Embedding: []float32{1, 0, 0, 0}},
	}); err != nil {
		t.Fatalf("UpdateKnowledgeChunkEmbeddings: %v", err)
	}
	// A chunk at the configured width in JSON alone — written before pgvector was installed, not backfilled.
	// The vector statements read embedding_vec and cannot see it; the Go fallback's jsonb_array_length scan can.
	if _, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES ($1, $2, $3, $4, $5::jsonb)`,
		"width-n0", docID, orgID, "three wide, json only", "[1,0,0]"); err != nil {
		t.Fatalf("seed the JSON-only chunk: %v", err)
	}
	for _, id := range []string{"width-w1", "width-w2", "width-w3", "width-n0"} {
		if storedVectorIndexed(t, b, id) {
			t.Errorf("%s has embedding_vec set; want NULL", id)
		}
	}
	if !storedVectorIndexed(t, b, "width-n1") {
		t.Error("width-n1 at the configured width has embedding_vec NULL")
	}
	total, indexed, err := b.CountKnowledgeChunks(ctx, orgID)
	if err != nil || total != 5 || indexed != 1 {
		t.Errorf("CountKnowledgeChunks = %d total, %d indexed, %v; want 5 and 1", total, indexed, err)
	}

	hits, err := b.SearchKnowledge(ctx, orgID, []float32{1, 0, 0, 0}, 10, "")
	if err != nil || chunkIDs(hits) != "width-w1,width-w2,width-w3" {
		t.Errorf("search at width 4 = %s, %v; want the three width-4 chunks (the Go fallback)", chunkIDs(hits), err)
	}
	hits, err = b.SearchKnowledge(ctx, orgID, []float32{1, 0, 0}, 10, "")
	if err != nil || chunkIDs(hits) != "width-n1" {
		t.Errorf("search at the configured width = %s, %v; want width-n1 only — the pgvector path; the Go fallback would return the JSON-only width-n0 too", chunkIDs(hits), err)
	}
	if warns := probe.warnings(""); len(warns) != 0 {
		msgs := make([]string, len(warns))
		for i, w := range warns {
			msgs[i] = w.Message
		}
		t.Errorf("the storage layer logged %d warnings over 5 chunk writes and 2 searches, want none (the rag layer warns once per org): %q", len(warns), msgs)
	}
}

// TestHNSWIndexable_SkipsWidthAbovePgvectorLimit (R-config-8): pgvector's HNSW on the vector type indexes at most
// 2,000 dimensions. A wider configured width gets no build attempt — it could only fail — and one boot warning
// naming both numbers, instead of a failed CREATE INDEX that only says "search stays an exact scan"; a width
// within the limit is built without a word.
func TestHNSWIndexable_SkipsWidthAbovePgvectorLimit(t *testing.T) {
	probe := installWidthLogProbe(t)
	if hnswIndexable(3072) {
		t.Error("hnswIndexable(3072) = true, want false above the limit")
	}
	warns := probe.warnings("HNSW")
	if len(warns) != 1 {
		t.Fatalf("got %d HNSW warnings, want 1", len(warns))
	}
	if attrs := recordAttrs(warns[0]); attrs["dims"] != "3072" || attrs["max_dims"] != "2000" {
		t.Errorf("warning attrs = %v, want dims 3072 and max_dims 2000", attrs)
	}
	if !hnswIndexable(1536) {
		t.Error("hnswIndexable(1536) = false, want true within the limit")
	}
	if warns := probe.warnings("HNSW"); len(warns) != 1 {
		t.Errorf("a width within the limit was warned about: %d HNSW warnings, want still 1", len(warns))
	}
}

// TestBackfillVectorColumnSQL_Shape pins the boot reconciliation statement without a database: only rows outside
// the vector index, only those whose JSON embedding is an array of the configured width (the CASE form, since
// jsonb_array_length errors on a scalar and Postgres need not short-circuit an AND), written from the JSON array
// the way the v20 repair did.
func TestBackfillVectorColumnSQL_Shape(t *testing.T) {
	q := oneLine(backfillVectorColumnSQL)
	for _, want := range []string{
		"UPDATE knowledge_chunks SET embedding_vec = ('[' || (SELECT string_agg(e, ',') FROM jsonb_array_elements_text(embedding) AS e) || ']')::vector",
		"WHERE embedding_vec IS NULL",
		"AND CASE WHEN jsonb_typeof(embedding) = 'array' THEN jsonb_array_length(embedding) ELSE 0 END = $1",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("backfill SQL lacks %q: %s", want, q)
		}
	}
	if !strings.Contains(oneLine(listKnowledgeChunkContentsSQL(true)), "AND c.embedding_vec IS NULL ORDER BY c.doc_id, c.id LIMIT $4") {
		t.Errorf("the unindexed page lacks the vector-column conjunct: %s", oneLine(listKnowledgeChunkContentsSQL(true)))
	}
	if strings.Contains(listKnowledgeChunkContentsSQL(false), "embedding_vec") {
		t.Errorf("the identity page tests the vector column, which a store without pgvector does not have: %s", listKnowledgeChunkContentsSQL(false))
	}
}

// TestEnsureVectorIndex_BackfillsJSONOnlyRowsAtTheConfiguredWidth (R-config-8, Postgres): rows at the configured
// width stored as JSON only — written at another PAD_EMBEDDING_DIM, or before pgvector was installed — join the
// vector column at boot and the vector search sees them; rows at another width stay out; the re-index's unindexed
// page and the listing's width probe see exactly the rows outside the index before and after; a second run has
// nothing left to convert.
func TestEnsureVectorIndex_BackfillsJSONOnlyRowsAtTheConfiguredWidth(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t) // configured width 3
	if !b.HasPgvector() {
		t.Skip("pgvector not installed — the vector column is the subject")
	}
	ctx := context.Background()
	const orgID, docID = "backfill-org", "backfill-doc"
	seedOrgRow(t, b.db, orgID)
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = $1`, docID)
		_, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID)
		_, _ = b.db.ExecContext(ctx, `DROP INDEX IF EXISTS `+knowledgeVectorIndexName(3))
	})
	if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "backfill.md"}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	// JSON-only rows, as another configured width or a pre-pgvector install left them: two at the configured
	// width, one at another; and a row the write path indexed.
	for _, c := range []struct{ id, emb string }{{"backfill-fits-1", "[1,0,0]"}, {"backfill-fits-2", "[0,1,0]"}, {"backfill-wide", "[1,0,0,0]"}} {
		if _, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES ($1, $2, $3, $4, $5::jsonb)`,
			c.id, docID, orgID, "text of "+c.id, c.emb); err != nil {
			t.Fatalf("seed %s: %v", c.id, err)
		}
	}
	if err := b.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{
		{ID: "backfill-indexed", DocID: docID, OrgID: orgID, Content: "indexed", Embedding: []float32{0, 0, 1}},
	}); err != nil {
		t.Fatalf("SaveKnowledgeChunks: %v", err)
	}
	page, err := b.ListUnindexedKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0)
	if err != nil || chunkIDs(page) != "backfill-fits-1,backfill-fits-2,backfill-wide" {
		t.Errorf("unindexed page before the boot = %s, %v; want the three JSON-only rows", chunkIDs(page), err)
	}
	if width, err := b.UnindexedKnowledgeChunkWidth(ctx, orgID); err != nil || (width != 3 && width != 4) {
		t.Errorf("UnindexedKnowledgeChunkWidth before the boot = %d, %v; want the width of one JSON-only row (3 or 4)", width, err)
	}

	if err := b.EnsureVectorIndex(ctx, 3); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}
	for _, id := range []string{"backfill-fits-1", "backfill-fits-2", "backfill-indexed"} {
		if !storedVectorIndexed(t, b, id) {
			t.Errorf("%s at the configured width has embedding_vec NULL after the boot", id)
		}
	}
	if storedVectorIndexed(t, b, "backfill-wide") {
		t.Error("backfill-wide at another width has embedding_vec set after the boot")
	}
	if total, indexed, err := b.CountKnowledgeChunks(ctx, orgID); err != nil || total != 4 || indexed != 3 {
		t.Errorf("CountKnowledgeChunks = %d total, %d indexed, %v; want 4 and 3", total, indexed, err)
	}
	page, err = b.ListUnindexedKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0)
	if err != nil || chunkIDs(page) != "backfill-wide" {
		t.Errorf("unindexed page after the boot = %s, %v; want the width-4 row alone", chunkIDs(page), err)
	}
	if width, err := b.UnindexedKnowledgeChunkWidth(ctx, orgID); err != nil || width != 4 {
		t.Errorf("UnindexedKnowledgeChunkWidth after the boot = %d, %v; want 4", width, err)
	}
	// The identity page still walks every row (none carries an identity), as before.
	if all, err := b.ListKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0, ""); err != nil || len(all) != 4 {
		t.Errorf("ListKnowledgeChunkContents = %d rows, %v; want all 4", len(all), err)
	}
	// The vector search (the configured width takes the pgvector path) now sees the backfilled rows.
	hits, err := b.SearchKnowledge(ctx, orgID, []float32{1, 0, 0}, 10, "")
	if err != nil || chunkIDs(hits) != "backfill-fits-1" {
		t.Errorf("search at the configured width = %s, %v; want the backfilled backfill-fits-1 (the others are orthogonal)", chunkIDs(hits), err)
	}
	if err := b.EnsureVectorIndex(ctx, 3); err != nil {
		t.Fatalf("EnsureVectorIndex (again): %v", err)
	}
	if total, indexed, err := b.CountKnowledgeChunks(ctx, orgID); err != nil || total != 4 || indexed != 3 {
		t.Errorf("CountKnowledgeChunks after the second boot = %d total, %d indexed, %v; want 4 and 3 unchanged", total, indexed, err)
	}
}
