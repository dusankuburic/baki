package database

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// Semantic test for the v28 knowledge_chunks policy rewrite: the policy now
// tests the chunk's own org_id against the caller's memberships instead of
// probing the parent document per row. DATABASE_URL-gated (throwaway DB).

// chunkIDsVisible lists which of ids the statements on ctx can read.
func (f *r2Fixture) chunkIDsVisible(ctx context.Context, ids []string) []string {
	f.t.Helper()
	rows, err := f.b.query(ctx).QueryContext(ctx, `SELECT id FROM knowledge_chunks WHERE id = ANY($1)`, ids)
	if err != nil {
		f.t.Fatalf("select chunks: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return sortedCopy(out)
}

// oldVisibleChunks evaluates the baseline policy body (document, then
// membership) WITHOUT row-level security, as the oracle.
func (f *r2Fixture) oldVisibleChunks(userID string, ids []string) []string {
	f.t.Helper()
	rows, err := f.b.db.QueryContext(f.ctx, `
		SELECT c.id FROM knowledge_chunks c
		WHERE c.id = ANY($1)
		  AND EXISTS (SELECT 1 FROM knowledge_documents kd WHERE kd.id = c.doc_id
		              AND EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = kd.org_id AND om.user_id = $2))`, ids, userID)
	if err != nil {
		f.t.Fatalf("oracle: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return sortedCopy(out)
}

// seedKnowledge writes a document with n chunks for orgID through the
// backend (pool: no RLS) and returns the chunk ids; removed at cleanup.
func (f *r2Fixture) seedKnowledge(label, orgID string, n int) (docID string, ids []string) {
	f.t.Helper()
	docID = f.id("kdoc-" + label)
	if err := f.b.SaveKnowledgeDocument(f.ctx, "", &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: label + ".txt"}); err != nil {
		f.t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	f.t.Cleanup(func() { _, _ = f.b.db.ExecContext(f.ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID) })
	chunks := make([]interfaces.KnowledgeChunk, 0, n)
	for i := 0; i < n; i++ {
		id := f.id("kchunk-" + label + "-" + string(rune('a'+i)))
		ids = append(ids, id)
		chunks = append(chunks, interfaces.KnowledgeChunk{ID: id, DocID: docID, OrgID: orgID, Content: "c", Embedding: []float32{1, 0, 0}})
	}
	if err := f.b.SaveKnowledgeChunks(f.ctx, "", chunks); err != nil {
		f.t.Fatalf("SaveKnowledgeChunks: %v", err)
	}
	return docID, sortedCopy(ids)
}

// TestPolicy_V28_KnowledgeChunks_VisibilityParity: a member of org A sees
// exactly A's chunks (as the baseline document-probing policy allowed), a
// member of B exactly B's, a stranger none, and the system context (no RLS)
// all — and the WITH CHECK side refuses a write into an org the caller is
// not a member of, while a row written without an org inherits its
// document's.
func TestPolicy_V28_KnowledgeChunks_VisibilityParity(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 28 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	ownerA, memberA, memberB, stranger := f.user("ownerA"), f.user("memberA"), f.user("memberB"), f.user("stranger")
	orgA := f.org("orgA", ownerA, map[string]string{ownerA: "owner", memberA: "member"})
	orgB := f.org("orgB", memberB, map[string]string{memberB: "owner"})
	docA, idsA := f.seedKnowledge("a", orgA, 3)
	_, idsB := f.seedKnowledge("b", orgB, 2)
	all := sortedCopy(append(append([]string{}, idsA...), idsB...))

	// The column really carries the org (the writer set it).
	var blank int
	if err := f.b.db.QueryRowContext(f.ctx, `SELECT count(*) FROM knowledge_chunks WHERE id = ANY($1) AND org_id = ''`, all).Scan(&blank); err != nil {
		t.Fatal(err)
	}
	if blank != 0 {
		t.Fatalf("%d seeded chunks have a blank org_id", blank)
	}

	want := map[string][]string{ownerA: idsA, memberA: idsA, memberB: idsB, stranger: nil}
	for _, uid := range []string{ownerA, memberA, memberB, stranger} {
		oracle := f.oldVisibleChunks(uid, all)
		if strings.Join(oracle, ",") != strings.Join(sortedCopy(want[uid]), ",") {
			t.Fatalf("%s: baseline predicate sees %v, want %v (fixture wrong)", uid, oracle, want[uid])
		}
		got := f.chunkIDsVisible(f.rls(uid), all)
		if strings.Join(got, ",") != strings.Join(oracle, ",") {
			t.Errorf("%s under RLS sees %v, want (baseline semantics) %v", uid, got, oracle)
		}
	}
	if got := f.chunkIDsVisible(f.ctx, all); strings.Join(got, ",") != strings.Join(all, ",") {
		t.Errorf("system context sees %v, want all %v", got, all)
	}

	// WITH CHECK: memberA cannot write a chunk into org B, even for A's own
	// document (the org on the row is what the policy tests); memberA can
	// write into A. Each in its own RLS transaction (an error aborts it).
	foreign := interfaces.KnowledgeChunk{ID: f.id("kchunk-foreign"), DocID: docA, OrgID: orgB, Content: "x", Embedding: []float32{1, 0, 0}}
	t.Cleanup(func() {
		_, _ = f.b.db.ExecContext(f.ctx, `DELETE FROM knowledge_chunks WHERE id LIKE $1`, "r2-kchunk-%-"+f.run)
	})
	if err := f.b.SaveKnowledgeChunks(f.rls(memberA), "", []interfaces.KnowledgeChunk{foreign}); err == nil {
		t.Error("memberA wrote a chunk carrying org B; WITH CHECK is not enforced")
	}
	// The admitted writes run in one RLS transaction as memberA that IS
	// committed, so the pool reads below (and the stranger's RLS read) see
	// the rows.
	txA, err := f.b.BeginRLS(f.ctx, memberA)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	ctxA := WithRLSTx(f.ctx, txA)
	own := interfaces.KnowledgeChunk{ID: f.id("kchunk-own"), DocID: docA, OrgID: orgA, Content: "x", Embedding: []float32{1, 0, 0}}
	if err := f.b.SaveKnowledgeChunks(ctxA, "", []interfaces.KnowledgeChunk{own}); err != nil {
		t.Errorf("memberA's write into org A refused: %v", err)
	}

	// A chunk saved WITHOUT an org (older tooling) inherits its document's,
	// both through SaveKnowledgeChunks (Go resolves it) and through a raw
	// insert (the v28 trigger), and is then subject to the policy as usual.
	inherit := interfaces.KnowledgeChunk{ID: f.id("kchunk-inherit"), DocID: docA, Content: "x", Embedding: []float32{1, 0, 0}}
	if err := f.b.SaveKnowledgeChunks(ctxA, "", []interfaces.KnowledgeChunk{inherit}); err != nil {
		t.Fatalf("SaveKnowledgeChunks without org: %v", err)
	}
	rawID := f.id("kchunk-raw")
	if _, err := txA.ExecContext(f.ctx, `INSERT INTO knowledge_chunks (id, doc_id, content, embedding) VALUES ($1, $2, 'x', '[1,0,0]'::jsonb)`, rawID, docA); err != nil {
		t.Fatalf("raw insert without org: %v", err)
	}
	if err := txA.Commit(); err != nil {
		t.Fatalf("commit memberA's writes: %v", err)
	}
	for _, id := range []string{own.ID, inherit.ID, rawID} {
		var gotOrg string
		if err := f.b.db.QueryRowContext(f.ctx, `SELECT org_id FROM knowledge_chunks WHERE id = $1`, id).Scan(&gotOrg); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if gotOrg != orgA {
			t.Errorf("%s org_id = %q, want the document's org %q", id, gotOrg, orgA)
		}
	}
	// And the stranger still cannot see them.
	if got := f.chunkIDsVisible(f.rls(stranger), []string{inherit.ID, rawID}); len(got) != 0 {
		t.Errorf("stranger sees inherited-org chunks %v", got)
	}
}

// TestV28_OrgScopedChunkReadsAgree: CountKnowledgeChunks,
// ListKnowledgeChunkContents and the samples — all now keyed on
// knowledge_chunks.org_id — return exactly the org's chunks, as the
// document join did.
func TestV28_OrgScopedChunkReadsAgree(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 28 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	owner := f.user("owner")
	orgA, orgB := f.org("orgA", owner, nil), f.org("orgB", owner, nil)
	_, idsA := f.seedKnowledge("a", orgA, 4)
	_, idsB := f.seedKnowledge("b", orgB, 2)

	total, _, err := f.b.CountKnowledgeChunks(f.ctx, orgA)
	if err != nil || total != len(idsA) {
		t.Errorf("CountKnowledgeChunks(A) = %d, %v; want %d", total, err, len(idsA))
	}
	page, err := f.b.ListKnowledgeChunkContents(f.ctx, orgA, interfaces.KnowledgeChunkCursor{}, 0, "")
	if err != nil {
		t.Fatalf("ListKnowledgeChunkContents: %v", err)
	}
	var got []string
	for _, c := range page {
		got = append(got, c.ID)
	}
	if strings.Join(sortedCopy(got), ",") != strings.Join(idsA, ",") {
		t.Errorf("ListKnowledgeChunkContents(A) = %v, want %v", got, idsA)
	}
	sample, err := f.b.SampleKnowledgeChunkEmbeddings(f.ctx, orgB, 10)
	if err != nil {
		t.Fatalf("SampleKnowledgeChunkEmbeddings: %v", err)
	}
	for _, c := range sample {
		if !strings.Contains(strings.Join(idsB, ","), c.ID) {
			t.Errorf("sample for B returned %s, not one of B's chunks %v", c.ID, idsB)
		}
	}
	if len(sample) != 1 { // one per document, B has one document
		t.Errorf("sample for B returned %d chunks, want 1 (one per document)", len(sample))
	}
	docs, err := f.b.ListKnowledgeDocuments(f.ctx, orgA, 0, 0)
	if err != nil || len(docs) != 1 || docs[0].ChunkCount != len(idsA) {
		t.Errorf("ListKnowledgeDocuments(A) = %+v, %v; want one document with %d chunks", docs, err, len(idsA))
	}
}
