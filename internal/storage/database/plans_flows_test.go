package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Plan tests for the second performance round (v27): the backend's statements,
// run as the app role inside an RLS transaction with sequential scans priced
// out over a seeded population, must yield keyed index scans; the old OR-shape
// predicates stay Seq Scans. DATABASE_URL-gated (throwaway DB).

// planNode is one node of an EXPLAIN (FORMAT JSON) tree.
type planNode struct {
	NodeType           string     `json:"Node Type"`
	RelationName       string     `json:"Relation Name"`
	IndexName          string     `json:"Index Name"`
	IndexCond          string     `json:"Index Cond"`
	Filter             string     `json:"Filter"`
	ParentRelationship string     `json:"Parent Relationship"` // "SubPlan" / "InitPlan" for a sub-plan's root
	SubplanName        string     `json:"Subplan Name"`
	Plans              []planNode `json:"Plans"`
}

// planPopulation is the background the plan tests run against: other owners'
// flows with alerts, plus a handful the plan user can see by each access path.
// Seeded set-based (no RLS context → the NOT app_rls_active() arm admits it),
// ANALYZEd, cascaded away at cleanup.
const (
	planUser         = "plan-user-r2"
	planOrg          = "plan-org-r2"
	planBgFlows      = 2000
	planBgAlertsEach = 3
)

