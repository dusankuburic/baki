package database

import (
	"context"
	"strings"
	"testing"
)

// Plan tests for the third performance round (v28), in the perf_round2
// pattern: the backend's statements, as the app role under RLS with sequential
// scans priced out, over a seeded population. DATABASE_URL-gated (throwaway DB).

// auditPlanUser is the account most of the seeded audit rows belong to — one
// dominant user, so neither the user nor resource index narrows the window and
// the counter's only selective key is the action.
const auditPlanUser = "r3-plan-user-main"

// seedAuditPopulation writes 3000 audit rows spread over the last 3000
// minutes — 95% for auditPlanUser, 2% of them failed logins — and ANALYZEs.
func seedAuditPopulation(t *testing.T, b *PostgresStorageBackend) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM audit_events WHERE id LIKE 'r3-plan-%'`) })
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, user_id, action, resource_type, resource_id, created_at)
		SELECT 'r3-plan-' || g,
		       CASE WHEN g % 20 = 0 THEN 'r3-plan-user-' || (g % 7) ELSE $1 END,
		       CASE WHEN g % 50 = 0 THEN 'auth.login_failure' ELSE 'flow.save' END,
		       'user',
		       CASE WHEN g % 20 = 0 THEN 'r3-plan-user-' || (g % 7) ELSE $1 END,
		       NOW() - (g || ' minutes')::interval
		FROM generate_series(1, 3000) g ON CONFLICT (id) DO NOTHING`, auditPlanUser); err != nil {
		t.Fatalf("seed audit population: %v", err)
	}
	if _, err := b.db.ExecContext(ctx, `ANALYZE audit_events`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// auditListByActionSQL is the statement ListAuditEvents sends for an
// action-only filter (the admin log filtered by event type).
const auditListByActionSQL = `SELECT id, user_id, email, action, resource_type, resource_id, ip, meta, created_at, org_id, actor_kind, token_id, actor_job, user_agent FROM audit_events WHERE action = $1 ORDER BY created_at DESC, id ASC LIMIT $2 OFFSET $3`

// failedLoginCountSQL is the dashboard's failed-login counter
// (GetDashboardOverview, security posture) verbatim.
const failedLoginCountSQL = `
		SELECT COUNT(*)::int FROM audit_events
		WHERE action = 'auth.login_failure'
		  AND (user_id = $1 OR (resource_type = 'user' AND resource_id = $1))
		  AND created_at >= NOW() - INTERVAL '24 hours'`

func nodeOn(nodes []planNode, relation string) (planNode, bool) {
	for _, n := range nodes {
		if n.RelationName == relation {
			return n, true
		}
	}
	return planNode{}, false
}

// TestPlan_AuditByAction_IndexOrderNoSort: the action-filtered audit list is
// served by audit_events_action_created_idx in the list's own order — no Sort
// node (the baseline action index made every page a Sort over every row).
func TestPlan_AuditByAction_IndexOrderNoSort(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t) // the plan must not depend on what earlier tests did to the table
	seedAuditPopulation(t, b)
	nodes := explainAsUser(t, b, planUser, auditListByActionSQL, []any{"flow.save", 100, 0})
	t.Logf("ListAuditEvents(action) plan:\n%s", describePlan(nodes))
	assertIndexUsed(t, nodes, "audit_events_action_created_idx", "ListAuditEvents(action)")
	assertKeyedAccess(t, nodes, "audit_events", "ListAuditEvents(action)")
	for _, n := range nodes {
		if strings.Contains(n.NodeType, "Sort") {
			t.Errorf("ListAuditEvents(action) still sorts (%s); the index must deliver the list's order:\n%s", n.NodeType, describePlan(nodes))
		}
	}
}

// TestPlan_FailedLoginCount_KeyedOnActionAndWindow: the 24-hour failed-login
// count reaches audit_events through the composite index with BOTH the action
// and the window in the index condition, not by walking either whole.
func TestPlan_FailedLoginCount_KeyedOnActionAndWindow(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seedAuditPopulation(t, b)
	nodes := explainAsUser(t, b, planUser, failedLoginCountSQL, []any{auditPlanUser})
	t.Logf("failed-login count plan:\n%s", describePlan(nodes))
	assertKeyedAccess(t, nodes, "audit_events", "failed-login count")
	for _, n := range nodes {
		if n.IndexName == "audit_events_created_at_idx" {
			t.Errorf("failed-login count walks the whole 24-hour window (%s):\n%s", n.IndexName, describePlan(nodes))
		}
	}
	assertIndexUsed(t, nodes, "audit_events_action_created_idx", "failed-login count")
	cond := ""
	for _, n := range nodes {
		if n.IndexName == "audit_events_action_created_idx" {
			cond = n.IndexCond
		}
	}
	if !strings.Contains(cond, "action") || !strings.Contains(cond, "created_at") {
		t.Errorf("failed-login count: index condition %q must key on action AND created_at:\n%s", cond, describePlan(nodes))
	}
}

