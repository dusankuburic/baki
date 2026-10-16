package database

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Plan tests for the library page (ListFlowsPage, v49): the page reads one row more than it holds instead of riding
// a COUNT(*) OVER() over every visible flow, the count stops at the cap, and a keyset page starts at its position.
// DATABASE_URL-gated, on a private database.

// libraryBigOrg is an organisation of planUser's holding libraryBigOrgFlows flows: a library deep enough that a
// window count over it, or a count without a cap, shows in the plan.
const (
	libraryBigOrg      = "plan-org-big"
	libraryBigOrgFlows = 2000
)

func seedLibraryPagePopulation(t *testing.T, b *PostgresStorageBackend) {
	t.Helper()
	seedPlanPopulation(t, b)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) VALUES ('` + libraryBigOrg + `', 'Big org', 'plan-org-owner')`,
		`INSERT INTO org_members (org_id, user_id, role) VALUES ('` + libraryBigOrg + `', '` + planUser + `', 'member')`,
		`INSERT INTO flows (id, name, content, owner_id, org_id, updated_at)
			SELECT 'plan-big-' || g, 'Big flow ' || g, '{}'::jsonb, 'plan-org-owner', '` + libraryBigOrg + `', NOW() - (g || ' minutes')::interval
			FROM generate_series(1, 2000) g`,
		`ANALYZE flows, org_members, organisations`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
}

// libraryPageFilter is the library's scope=all page for planUser, his organisations included.
func libraryPageFilter() interfaces.FlowFilter {
	f := libraryPlanFilter(planUser)
	f.OrganizationIDs = append(f.OrganizationIDs, libraryBigOrg)
	return f
}

// analyzedNode is a node of EXPLAIN (ANALYZE, FORMAT JSON).
type analyzedNode struct {
	NodeType   string         `json:"Node Type"`
	ActualRows float64        `json:"Actual Rows"`
	Plans      []analyzedNode `json:"Plans"`
}

// explainAnalyzeAsUser runs EXPLAIN (ANALYZE, FORMAT JSON) of q in a fresh RLS transaction for userID (rolled back)
// and returns the plan's root.
func explainAnalyzeAsUser(t *testing.T, b *PostgresStorageBackend, userID, q string, args []any) analyzedNode {
	t.Helper()
	ctx := context.Background()
	tx, err := b.BeginRLS(ctx, userID)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var raw string
	if err := tx.QueryRowContext(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+q, args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN ANALYZE: %v\n%s", err, q)
	}
	var top []struct {
		Plan analyzedNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &top); err != nil || len(top) == 0 {
		t.Fatalf("parse explain json: %v\n%s", err, raw)
	}
	return top[0].Plan
}

func hasNode(nodes []planNode, nodeType string) bool {
	for _, n := range nodes {
		if n.NodeType == nodeType {
			return true
		}
	}
	return false
}

// TestPlan_LibraryPage_NoWindowCountOverTheLibrary: the page statement has no WindowAgg (the pre-v49 page computed
// COUNT(*) OVER() over every flow the caller sees before its LIMIT), and it reaches flows through indexes for every
// library filter shape, keyset pages included.
func TestPlan_LibraryPage_NoWindowCountOverTheLibrary(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seedLibraryPagePopulation(t, b)
	const user = planUser

	// The pre-v49 page: the list statement with the window count riding every row.
	oldQ, oldArgs := listFlowsSQL(libraryPageFilter())
	oldQ = strings.Replace(oldQ, "version, tags\n", "version, tags, COUNT(*) OVER() AS page_total\n", 1)
	if old := explainAsUser(t, b, user, oldQ, oldArgs); !hasNode(old, "WindowAgg") {
		t.Fatalf("the pre-v49 page has no WindowAgg; the check below proves nothing:\n%s\n%s", describePlan(old), oldQ)
	}

	cursor := &interfaces.PageCursor{At: time.Now().Add(-500 * time.Minute), ID: "plan-big-500"}
	cases := map[string]interfaces.FlowFilter{
		"first page": libraryPageFilter(),
		"deep offset": func() interfaces.FlowFilter {
			f := libraryPageFilter()
			f.Offset = 1500
			return f
		}(),
		"keyset, newest first": func() interfaces.FlowFilter {
			f := libraryPageFilter()
			f.After = cursor
			return f
		}(),
		"keyset, oldest first": func() interfaces.FlowFilter {
			f := libraryPageFilter()
			f.SortBy, f.After = interfaces.FlowSortUpdatedAsc, cursor
			return f
		}(),
		"keyset with a name query": func() interfaces.FlowFilter {
			f := libraryPageFilter()
			f.Query, f.After = "flow", cursor
			return f
		}(),
	}
	for name, f := range cases {
		q, args := listFlowsPageSQL(f)
		nodes := explainAsUser(t, b, user, q, args)
		t.Logf("ListFlowsPage %s plan:\n%s", name, describePlan(nodes))
		if hasNode(nodes, "WindowAgg") {
			t.Errorf("%s: the page computes a window count:\n%s", name, describePlan(nodes))
		}
		assertNoSeqScan(t, nodes, "flows", "ListFlowsPage "+name)
		cq, cargs := countFlowsCappedSQL(f, 10000)
		assertNoSeqScan(t, explainAsUser(t, b, user, cq, cargs), "flows", "ListFlowsPage count "+name)
	}
}

// TestPlan_LibraryPage_CountStopsAtTheCap: over 2000 visible flows the capped count reads cap+1 of them and stops
// (the window count counted all 2000 on every page), the page reports the capped total, and its cursor continues the
// order.
func TestPlan_LibraryPage_CountStopsAtTheCap(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seedLibraryPagePopulation(t, b)
	const user = planUser
	f := libraryPageFilter()

	cq, cargs := countFlowsCappedSQL(f, 100)
	root := explainAnalyzeAsUser(t, b, user, cq, cargs)
	if root.NodeType != "Aggregate" || len(root.Plans) != 1 || root.Plans[0].NodeType != "Limit" || root.Plans[0].ActualRows != 101 {
		t.Errorf("the capped count is not an aggregate over cap+1 rows: %s with %v", root.NodeType, root.Plans)
	}
	var n int
	if err := b.db.QueryRowContext(context.Background(), cq, cargs...).Scan(&n); err != nil || n != 101 {
		t.Errorf("capped count = %d, %v; want 101 (the cap + 1 says: more than the cap)", n, err)
	}

	page, err := b.ListFlowsPage(context.Background(), f, 100)
	if err != nil || len(page.Docs) != 25 || !page.HasMore || page.Total != 100 || !page.TotalCapped {
		t.Fatalf("ListFlowsPage = %v; want 25 docs, more, 100 (capped)", page)
	}
	last := page.Docs[len(page.Docs)-1]
	f.After = &interfaces.PageCursor{At: last.UpdatedAt, ID: last.ID}
	next, err := b.ListFlowsPage(context.Background(), f, 100)
	if err != nil || len(next.Docs) != 25 || next.Docs[0].UpdatedAt.After(last.UpdatedAt) {
		t.Fatalf("the keyset page = %v, %v; want the 25 flows after %s", next, err, last.ID)
	}
}