func seedPlanPopulation(t *testing.T, b *PostgresStorageBackend) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(ctx, `DELETE FROM flows WHERE id LIKE 'plan-bg-%' OR id LIKE 'plan-own-%'`)
		_, _ = b.db.ExecContext(ctx, `DELETE FROM organisations WHERE id LIKE 'plan-org-%'`)
		_, _ = b.db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, planUser) // cascades the collaborator grants
	})
	// flow_collaborators.user_id references users.
	if _, err := b.LoadUserByID(ctx, planUser); err != nil {
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: planUser, Email: planUser + "@test.local", Password: "$2a$12$testhash", Role: "member",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed plan user: %v", err)
		}
	}
	type stmt struct {
		sql  string
		args []any
	}
	stmts := []stmt{
		// 2000 flows: 100 owners, 20 orgs, none of them the plan user's.
		{fmt.Sprintf(`INSERT INTO flows (id, name, content, owner_id, org_id)
			SELECT 'plan-bg-' || g, 'Background flow ' || g, jsonb_build_object('n', g),
			       'plan-bg-owner-' || (g %% 100), 'plan-org-bg-' || (g %% 20)
			FROM generate_series(1, %d) g ON CONFLICT (id) DO NOTHING`, planBgFlows), nil},
		// Alerts on every background flow: team-wide, a third of them read.
		{fmt.Sprintf(`INSERT INTO gov_alerts (id, flow_id, type, created_at, read_at)
			SELECT 'plan-bg-' || g || '-' || k, 'plan-bg-' || g, 'drift',
			       NOW() - (g || ' seconds')::interval,
			       CASE WHEN k = 1 THEN NOW() END
			FROM generate_series(1, %d) g, generate_series(1, %d) k ON CONFLICT (id) DO NOTHING`, planBgFlows, planBgAlertsEach), nil},
		// The plan user: 3 owned flows, an org with 3 flows, 2 collaborator grants.
		{`INSERT INTO organisations (id, name, owner_id) VALUES ($1, 'Plan org', 'plan-org-owner') ON CONFLICT (id) DO NOTHING`, []any{planOrg}},
		{`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, []any{planOrg, planUser}},
		{`INSERT INTO flows (id, name, content, owner_id, org_id)
			SELECT 'plan-own-' || g, 'Own flow ' || g, '{}'::jsonb, $1, '' FROM generate_series(1, 3) g ON CONFLICT (id) DO NOTHING`, []any{planUser}},
		{`INSERT INTO flows (id, name, content, owner_id, org_id)
			SELECT 'plan-own-org-' || g, 'Org flow ' || g, '{}'::jsonb, 'plan-org-owner', $1 FROM generate_series(1, 3) g ON CONFLICT (id) DO NOTHING`, []any{planOrg}},
		{`INSERT INTO flow_collaborators (flow_id, user_id) VALUES ('plan-bg-1', $1), ('plan-bg-2', $1) ON CONFLICT DO NOTHING`, []any{planUser}},
		{`INSERT INTO flow_search (flow_id, name, body) SELECT id, name, content::text FROM flows WHERE id LIKE 'plan-%' ON CONFLICT (flow_id) DO NOTHING`, nil},
		{`ANALYZE flows, flow_search, flow_collaborators, org_members, organisations, gov_alerts`, nil},
	}
	for _, s := range stmts {
		if _, err := b.db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed plan population: %v\n%s", err, s.sql)
		}
	}
}

// flatten returns the node and every descendant (sub-plans included).
func (p planNode) flatten() []planNode {
	out := []planNode{p}
	for _, c := range p.Plans {
		out = append(out, c.flatten()...)
	}
	return out
}

// explainAsUser runs EXPLAIN (FORMAT JSON) of q inside a fresh RLS
// transaction for userID with enable_seqscan off, and returns every plan node.
func explainAsUser(t *testing.T, b *PostgresStorageBackend, userID, q string, args []any) []planNode {
	t.Helper()
	ctx := context.Background()
	tx, err := b.BeginRLS(ctx, userID)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("SET LOCAL enable_seqscan: %v", err)
	}
	rows, err := tx.QueryContext(ctx, "EXPLAIN (FORMAT JSON) "+q, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\n%s", err, q)
	}
	defer rows.Close()
	var raw string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain: %v", err)
		}
		raw += line
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	var top []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("parse explain json: %v\n%s", err, raw)
	}
	if len(top) == 0 {
		t.Fatalf("empty explain output:\n%s", raw)
	}
	return top[0].Plan.flatten()
}

// assertNoSeqScan fails when any node sequentially scans relation.
func assertNoSeqScan(t *testing.T, nodes []planNode, relation, what string) {
	t.Helper()
	for _, n := range nodes {
		if n.NodeType == "Seq Scan" && n.RelationName == relation {
			t.Errorf("%s: plan sequentially scans %s:\n%s", what, relation, describePlan(nodes))
			return
		}
	}
}

// assertIndexUsed fails unless some node scans an index whose name has prefix.
func assertIndexUsed(t *testing.T, nodes []planNode, prefix, what string) {
	t.Helper()
	for _, n := range nodes {
		if strings.HasPrefix(n.IndexName, prefix) {
			return
		}
	}
	t.Errorf("%s: no scan of an index named %s*:\n%s", what, prefix, describePlan(nodes))
}

// assertKeyedAccess fails unless every access of relation is a keyed index
// probe: an Index (Only) Scan with an Index Cond, or a Bitmap Heap Scan fed by
// one. A Seq Scan or an index walked end to end (empty Index Cond) fails.
func assertKeyedAccess(t *testing.T, nodes []planNode, relation, what string) {
	t.Helper()
	for _, n := range nodes {
		if n.RelationName != relation {
			continue
		}
		keyed := n.IndexCond != ""
		if n.NodeType == "Bitmap Heap Scan" {
			for _, c := range n.Plans {
				if c.IndexCond != "" {
					keyed = true
				}
			}
		}
		if !keyed {
			t.Errorf("%s: %s is reached by an unkeyed %s (every %s access must be an index probe):\n%s", what, relation, n.NodeType, relation, describePlan(nodes))
			return
		}
	}
}

func describePlan(nodes []planNode) string {
	var sb strings.Builder
	for _, n := range nodes {
		sb.WriteString("  " + n.NodeType)
		if n.RelationName != "" {
			sb.WriteString(" on " + n.RelationName)
		}
		if n.IndexName != "" {
			sb.WriteString(" using " + n.IndexName)
		}
		if n.IndexCond != "" {
			sb.WriteString(" cond " + n.IndexCond)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// libraryPlanFilter is the library's scope=all filter: owner + the caller's
// orgs + collaborator grants, newest first, metadata only.
func libraryPlanFilter(userID string) interfaces.FlowFilter {
	return interfaces.FlowFilter{
		UserID:          userID,
		OrganizationIDs: []string{planOrg, "plan-org-other"},
		MetadataOnly:    true,
		Limit:           25,
		SortBy:          interfaces.FlowSortUpdatedDesc,
	}
}

// TestPlan_ListFlows_NoSeqScanOnFlows: the library page and its total must
// reach flows through indexes for every library filter shape, as the app role.
func TestPlan_ListFlows_NoSeqScanOnFlows(t *testing.T) {
	b := openMigrationTestDB(t)
	seedPlanPopulation(t, b)
	const user = planUser
	cases := []struct {
		name   string
		filter interfaces.FlowFilter
	}{
		{"scope all", libraryPlanFilter(user)},
		{"scope all with name query", func() interfaces.FlowFilter {
			f := libraryPlanFilter(user)
			f.Query = "invoice"
			return f
		}()},
		{"scope all with tags", func() interfaces.FlowFilter {
			f := libraryPlanFilter(user)
			f.Tags = []string{"prod", "critical"}
			return f
		}()},
		{"scope mine", interfaces.FlowFilter{UserID: user, MetadataOnly: true, Limit: 25}},
		{"single org", interfaces.FlowFilter{UserID: user, OrganizationID: planOrg, MetadataOnly: true, Limit: 25}},
		{"shared only", interfaces.FlowFilter{UserID: user, SharedOnly: true, MetadataOnly: true, Limit: 25}},
		{"name sort", func() interfaces.FlowFilter {
			f := libraryPlanFilter(user)
			f.SortBy = interfaces.FlowSortNameAsc
			return f
		}()},
		{"keyset walk", func() interfaces.FlowFilter {
			f := libraryPlanFilter(user)
			f.SortBy = interfaces.FlowSortIDAsc
			f.AfterID = "flow-0001"
			f.Limit = 500
			return f
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, args := listFlowsSQL(tc.filter)
			nodes := explainAsUser(t, b, user, q, args)
			assertNoSeqScan(t, nodes, "flows", "ListFlows "+tc.name)

			cq, cargs := countFlowsSQL(tc.filter)
			cnodes := explainAsUser(t, b, user, cq, cargs)
			assertNoSeqScan(t, cnodes, "flows", "CountFlows "+tc.name)
		})
	}
}

// TestPlan_SearchFlowContents_UsesFlowSearchIndex: the content search must be
// served by the flow_search trigram indexes (on flows, FORCE RLS + non-leakproof
// ILIKE makes the equivalent indexes dead) and never scan flows.
func TestPlan_SearchFlowContents_UsesFlowSearchIndex(t *testing.T) {
	b := openMigrationTestDB(t)
	seedPlanPopulation(t, b)
	const user = planUser
	q, args := searchFlowContentsSQL(interfaces.FlowFilter{UserID: user}, "SetVariable", 200)
	nodes := explainAsUser(t, b, user, q, args)
	assertNoSeqScan(t, nodes, "flows", "SearchFlowContents")
	assertNoSeqScan(t, nodes, "flow_search", "SearchFlowContents")
	assertKeyedAccess(t, nodes, "flow_search", "SearchFlowContents")
	assertIndexUsed(t, nodes, "flow_search_", "SearchFlowContents")

	// Pinned: the body predicate is evaluated on flow_search, never on flows,
	// and nothing is sequentially scanned — the driving side is the planner's call.
	wide := interfaces.FlowFilter{UserID: user}
	for i := 0; i < 20; i++ {
		wide.OrganizationIDs = append(wide.OrganizationIDs, fmt.Sprintf("plan-org-bg-%d", i))
	}
	wq, wargs := searchFlowContentsSQL(wide, "SetVariable", 200)
	wnodes := explainAsUser(t, b, user, wq, wargs)
	assertNoSeqScan(t, wnodes, "flows", "SearchFlowContents wide scope")
	assertNoSeqScan(t, wnodes, "flow_search", "SearchFlowContents wide scope")
	assertIndexUsed(t, wnodes, "flow_search_", "SearchFlowContents wide scope")

	// The trigram indexes are usable by the app role — the reason the side
	// table has no RLS. Body for the content search, name for the library filter.
	sub := explainAsUser(t, b, user, `SELECT flow_id FROM flow_search WHERE body ILIKE $1`, []any{"%setvariable%"})
	assertIndexUsed(t, sub, "flow_search_body_trgm_idx", "flow_search body predicate")
	nsub := explainAsUser(t, b, user, `SELECT flow_id FROM flow_search WHERE name ILIKE $1`, []any{"%invoice%"})
	assertIndexUsed(t, nsub, "flow_search_name_trgm_idx", "flow_search name predicate")

	// The library's name filter goes through the same side table — the
	// predicate is pinned to flow_search, never an ILIKE over flows.
	f := libraryPlanFilter(user)
	f.Query = "invoice"
	lq, largs := listFlowsSQL(f)
	lnodes := explainAsUser(t, b, user, lq, largs)
	assertNoSeqScan(t, lnodes, "flow_search", "ListFlows name query")
	assertIndexUsed(t, lnodes, "flow_search_", "ListFlows name query")
}

// TestPlan_GovernanceAlerts_StartFromVisibleFlows: the badge count and alert
// list must probe gov_alerts from the caller's flows — every access keyed. The
// old nested-EXISTS predicate walks every unread alert, exactly what the
// keyed-access check rejects.
func TestPlan_GovernanceAlerts_StartFromVisibleFlows(t *testing.T) {
	b := openMigrationTestDB(t)
	seedPlanPopulation(t, b)
	const user = planUser

	cq, cargs := unreadGovAlertCountSQL(user)
	cnodes := explainAsUser(t, b, user, cq, cargs)
	assertKeyedAccess(t, cnodes, "gov_alerts", "UnreadGovernanceAlertCountFor")
	assertKeyedAccess(t, cnodes, "flows", "UnreadGovernanceAlertCountFor")

	for _, dismissed := range []bool{false, true} {
		lq, largs := listGovAlertsSQL(interfaces.GovernanceAlertFilter{UserID: user, IncludeDismissed: dismissed}, govAlertListDefaultLimit)
		lnodes := explainAsUser(t, b, user, lq, largs)
		assertKeyedAccess(t, lnodes, "gov_alerts", fmt.Sprintf("ListGovernanceAlerts(dismissed=%v)", dismissed))
		assertKeyedAccess(t, lnodes, "flows", fmt.Sprintf("ListGovernanceAlerts(dismissed=%v)", dismissed))
	}
}

// TestPlan_OldAlertPredicateWalksEveryAlert: the pre-v27 badge query has no
// key to start from, proving the keyed-access check tells the shapes apart.
func TestPlan_OldAlertPredicateWalksEveryAlert(t *testing.T) {
	b := openMigrationTestDB(t)
	seedPlanPopulation(t, b)
	var args sqlArgs
	vis := govAlertWhere(planUser, &args)
	old := `SELECT COUNT(*) FROM gov_alerts WHERE read_at IS NULL AND dismissed_at IS NULL AND ` + vis
	nodes := explainAsUser(t, b, planUser, old, args)
	for _, n := range nodes {
		if (n.RelationName == "gov_alerts" || n.RelationName == "flows") && n.IndexCond == "" && n.NodeType != "Bitmap Heap Scan" {
			return // unkeyed, as expected
		}
	}
	t.Fatalf("the old alert predicate is now keyed on both gov_alerts and flows; the alert plan test cannot tell the shapes apart:\n%s", describePlan(nodes))
}

// TestPlan_OldVisibilityShapeIsASeqScan proves the assertions above are not
// vacuous: the pre-v27 OR shape has no index path, so the planner can only
// scan flows. If it starts passing, the harness has stopped observing the
// planner (e.g. a BYPASSRLS role) and the tests above prove nothing.
func TestPlan_OldVisibilityShapeIsASeqScan(t *testing.T) {
	b := openMigrationTestDB(t)
	seedPlanPopulation(t, b)
	const user = planUser
	old := `SELECT count(*) FROM flows WHERE (owner_id = $1 OR org_id = ANY($2)
		OR EXISTS (SELECT 1 FROM flow_collaborators WHERE flow_id = id AND user_id = $1))`
	nodes := explainAsUser(t, b, user, old, []any{user, []string{planOrg}})
	seq := false
	for _, n := range nodes {
		if n.NodeType == "Seq Scan" && n.RelationName == "flows" {
			seq = true
		}
	}
	if !seq {
		t.Fatalf("the old OR shape no longer sequentially scans flows; the plan tests cannot tell the shapes apart:\n%s", describePlan(nodes))
	}
}

// TestPlan_StatementShapesAreStable: list-valued filters bind ONE array
// parameter, so the statement text (the pgx statement-cache key) is fixed.
func TestPlan_StatementShapesAreStable(t *testing.T) {
	one := libraryPlanFilter("u")
	one.OrganizationIDs = []string{"a"}
	one.Tags = []string{"x"}
	many := libraryPlanFilter("u")
	many.OrganizationIDs = []string{"a", "b", "c", "d"}
	many.Tags = []string{"x", "y", "z"}
	q1, a1 := listFlowsSQL(one)
	q2, a2 := listFlowsSQL(many)
	if q1 != q2 {
		t.Errorf("ListFlows statement text varies with list lengths:\n%s\n---\n%s", q1, q2)
	}
	if len(a1) != len(a2) {
		t.Errorf("ListFlows arg count varies with list lengths: %d vs %d", len(a1), len(a2))
	}
	if strings.Contains(q1, " IN ($") || strings.Contains(q1, ",$") {
		t.Errorf("ListFlows still expands a list into placeholders:\n%s", q1)
	}
	if !strings.Contains(q1, "org_id = ANY($2)") {
		t.Errorf("org list must bind as = ANY($n):\n%s", q1)
	}
}

// TestSearchFlowContents_NeedleIsLiteral: the needle is a literal substring
// — LIKE wildcards in it are escaped (as the name query's now are too).
func TestSearchFlowContents_NeedleIsLiteral(t *testing.T) {
	_, args := searchFlowContentsSQL(interfaces.FlowFilter{UserID: "u"}, `50%_done\`, 10)
	got, _ := args[len(args)-2].(string)
	if want := `%50\%\_done\\%`; got != want {
		t.Errorf("needle pattern = %q, want %q", got, want)
	}
	// A run of three letters, so a substring (without one the name query is a prefix: plans_search_minimum_test.go).
	_, largs := listFlowsSQL(interfaces.FlowFilter{UserID: "u", Query: "abc_d%"})
	if got, _ := largs[1].(string); got != `%abc\_d\%%` {
		t.Errorf("name query pattern = %q, want %q", got, `%abc\_d\%%`)
	}
}
