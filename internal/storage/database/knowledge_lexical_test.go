package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// The lexical half of hybrid retrieval (R-knowledge-8). knowledge_chunks is FORCE ROW LEVEL SECURITY and pg_trgm's
// operators are not LEAKPROOF, so no trigram index on it would ever be used: v50 keeps the chunk text in a side table
// without row-level security (knowledge_chunk_search, written by a trigger, the older chunks copied after boot), whose
// trigram index serves the lexical statement; the statement reads the chunks themselves under their policy.

// TestMigration0050_TheTriggerKeepsTheSideTable: a chunk's insert writes its row of knowledge_chunk_search, a change of
// its text rewrites it, its delete removes it.
func TestMigration0050_TheTriggerKeepsTheSideTable(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	seedOrgRow(t, b.db, "kcs-org")
	if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: "kcs-doc", OrgID: "kcs-org", Filename: "a.md"}); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{{ID: "kcs-1", DocID: "kcs-doc", Content: "Run DOS command", Embedding: []float32{1, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	side := func() (org, content string, ok bool) {
		t.Helper()
		err := b.db.QueryRowContext(ctx, `SELECT org_id, content FROM knowledge_chunk_search WHERE chunk_id = 'kcs-1'`).Scan(&org, &content)
		return org, content, err == nil
	}
	if org, content, ok := side(); !ok || org != "kcs-org" || content != "Run DOS command" {
		t.Fatalf("side row after the insert = %q %q %v; want the chunk's org (filled from its document) and text", org, content, ok)
	}
	if _, err := b.db.ExecContext(ctx, `UPDATE knowledge_chunks SET content = 'Invoke web service' WHERE id = 'kcs-1'`); err != nil {
		t.Fatal(err)
	}
	if _, content, _ := side(); content != "Invoke web service" {
		t.Errorf("side text after the update = %q", content)
	}
	if _, err := b.db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE id = 'kcs-1'`); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := side(); ok {
		t.Error("the side row outlived its chunk")
	}
}

// TestSchemaUpkeep_CopiesTheChunksStoredBeforeV50: the chunks a v49 schema held (here: rows whose side row is gone)
// are copied after boot, page by page, and a second run finds nothing to do.
func TestSchemaUpkeep_CopiesTheChunksStoredBeforeV50(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	defer func(was int) { backfillPageSize = was }(backfillPageSize)
	backfillPageSize = 7
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) VALUES ('kcs-bf-org', 'Org', '')`,
		`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('kcs-bf-doc', 'kcs-bf-org', 'a.md')`,
		`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding)
			SELECT 'kcs-bf-' || lpad(g::text, 3, '0'), 'kcs-bf-doc', 'kcs-bf-org', 'text ' || g, '[]' FROM generate_series(1, 30) g`,
		`DELETE FROM knowledge_chunk_search`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
	if err := b.SchemaUpkeep(ctx); err != nil {
		t.Fatalf("SchemaUpkeep: %v", err)
	}
	var n int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_chunk_search s JOIN knowledge_chunks c ON c.id = s.chunk_id AND c.content = s.content AND c.org_id = s.org_id`).Scan(&n); err != nil || n != 30 {
		t.Errorf("side rows matching their chunk after the upkeep = %d, %v; want 30", n, err)
	}
	if err := b.SchemaUpkeep(ctx); err != nil {
		t.Errorf("a second upkeep: %v", err)
	}
}

// TestSearchKnowledgeLexical_RespectsRowLevelSecurity: inside a request's RLS transaction the lexical statement sees
// what the knowledge_chunks policy admits, like the vector one: a member of the org gets its matching chunks, a
// stranger nothing, though the side table it probes has no policy.
func TestSearchKnowledgeLexical_RespectsRowLevelSecurity(t *testing.T) {
	f := newR2Fixture(t)
	skipIfRLSBypassedInternal(t, f.b)
	member, stranger := f.user("kblex-member"), f.user("kblex-stranger")
	orgID := f.org("kblex-org", member, map[string]string{member: "owner"})
	docID := f.id("kblex-doc")
	if err := f.b.SaveKnowledgeDocument(f.ctx, "", &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "lex.md"}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	t.Cleanup(func() { _, _ = f.b.db.ExecContext(f.ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID) })
	hit := docID + "-1"
	if err := f.b.SaveKnowledgeChunks(f.ctx, "", []interfaces.KnowledgeChunk{
		{ID: hit, DocID: docID, OrgID: orgID, Content: "Never use Run DOS command with admin rights.", Embedding: []float32{1, 0, 0}, EmbeddingIdentity: "A"},
		{ID: docID + "-2", DocID: docID, OrgID: orgID, Content: "Retry a failed web request.", Embedding: []float32{1, 0, 0}, EmbeddingIdentity: "A"},
	}); err != nil {
		t.Fatalf("SaveKnowledgeChunks: %v", err)
	}
	phrases := []string{"run dos command"}
	got, err := f.b.SearchKnowledgeLexical(f.rls(member), orgID, phrases, 10, "A")
	if err != nil || chunkIDs(got) != hit {
		t.Errorf("the member's lexical search = %s, %v; want %s", chunkIDs(got), err, hit)
	}
	got, err = f.b.SearchKnowledgeLexical(f.rls(stranger), orgID, phrases, 10, "A")
	if err != nil || len(got) != 0 {
		t.Errorf("a stranger's lexical search = %s, %v; want nothing (RLS)", chunkIDs(got), err)
	}
}

// TestSearchKnowledgeLexical_PlanUsesTheTrigramIndex: over an organisation with thousands of chunks, the lexical
// statement finds its candidates through the side table's trigram index (a bitmap probe of
// knowledge_chunk_search_trgm_idx with %>), not by testing the similarity on every chunk of the org. Run as
// the statement runs: in a transaction with the floor set as the threshold of the %> operator.
func TestSearchKnowledgeLexical_PlanUsesTheTrigramIndex(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	const orgs, docsPerOrg, chunksPerDoc = 4, 100, 50
	stmts := []string{
		fmt.Sprintf(`INSERT INTO organisations (id, name, owner_id) SELECT 'lex-org-' || g, 'Org ' || g, '' FROM generate_series(1, %d) g`, orgs),
		fmt.Sprintf(`INSERT INTO knowledge_documents (id, org_id, filename)
			SELECT 'lex-doc-' || o || '-' || d, 'lex-org-' || o, 'f' || d || '.md' FROM generate_series(1, %d) o, generate_series(1, %d) d`, orgs, docsPerOrg),
		// Random physical order: a planner that reads rows in insertion order must not look better than it is.
		fmt.Sprintf(`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding)
			SELECT 'lex-c-' || o || '-' || d || '-' || k, 'lex-doc-' || o || '-' || d, 'lex-org-' || o,
			       md5(o || '-' || d || '-' || k) || ' guideline about flows, retries and naming ' || md5(k || '-' || d || '-' || o), '[]'
			FROM generate_series(1, %d) o, generate_series(1, %d) d, generate_series(1, %d) k
			ORDER BY random()`, orgs, docsPerOrg, chunksPerDoc),
		`UPDATE knowledge_chunks SET content = content || ' Never use Run DOS command with admin rights.' WHERE id IN ('lex-c-1-3-2', 'lex-c-1-9-5')`,
		`ANALYZE organisations, knowledge_documents, knowledge_chunks, knowledge_chunk_search`,
	}
	for _, s := range stmts {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}

	phrases := []string{"run dos command", "admin rights"}
	hits, err := b.SearchKnowledgeLexical(ctx, "lex-org-1", phrases, 5, "")
	if err != nil || chunkIDs(hits) != "lex-c-1-3-2,lex-c-1-9-5" {
		t.Fatalf("SearchKnowledgeLexical = %s, %v; want the two chunks naming the action", chunkIDs(hits), err)
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range knowledgeLexicalSetup() {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `EXPLAIN (FORMAT JSON) `+knowledgeLexicalSearchSQL,
		"lex-org-1", textArray(phrases), 5, "", knowledgeMinWordSimilarity, knowledgeLexicalPerPhrase, knowledgeLexicalWindow).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var top []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &top); err != nil || len(top) == 0 {
		t.Fatalf("parse explain json: %v\n%s", err, raw)
	}
	used := false
	for _, n := range top[0].Plan.flatten() {
		if n.IndexName == "knowledge_chunk_search_trgm_idx" && strings.Contains(n.IndexCond, "%>") {
			used = true
		}
		if n.NodeType == "Seq Scan" && (n.RelationName == "knowledge_chunks" || n.RelationName == "knowledge_chunk_search") {
			t.Errorf("the lexical statement reads %s sequentially:\n%s", n.RelationName, raw)
		}
	}
	if !used {
		t.Errorf("the lexical statement does not probe knowledge_chunk_search_trgm_idx with %%>:\n%s", raw)
	}
}