// knowledgeFixture is a width-8 corpus for the knowledge plan tests: the
// vector-index fixture with the HNSW index built the way boot builds it,
// ANALYZEd.
func knowledgePlanFixture(t *testing.T) *vectorIndexFixture {
	t.Helper()
	f := newVectorIndexFixtureWith(t, vectorFixtureOpts{countA: 400, countB: 400})
	if err := f.b.EnsureVectorIndex(f.ctx, vectorIndexTestDims); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}
	if exists, valid := f.indexState(); !exists || !valid {
		t.Fatalf("HNSW index exists=%v valid=%v after EnsureVectorIndex", exists, valid)
	}
	if _, err := f.b.db.ExecContext(f.ctx, `ANALYZE knowledge_chunks, knowledge_documents, org_members`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return f
}

// chunkScanIsUncorrelated asserts the v28 policy shape on every
// knowledge_chunks scan: the policy test is a hashed (or init) sub-plan
// evaluated once per statement, with no sub-plan under the scan reading
// knowledge_documents (the baseline per-row probe).
func chunkScanIsUncorrelated(t *testing.T, nodes []planNode, what string) {
	t.Helper()
	found := false
	for _, n := range nodes {
		if n.RelationName != "knowledge_chunks" {
			continue
		}
		found = true
		filter := n.Filter // the policy test is a Filter on the (heap / index) scan node
		if !strings.Contains(filter, "hashed SubPlan") && !strings.Contains(filter, "InitPlan") {
			t.Errorf("%s: knowledge_chunks %s filter %q is not an uncorrelated (hashed/init) sub-plan:\n%s", what, n.NodeType, filter, describePlan(nodes))
		}
		if strings.Contains(filter, "knowledge_documents") {
			t.Errorf("%s: knowledge_chunks filter still names the document table: %q", what, filter)
		}
		for _, sub := range n.Plans {
			if sub.ParentRelationship != "SubPlan" && sub.ParentRelationship != "InitPlan" {
				continue
			}
			for _, d := range sub.flatten() {
				if d.RelationName == "knowledge_documents" {
					t.Errorf("%s: a sub-plan under the knowledge_chunks scan probes knowledge_documents per row (%s):\n%s", what, sub.SubplanName, describePlan(nodes))
				}
			}
		}
	}
	if !found {
		t.Errorf("%s: no knowledge_chunks scan in the plan:\n%s", what, describePlan(nodes))
	}
}

// TestPlan_KnowledgeChunks_PolicyIsHashedSubplan: under RLS, every org-scoped
// chunk read evaluates rls_knowledge_chunks_visible as one hashed sub-plan
// over the caller's memberships — not a correlated document probe per row.
func TestPlan_KnowledgeChunks_PolicyIsHashedSubplan(t *testing.T) {
	f := knowledgePlanFixture(t)
	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"CountKnowledgeChunks", `SELECT COUNT(c.id), COUNT(c.id) FILTER (WHERE c.embedding_vec IS NOT NULL) FROM knowledge_chunks c WHERE c.org_id = $1`, []any{f.orgA}},
		{"ListKnowledgeChunkContents", `SELECT c.id, c.doc_id, c.content FROM knowledge_chunks c WHERE c.org_id = $1 AND (c.doc_id, c.id) > ($2::text, $3::text) ORDER BY c.doc_id, c.id LIMIT $4`, []any{f.orgA, "", "", 2000}},
		{"exact vector search", knowledgeVectorSearchExactSQL, []any{f.orgA, FormatVector(make([]float32, vectorIndexTestDims)), 5, knowledgeMaxCosineDistance, vectorIndexTestDims}},
		{"indexed vector search", knowledgeVectorSearchSQL(vectorIndexTestDims), []any{f.orgA, FormatVector(make([]float32, vectorIndexTestDims)), 5, knowledgeMaxCosineDistance}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes := explainAsUser(t, f.b, f.user, tc.sql, tc.args)
			if scan, ok := nodeOn(nodes, "knowledge_chunks"); ok {
				t.Logf("%s: knowledge_chunks %s filter: %s", tc.name, scan.NodeType, scan.Filter)
			}
			chunkScanIsUncorrelated(t, nodes, tc.name)
		})
	}
}

