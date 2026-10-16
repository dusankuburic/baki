package database

import (
	"context"
	"crypto/md5" // #nosec G501 -- test data only: md5 spreads the seeded emails' first characters
	"fmt"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// Plan tests for migration 0049's indexes, each on a private database: the statement is first planned without the
// index (dropped if the schema already has it), to show the check tells the shapes apart, then with it.

// withoutThenWith0049 drops the named v49 indexes, runs before, creates them (indexes0049's statements) and refreshes
// the statistics of tables.
func withoutThenWith0049(t *testing.T, b *PostgresStorageBackend, tables string, names []string, before func()) {
	t.Helper()
	ctx := context.Background()
	for _, name := range names {
		if _, err := b.db.ExecContext(ctx, `DROP INDEX IF EXISTS `+name); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}
	if _, err := b.db.ExecContext(ctx, `ANALYZE `+tables); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	before()
	for _, name := range names {
		if _, err := b.db.ExecContext(ctx, indexes0049[name]); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if _, err := b.db.ExecContext(ctx, `ANALYZE `+tables); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// unkeyedOn reports whether some scan of relation (or of one of its indexes, named with prefix) has no index condition.
func unkeyedOn(nodes []planNode, relation, prefix string) bool {
	for _, n := range nodes {
		if (n.RelationName == relation || strings.HasPrefix(n.IndexName, prefix)) && n.IndexCond == "" &&
			n.NodeType != "Bitmap Heap Scan" && n.NodeType != "BitmapOr" && n.NodeType != "BitmapAnd" {
			return true
		}
	}
	return false
}

func hasSort(nodes []planNode) bool {
	for _, n := range nodes {
		if strings.HasSuffix(n.NodeType, "Sort") {
			return true
		}
	}
	return false
}

// TestPlan_ListKnowledgeDocuments_ReadsThePageInIndexOrder: the knowledge document list is read in
// knowledge_documents_org_created_idx's order, newest first with the id breaking ties, so no Sort; the (org_id) and
// (org_id, filename) indexes found the organisation's rows and sorted them.
func TestPlan_ListKnowledgeDocuments_ReadsThePageInIndexOrder(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) SELECT 'k49-org-' || g, 'Org', '' FROM generate_series(1, 50) g`,
		`INSERT INTO knowledge_documents (id, org_id, filename, created_at)
			SELECT 'k49-kd-' || g || '-' || k, 'k49-org-' || g, 'f' || k || '.md', NOW() - (k || ' minutes')::interval
			FROM generate_series(1, 50) g, generate_series(1, 40) k`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	q := listKnowledgeDocumentsSQL(b.hasPgvector)
	args := []any{"k49-org-7", 10, 0}
	withoutThenWith0049(t, b, "knowledge_documents", []string{"knowledge_documents_org_created_idx"}, func() {
		if nodes := explainAsUser(t, b, "", q, args); !hasSort(nodes) {
			t.Errorf("without the index the page is not sorted; the check below proves nothing:\n%s", describePlan(nodes))
		}
	})
	nodes := explainAsUser(t, b, "", q, args)
	t.Logf("ListKnowledgeDocuments plan:\n%s", describePlan(nodes))
	assertIndexUsed(t, nodes, "knowledge_documents_org_created_idx", "ListKnowledgeDocuments")
	assertKeyedAccess(t, nodes, "knowledge_documents", "ListKnowledgeDocuments")
	if hasSort(nodes) {
		t.Errorf("ListKnowledgeDocuments still sorts:\n%s", describePlan(nodes))
	}
}

// TestPlan_QueryUsers_SearchUsesTrigramIndexes: the admin user search probes the trigram indexes on lower(email) and
// lower(display_name), for the count and the page, with a substring of three characters or more and with a shorter
// prefix; without them it read every account.
func TestPlan_QueryUsers_SearchUsesTrigramIndexes(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	if _, err := b.db.ExecContext(context.Background(), `INSERT INTO users (id, email, password, role, display_name)
		SELECT 'u49-' || g, md5(g::text) || '@example.com', 'x', 'member', 'Name ' || g FROM generate_series(1, 3000) g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sum := fmt.Sprintf("%x", md5.Sum([]byte("1234"))) // #nosec G401 -- test data
	trgm := []string{"users_email_trgm_idx", "users_display_name_trgm_idx"}
	queries := []struct {
		what string
		q    interfaces.UserQuery
	}{
		{"substring", interfaces.UserQuery{Q: sum[3:11], Limit: 50}},
		{"short prefix", interfaces.UserQuery{Q: sum[:2], Limit: 50}},
		{"substring with a role", interfaces.UserQuery{Q: sum[3:11], Role: "member", Limit: 50}},
	}
	withoutThenWith0049(t, b, "users", trgm, func() {
		cq, cargs, _, _ := queryUsersSQL(queries[0].q)
		if nodes := explainAsUser(t, b, "", cq, cargs); !unkeyedOn(nodes, "users", "users_") {
			t.Errorf("without the trigram indexes the search is keyed; the check below proves nothing:\n%s", describePlan(nodes))
		}
	})
	for _, tc := range queries {
		cq, cargs, pq, pargs := queryUsersSQL(tc.q)
		for _, st := range []struct {
			name string
			q    string
			args []any
		}{{"count", cq, cargs}, {"page", pq, pargs}} {
			nodes := explainAsUser(t, b, "", st.q, st.args)
			what := fmt.Sprintf("QueryUsers %s, %s", tc.what, st.name)
			if unkeyedOn(nodes, "users", "users_") {
				t.Errorf("%s: users is read unkeyed:\n%s", what, describePlan(nodes))
			}
			for _, ix := range trgm {
				assertIndexUsed(t, nodes, ix, what)
			}
		}
	}
}

// TestPlan_LockedAccountCount_UsesThePartialIndex: the dashboard's and the analytics page's locked-account count
// probes users_locked_until_idx for the accounts locked now; without it the count read every account.
func TestPlan_LockedAccountCount_UsesThePartialIndex(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	if _, err := b.db.ExecContext(context.Background(), `INSERT INTO users (id, email, password, role, locked_until)
		SELECT 'l49-' || g, 'l49-' || g || '@example.com', 'x', 'member',
		       CASE WHEN g % 500 = 0 THEN NOW() + INTERVAL '10 minutes' WHEN g % 100 = 0 THEN NOW() - INTERVAL '1 day' END
		FROM generate_series(1, 3000) g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	withoutThenWith0049(t, b, "users", []string{"users_locked_until_idx"}, func() {
		if nodes := explainAsUser(t, b, "", lockedAccountsCountSQL, nil); !unkeyedOn(nodes, "users", "users_") {
			t.Errorf("without the partial index the count is keyed; the check below proves nothing:\n%s", describePlan(nodes))
		}
	})
	nodes := explainAsUser(t, b, "", lockedAccountsCountSQL, nil)
	assertIndexUsed(t, nodes, "users_locked_until_idx", "locked-account count")
	if unkeyedOn(nodes, "users", "users_") {
		t.Errorf("locked-account count: users is read unkeyed:\n%s", describePlan(nodes))
	}
	var n int
	if err := b.db.QueryRowContext(context.Background(), lockedAccountsCountSQL).Scan(&n); err != nil || n != 6 {
		t.Errorf("locked accounts = %d, %v; want the 6 locked now", n, err)
	}
}
