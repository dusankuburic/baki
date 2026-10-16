package database

import (
	"context"
	"fmt"
	"testing"
)

// indexes0048 are the gov_alerts and erasure actor-column indexes of v48, as SchemaUpkeep builds them after boot
// (without CONCURRENTLY here; TestMigration0048_Shape holds the two lists equal). The plan tests below create them on a
// private database before checking the plans, so they hold whether the schema they start from has the indexes or not
// (IF NOT EXISTS: a no-op once the upkeep has run).
var indexes0048 = map[string]string{
	"gov_alerts_dismissed_flow_idx": `CREATE INDEX IF NOT EXISTS gov_alerts_dismissed_flow_idx ON gov_alerts (flow_id) WHERE dismissed_at IS NOT NULL`,
	"flow_versions_created_by_idx":  `CREATE INDEX IF NOT EXISTS flow_versions_created_by_idx ON flow_versions (created_by) WHERE created_by <> ''`,
	"finding_comments_author_idx":   `CREATE INDEX IF NOT EXISTS finding_comments_author_idx ON finding_comments (author_id) WHERE author_id <> ''`,
	"finding_status_updated_by_idx": `CREATE INDEX IF NOT EXISTS finding_status_updated_by_idx ON finding_status (updated_by) WHERE updated_by <> ''`,
	"finding_status_assignee_idx":   `CREATE INDEX IF NOT EXISTS finding_status_assignee_idx ON finding_status (assignee_id) WHERE assignee_id <> ''`,
}

// createIndexes0048 creates the named indexes and refreshes the statistics.
func createIndexes0048(t *testing.T, b *PostgresStorageBackend, names ...string) {
	t.Helper()
	ctx := context.Background()
	for _, name := range names {
		if _, err := b.db.ExecContext(ctx, indexes0048[name]); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if _, err := b.db.ExecContext(ctx, `ANALYZE gov_alerts, flow_versions, finding_comments, finding_status`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// scans drops the ModifyTable node of an UPDATE or DELETE plan: it names the target table but reads nothing itself.
func scans(nodes []planNode) []planNode {
	out := make([]planNode, 0, len(nodes))
	for _, n := range nodes {
		if n.NodeType != "ModifyTable" {
			out = append(out, n)
		}
	}
	return out
}

// indexExists reports whether the schema already has the index (the upkeep built it).
func indexExists(t *testing.T, b *PostgresStorageBackend, name string) bool {
	t.Helper()
	var ok bool
	if err := b.db.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1)`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestPlan_GovernanceAlertBulkMutations_StartFromVisibleFlows: "mark all read" and "clear" reach gov_alerts through the
// caller's flows, every access keyed, like the list and the badge; the nested-EXISTS shape they had walked every
// tenant's unread or dismissed alerts. With the partial index on dismissed alerts the clear probes only those.
func TestPlan_GovernanceAlertBulkMutations_StartFromVisibleFlows(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seedPlanPopulation(t, b)
	if _, err := b.db.ExecContext(context.Background(), `UPDATE gov_alerts SET dismissed_at = NOW() WHERE id LIKE 'plan-bg-%-2'`); err != nil {
		t.Fatalf("dismiss a third of the alerts: %v", err)
	}
	createIndexes0048(t, b) // statistics only
	const user = planUser

	mq, margs := markAllGovAlertsReadSQL(user)
	mnodes := scans(explainAsUser(t, b, user, mq, margs))
	assertKeyedAccess(t, mnodes, "gov_alerts", "MarkAllGovernanceAlertsRead")
	assertKeyedAccess(t, mnodes, "flows", "MarkAllGovernanceAlertsRead")

	cq, cargs := clearGovAlertsSQL(user)
	cnodes := scans(explainAsUser(t, b, user, cq, cargs))
	assertKeyedAccess(t, cnodes, "gov_alerts", "ClearGovernanceAlerts")
	assertKeyedAccess(t, cnodes, "flows", "ClearGovernanceAlerts")

	createIndexes0048(t, b, "gov_alerts_dismissed_flow_idx")
	cnodes = scans(explainAsUser(t, b, user, cq, cargs))
	assertKeyedAccess(t, cnodes, "gov_alerts", "ClearGovernanceAlerts with the dismissed index")
	assertIndexUsed(t, cnodes, "gov_alerts_dismissed_flow_idx", "ClearGovernanceAlerts with the dismissed index")
}

// TestPlan_ErasureActorColumns_UseIndexes: the erase statements that clear a user's id from the rows kept for others
// probe the actor columns through v48's partial indexes instead of reading the whole table. Where the schema does not
// have the indexes yet (a fresh boot, before the upkeep), the same statements are first shown unkeyed, so the check
// tells the two apart.
func TestPlan_ErasureActorColumns_UseIndexes(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seedPlanPopulation(t, b)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO flow_versions (id, flow_id, version, created_by)
		 SELECT f.id || '-v' || v, f.id, v, 'plan-bg-owner-' || (g % 100)
		 FROM generate_series(1, 2000) g JOIN flows f ON f.id = 'plan-bg-' || g, generate_series(1, 3) v`,
		`INSERT INTO finding_comments (id, flow_id, finding_key, author_id, body)
		 SELECT 'plan-bg-c-' || g, 'plan-bg-' || g, 'r:' || g, 'plan-bg-owner-' || (g % 100), 'words'
		 FROM generate_series(1, 2000) g`,
		`INSERT INTO finding_status (flow_id, finding_key, status, assignee_id, updated_by)
		 SELECT 'plan-bg-' || g, 'r:' || k, 'acknowledged', CASE WHEN k = 1 THEN 'plan-bg-owner-' || (g % 50) ELSE '' END, 'plan-bg-owner-' || (g % 100)
		 FROM generate_series(1, 2000) g, generate_series(1, 3) k`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	createIndexes0048(t, b) // statistics only

	stmt := func(table string) string {
		t.Helper()
		for _, s := range eraseSteps() {
			if s.table == table && s.phase == eraseIdentity && s.query != "" {
				return s.query
			}
		}
		t.Fatalf("no identity erase step on %s", table)
		return ""
	}
	cases := []struct {
		table   string
		indexes []string
	}{
		{"flow_versions", []string{"flow_versions_created_by_idx"}},
		{"finding_comments", []string{"finding_comments_author_idx"}},
		// updated_by = $1 OR assignee_id = $1: a BitmapOr of the two.
		{"finding_status", []string{"finding_status_updated_by_idx", "finding_status_assignee_idx"}},
	}
	args := []any{"plan-bg-owner-7"}
	for _, tc := range cases {
		q := stmt(tc.table)
		shipped := true
		for _, ix := range tc.indexes {
			shipped = shipped && indexExists(t, b, ix)
		}
		if !shipped {
			before := scans(explainAsUser(t, b, "", q, args))
			unkeyed := false
			for _, n := range before {
				if n.RelationName == tc.table && n.IndexCond == "" && n.NodeType != "Bitmap Heap Scan" {
					unkeyed = true
				}
			}
			if !unkeyed {
				t.Errorf("%s: the erase statement is keyed before the indexes exist; the check below proves nothing:\n%s", tc.table, describePlan(before))
			}
		}
		createIndexes0048(t, b, tc.indexes...)
		after := scans(explainAsUser(t, b, "", q, args))
		assertKeyedAccess(t, after, tc.table, fmt.Sprintf("erase %s", tc.table))
		for _, ix := range tc.indexes {
			assertIndexUsed(t, after, ix, fmt.Sprintf("erase %s", tc.table))
		}
	}
}
