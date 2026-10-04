package database

import (
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// Tests for the fourth performance round's knowledge document reads:
// ListKnowledgeDocuments pages the documents before counting their chunks
// (listKnowledgeDocumentsSQL), and ListKnowledgeDocumentIDsByFilename
// serves the re-upload's "which versions do I replace, is anything else
// left" question without a chunk aggregate. DATABASE_URL-gated (throwaway
// DB).

// r4KnowledgeFixture seeds, through the r2 fixture, an org A (the owner a
// member) with six documents of 1..6 chunks created an hour apart (newest
// first: "f", "e", … "a") plus a second version of "c.txt", and an org B
// with one document; ANALYZEd. It returns the fixture, the two org ids and
// the ids of the two "c.txt" versions (oldest first).
func r4KnowledgeFixture(t *testing.T) (f *r2Fixture, orgA, orgB string, cVersions []string) {
	t.Helper()
	f = newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 28 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	owner := f.user("owner")
	orgA = f.org("orgA", owner, map[string]string{owner: "owner"})
	orgB = f.org("orgB", owner, map[string]string{owner: "owner"})
	var docs []string
	for i, label := range []string{"a", "b", "c", "d", "e", "f"} {
		id, _ := f.seedKnowledge(label, orgA, i+1)
		docs = append(docs, id)
	}
	// A second, newer version of c.txt with no chunks (a replace in flight).
	cNew := f.id("kdoc-c2")
	if err := f.b.SaveKnowledgeDocument(f.ctx, "", &interfaces.KnowledgeDocument{ID: cNew, OrgID: orgA, Filename: "c.txt"}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	t.Cleanup(func() { _, _ = f.b.db.ExecContext(f.ctx, `DELETE FROM knowledge_documents WHERE id = $1`, cNew) })
	docs = append(docs, cNew)
	cVersions = []string{docs[2], cNew}
	// Distinct, known creation times: the k-th seeded document is k hours
	// younger than the first.
	for k, id := range docs {
		if _, err := f.b.db.ExecContext(f.ctx, `UPDATE knowledge_documents SET created_at = NOW() - INTERVAL '1 day' + make_interval(hours => $2::int) WHERE id = $1`, id, k); err != nil {
			t.Fatalf("stamp document: %v", err)
		}
	}
	f.seedKnowledge("b-only", orgB, 3)
	if _, err := f.b.db.ExecContext(f.ctx, `ANALYZE knowledge_documents, knowledge_chunks, org_members`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return f, orgA, orgB, cVersions
}

// oldListKnowledgeDocumentsSQL is ListKnowledgeDocuments' statement before
// the round (the oracle): the whole org's chunks aggregated, then limited.
func oldListKnowledgeDocumentsSQL(pgvector bool) string {
	indexed := "0 AS indexed_count"
	if pgvector {
		indexed = "COUNT(c.id) FILTER (WHERE c.embedding_vec IS NOT NULL) AS indexed_count"
	}
	return `
		SELECT d.id, d.org_id, d.filename, d.created_at,
		       COUNT(c.id) AS chunk_count,
		       ` + indexed + `
		FROM knowledge_documents d
		LEFT JOIN knowledge_chunks c ON c.org_id = d.org_id AND c.doc_id = d.id
		WHERE d.org_id = $1
		GROUP BY d.id
		ORDER BY d.created_at DESC
		LIMIT CASE WHEN $2 > 0 THEN $2 ELSE NULL END`
}

func describeDocs(docs []*interfaces.KnowledgeDocument) string {
	var sb strings.Builder
	for _, d := range docs {
		fmt.Fprintf(&sb, "%s:%d/%d ", d.Filename, d.ChunkCount, d.VectorIndexed)
	}
	return sb.String()
}

// TestListKnowledgeDocuments_PagedLateralMatchesGroupBy: for every limit
// (all, within, beyond the org's documents) the paged list returns the
// same documents, in the same order, with the same chunk and indexed
// counts as the GROUP BY statement did — a document with no chunks
// included — and for a stranger (RLS) nothing.
func TestListKnowledgeDocuments_PagedLateralMatchesGroupBy(t *testing.T) {
	f, orgA, _, _ := r4KnowledgeFixture(t)
	for _, limit := range []int{0, 1, 3, 7, 50} {
		got, err := f.b.ListKnowledgeDocuments(f.ctx, orgA, limit, 0)
		if err != nil {
			t.Fatalf("ListKnowledgeDocuments(limit %d): %v", limit, err)
		}
		rows, err := f.b.db.QueryContext(f.ctx, oldListKnowledgeDocumentsSQL(f.b.hasPgvector), orgA, limit)
		if err != nil {
			t.Fatalf("oracle: %v", err)
		}
		var want []*interfaces.KnowledgeDocument
		func() {
			defer rows.Close()
			for rows.Next() {
				var d interfaces.KnowledgeDocument
				if err := rows.Scan(&d.ID, &d.OrgID, &d.Filename, &d.CreatedAt, &d.ChunkCount, &d.VectorIndexed); err != nil {
					t.Fatal(err)
				}
				want = append(want, &d)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		}()
		if len(want) == 0 || (limit > 0 && limit < 7 && len(want) != limit) || (limit == 0 && len(want) != 7) {
			t.Fatalf("oracle returned %d documents for limit %d (fixture wrong)", len(want), limit)
		}
		if len(got) != len(want) {
			t.Errorf("limit %d: %d documents, oracle %d:\n got %s\nwant %s", limit, len(got), len(want), describeDocs(got), describeDocs(want))
			continue
		}
		for i := range want {
			if got[i].ID != want[i].ID || got[i].ChunkCount != want[i].ChunkCount || got[i].VectorIndexed != want[i].VectorIndexed || got[i].Filename != want[i].Filename || !got[i].CreatedAt.Equal(want[i].CreatedAt) {
				t.Errorf("limit %d, row %d: got %s %d/%d, oracle %s %d/%d", limit, i, got[i].Filename, got[i].ChunkCount, got[i].VectorIndexed, want[i].Filename, want[i].ChunkCount, want[i].VectorIndexed)
			}
		}
	}
	// Newest first: the chunkless second c.txt, then f (6 chunks) … a (1).
	all, _ := f.b.ListKnowledgeDocuments(f.ctx, orgA, 0, 0)
	if got := describeDocs(all); got != "c.txt:0/0 f.txt:6/0 e.txt:5/0 d.txt:4/0 c.txt:3/0 b.txt:2/0 a.txt:1/0 " {
		t.Errorf("order / counts: %s", got)
	}
	stranger := f.user("stranger")
	if docs, err := f.b.ListKnowledgeDocuments(f.rls(stranger), orgA, 0, 0); err != nil || len(docs) != 0 {
		t.Errorf("a stranger lists %d documents, %v; want none", len(docs), err)
	}
}

// TestListKnowledgeDocumentIDsByFilename: the ids of every version of a
// filename (oldest first) with the org's document total; no ids but the
// total for an unknown name; nothing for another org's name; nothing at
// all for a stranger under RLS.
func TestListKnowledgeDocumentIDsByFilename(t *testing.T) {
	f, orgA, orgB, cVersions := r4KnowledgeFixture(t)
	ids, total, err := f.b.ListKnowledgeDocumentIDsByFilename(f.ctx, orgA, "c.txt")
	if err != nil || strings.Join(ids, ",") != strings.Join(cVersions, ",") || total != 7 {
		t.Errorf("c.txt: ids %v total %d err %v; want %v, 7", ids, total, err, cVersions)
	}
	ids, total, err = f.b.ListKnowledgeDocumentIDsByFilename(f.ctx, orgA, "nope.txt")
	if err != nil || len(ids) != 0 || total != 7 {
		t.Errorf("unknown name: ids %v total %d err %v; want none, 7", ids, total, err)
	}
	ids, total, err = f.b.ListKnowledgeDocumentIDsByFilename(f.ctx, orgB, "a.txt")
	if err != nil || len(ids) != 0 || total != 1 {
		t.Errorf("another org: ids %v total %d err %v; want none, 1", ids, total, err)
	}
	ids, total, err = f.b.ListKnowledgeDocumentIDsByFilename(f.ctx, "r4-no-such-org", "a.txt")
	if err != nil || len(ids) != 0 || total != 0 {
		t.Errorf("no such org: ids %v total %d err %v; want none, 0", ids, total, err)
	}
	stranger := f.user("stranger")
	ids, total, err = f.b.ListKnowledgeDocumentIDsByFilename(f.rls(stranger), orgA, "c.txt")
	if err != nil || len(ids) != 0 || total != 0 {
		t.Errorf("stranger under RLS: ids %v total %d err %v; want none, 0", ids, total, err)
	}
}

// TestPlan_ListKnowledgeDocuments_ChunksCountedPerPagedDocument: as a member
// under RLS, the document list's chunk counts are keyed probes for the
// documents the LIMIT lets through (the knowledge_chunks scan sits outside
// the Limit) — nothing hashes or aggregates the org's whole chunk set.
func TestPlan_ListKnowledgeDocuments_ChunksCountedPerPagedDocument(t *testing.T) {
	f, orgA, _, _ := r4KnowledgeFixture(t)
	owner := f.id("owner")
	for _, limit := range []int{5, 0} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			nodes := explainAsUser(t, f.b, owner, listKnowledgeDocumentsSQL(f.b.hasPgvector), []any{orgA, limit})
			t.Logf("ListKnowledgeDocuments(limit %d) plan:\n%s", limit, describePlan(nodes))
			scan, ok := nodeOn(nodes, "knowledge_chunks")
			if !ok {
				t.Fatalf("no knowledge_chunks scan:\n%s", describePlan(nodes))
			}
			// The probe is an Index Scan, or (as the fixture grows across
			// runs) a Bitmap Heap Scan whose Bitmap Index Scan child carries
			// the condition and the index name — read whichever holds them.
			indexCond, indexName := scan.IndexCond, scan.IndexName
			if scan.NodeType == "Bitmap Heap Scan" {
				for _, c := range scan.Plans {
					// The heap scan's children also hold the RLS sub-plans
					// (keyed probes of org_members); only the Bitmap Index
					// Scan carries the chunk index's condition.
					if c.NodeType == "Bitmap Index Scan" && c.IndexCond != "" {
						indexCond, indexName = c.IndexCond, c.IndexName
					}
				}
			}
			if !strings.Contains(indexCond, "doc_id") {
				t.Errorf("knowledge_chunks index condition %q must key on doc_id:\n%s", indexCond, describePlan(nodes))
			}
			if indexName != "knowledge_chunks_org_doc_idx" && indexName != "knowledge_chunks_doc_id_idx" {
				t.Errorf("knowledge_chunks is reached through %q, want the (org, doc) or the doc_id index:\n%s", indexName, describePlan(nodes))
			}
			assertKeyedAccess(t, nodes, "knowledge_chunks", "ListKnowledgeDocuments")
			limitSeen, chunksUnderLimit := limitScope(nodes[0], "knowledge_chunks")
			if limit > 0 && !limitSeen {
				t.Errorf("no Limit node in the plan:\n%s", describePlan(nodes))
			}
			if chunksUnderLimit {
				t.Errorf("knowledge_chunks is read inside the LIMIT (the org's chunks are aggregated before the page is cut):\n%s", describePlan(nodes))
			}
			for _, n := range nodes {
				if strings.Contains(n.NodeType, "Hash") && n.NodeType != "Hash" { // Hash Join / Hash Right Join / HashAggregate; the policy's hashed SubPlan is a filter, not a node
					t.Errorf("plan has a %s node over the org's chunks:\n%s", n.NodeType, describePlan(nodes))
				}
			}
		})
	}
}

// TestPlan_OldListKnowledgeDocumentsAggregatesEveryChunk proves the check
// above observes the planner: the GROUP BY statement reads knowledge_chunks
// inside the LIMIT (or hashes the whole org's chunks) on the same fixture.
func TestPlan_OldListKnowledgeDocumentsAggregatesEveryChunk(t *testing.T) {
	f, orgA, _, _ := r4KnowledgeFixture(t)
	nodes := explainAsUser(t, f.b, f.id("owner"), oldListKnowledgeDocumentsSQL(f.b.hasPgvector), []any{orgA, 5})
	if _, under := limitScope(nodes[0], "knowledge_chunks"); under {
		return
	}
	for _, n := range nodes {
		if strings.Contains(n.NodeType, "Hash") && n.NodeType != "Hash" {
			return
		}
	}
	t.Fatalf("the GROUP BY shape no longer aggregates the org's chunks inside the LIMIT; the paged plan test cannot tell the shapes apart:\n%s", describePlan(nodes))
}

// TestPlan_KnowledgeDocumentIDsByFilename_KeyedOnBothColumns: the ids are a
// keyed probe of knowledge_documents_org_filename_idx on org AND filename;
// the total an index-keyed walk of the org's entries — no sequential scan,
// no chunk access.
func TestPlan_KnowledgeDocumentIDsByFilename_KeyedOnBothColumns(t *testing.T) {
	f, orgA, _, _ := r4KnowledgeFixture(t)
	nodes := explainAsUser(t, f.b, f.id("owner"), knowledgeDocumentIDsByFilenameSQL, []any{orgA, "c.txt"})
	t.Logf("ListKnowledgeDocumentIDsByFilename plan:\n%s", describePlan(nodes))
	assertKeyedAccess(t, nodes, "knowledge_documents", "ListKnowledgeDocumentIDsByFilename")
	assertIndexUsed(t, nodes, "knowledge_documents_org_filename_idx", "ListKnowledgeDocumentIDsByFilename")
	byName := false
	for _, n := range nodes {
		if n.RelationName == "knowledge_documents" && strings.Contains(n.IndexCond, "filename") && strings.Contains(n.IndexCond, "org_id") {
			byName = true
		}
		if n.RelationName == "knowledge_chunks" {
			t.Errorf("the ids-by-filename read touches knowledge_chunks:\n%s", describePlan(nodes))
		}
	}
	if !byName {
		t.Errorf("no knowledge_documents probe keyed on org_id AND filename:\n%s", describePlan(nodes))
	}
}