// TestPlan_KnowledgeVectorSearch_HNSWWithOrgFilter: the indexed search is an
// Index Scan on knowledge_chunks_hnsw_<dims> whose filter carries the org
// predicate on the chunk row (c.org_id = $1) — no document join — and the
// document is reached only for the returned rows, outside the LIMIT, by key.
func TestPlan_KnowledgeVectorSearch_HNSWWithOrgFilter(t *testing.T) {
	f := knowledgePlanFixture(t)
	q := FormatVector(make([]float32, vectorIndexTestDims))
	nodes := explainAsUser(t, f.b, f.user, knowledgeVectorSearchSQL(vectorIndexTestDims), []any{f.orgA, q, 5, knowledgeMaxCosineDistance})
	hnsw := knowledgeVectorIndexName(vectorIndexTestDims)
	t.Logf("indexed knowledge search plan:\n%s", describePlan(nodes))
	assertIndexUsed(t, nodes, hnsw, "indexed knowledge search")
	scan, ok := nodeOn(nodes, "knowledge_chunks")
	if !ok {
		t.Fatalf("no knowledge_chunks scan:\n%s", describePlan(nodes))
	}
	if scan.IndexName != hnsw {
		t.Errorf("knowledge_chunks is reached by %s using %q, want an Index Scan on %s:\n%s", scan.NodeType, scan.IndexName, hnsw, describePlan(nodes))
	}
	// The bound org is inlined into the custom plan as a literal.
	if !strings.Contains(scan.Filter, "org_id = '"+f.orgA+"'") {
		t.Errorf("the HNSW scan filter %q must test the chunk's own org (org_id = $1):\n%s", scan.Filter, describePlan(nodes))
	}
	if strings.Contains(scan.Filter, "knowledge_documents") || strings.Contains(scan.Filter, "doc_id") {
		t.Errorf("the HNSW scan filter %q reaches the document per candidate", scan.Filter)
	}
	// The document is joined for its filename OUTSIDE the LIMIT — on the
	// returned rows, never per candidate (how the tiny join executes is the
	// planner's call).
	var limitSeen bool
	var walk func(n planNode, underLimit bool)
	walk = func(n planNode, underLimit bool) {
		if n.NodeType == "Limit" {
			underLimit, limitSeen = true, true
		}
		if underLimit && n.RelationName == "knowledge_documents" {
			t.Errorf("knowledge_documents is read inside the LIMIT (per candidate):\n%s", describePlan(nodes))
		}
		for _, c := range n.Plans {
			walk(c, underLimit)
		}
	}
	walk(nodes[0], false)
	if !limitSeen {
		t.Errorf("no Limit node in the indexed search plan:\n%s", describePlan(nodes))
	}
}

// TestPlan_OldChunkPolicyShapeIsCorrelated proves the hashed-sub-plan check
// is not vacuous: the baseline policy body over the same scan is a correlated
// sub-plan that reads knowledge_documents.
func TestPlan_OldChunkPolicyShapeIsCorrelated(t *testing.T) {
	f := knowledgePlanFixture(t)
	old := `SELECT COUNT(*) FROM knowledge_chunks c WHERE c.org_id = $1 AND EXISTS (
		SELECT 1 FROM knowledge_documents kd WHERE kd.id = c.doc_id
		  AND EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = kd.org_id AND om.user_id = app_current_user_id()))`
	nodes := explainAsUser(t, f.b, f.user, old, []any{f.orgA})
	// Semi-join pull-up or correlated SubPlan — either way the document
	// table is read in step with the chunk rows, which the v28 checks reject.
	if _, ok := nodeOn(nodes, "knowledge_documents"); !ok {
		t.Fatalf("the old correlated shape no longer reads knowledge_documents; the v28 plan tests cannot tell the shapes apart:\n%s", describePlan(nodes))
	}
}

// TestKnowledgeStatements_FilterOnChunkOrg pins, without a database, that
// every org-scoped chunk statement tests c.org_id, not the document's org.
func TestKnowledgeStatements_FilterOnChunkOrg(t *testing.T) {
	for name, q := range map[string]string{
		"exact":   knowledgeVectorSearchExactSQL,
		"indexed": knowledgeVectorSearchSQL(8),
	} {
		one := oneLine(q)
		if !strings.Contains(one, "WHERE c.org_id = $1") {
			t.Errorf("%s search lacks the chunk org predicate: %s", name, one)
		}
		if strings.Contains(one, "d.org_id") {
			t.Errorf("%s search still filters on the document's org: %s", name, one)
		}
		// The document join is outside the LIMIT.
		if i, j := strings.Index(one, "LIMIT $3"), strings.Index(one, "JOIN knowledge_documents"); i < 0 || j < i {
			t.Errorf("%s search must join knowledge_documents after the LIMIT subquery: %s", name, one)
		}
	}
}
