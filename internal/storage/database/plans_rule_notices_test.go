package database

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Rule notices filter on the caller's scope; their indexes are on the generated user_id and org_id columns (v37), so
// the predicate has to name those columns for the indexes to serve it. DATABASE_URL-gated, on a private database.

// oldRuleNoticeVisible is ruleNoticeVisible before v49 (the oracle): the scope and scope_id columns, which no index
// covers.
func oldRuleNoticeVisible(user string) string {
	return fmt.Sprintf(`(scope = 'app'
		 OR (scope = 'user' AND scope_id = %[1]s)
		 OR (scope = 'org' AND EXISTS (SELECT 1 FROM org_members om
		                                WHERE om.org_id = rule_notices.scope_id AND om.user_id = %[1]s)))`, user)
}

const (
	rnPlanUsers = 400
	rnPlanOrgs  = 100
)

// seedRuleNoticePopulation stores rnPlanUsers users with a notice each and rnPlanOrgs organisations with three notices
// each, none of them planUser's, then planUser's own notice, two of planUser's organisations with a notice each, and
// two app notices; ANALYZEd. It also builds v49's app-notice index (IF NOT EXISTS: a no-op once the step ran).
func seedRuleNoticePopulation(t *testing.T, b *PostgresStorageBackend) {
	t.Helper()
	ctx := context.Background()
	for _, s := range []string{
		fmt.Sprintf(`INSERT INTO users (id, email, password, role)
			SELECT 'rn-plan-u-' || g, 'rn-plan-u-' || g || '@example.com', 'x', 'member' FROM generate_series(1, %d) g`, rnPlanUsers),
		fmt.Sprintf(`INSERT INTO organisations (id, name, owner_id)
			SELECT 'rn-plan-org-' || g, 'Org ' || g, 'rn-plan-u-1' FROM generate_series(1, %d) g`, rnPlanOrgs),
		`INSERT INTO users (id, email, password, role) VALUES ('` + planUser + `', '` + planUser + `@example.com', 'x', 'member')`,
		`INSERT INTO org_members (org_id, user_id, role) VALUES ('rn-plan-org-1', '` + planUser + `', 'member'), ('rn-plan-org-2', '` + planUser + `', 'admin')`,
		// Other members, so the organisation arm is not just the caller's rows.
		fmt.Sprintf(`INSERT INTO org_members (org_id, user_id, role)
			SELECT 'rn-plan-org-' || (g %% %d + 1), 'rn-plan-u-' || g, 'member' FROM generate_series(1, %d) g`, rnPlanOrgs, rnPlanUsers),
		fmt.Sprintf(`INSERT INTO rule_notices (id, kind, scope, scope_id, rule_set_digest, created_at)
			SELECT 'new_rules|user|rn-plan-u-' || g || '|d', 'new_rules', 'user', 'rn-plan-u-' || g, 'd', NOW() - (g || ' minutes')::interval
			FROM generate_series(1, %d) g`, rnPlanUsers),
		fmt.Sprintf(`INSERT INTO rule_notices (id, kind, scope, scope_id, rule_set_digest, created_at, read_at)
			SELECT 'new_rules|org|rn-plan-org-' || g || '|d' || k, 'new_rules', 'org', 'rn-plan-org-' || g, 'd' || k,
			       NOW() - (g || ' minutes')::interval, CASE WHEN k = 1 THEN NOW() END
			FROM generate_series(1, %d) g, generate_series(1, 3) k`, rnPlanOrgs),
		`INSERT INTO rule_notices (id, kind, scope, scope_id, rule_set_digest) VALUES
			('new_rules|user|` + planUser + `|d', 'new_rules', 'user', '` + planUser + `', 'd'),
			('new_rules|app||d1', 'new_rules', 'app', '', 'd1'), ('new_rules|app||d2', 'new_rules', 'app', '', 'd2')`,
		`CREATE INDEX IF NOT EXISTS rule_notices_app_idx ON rule_notices (id) WHERE scope = 'app'`,
		`ANALYZE users, organisations, org_members, rule_notices`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed rule notices: %v\n%s", err, s)
		}
	}
}

// assertRuleNoticesKeyed fails unless rule_notices is reached only through keyed probes of its scope indexes: no
// Seq Scan, every index scan with a condition, and both the user and the organisation index among them.
func assertRuleNoticesKeyed(t *testing.T, nodes []planNode, what string) {
	t.Helper()
	used := map[string]bool{}
	for _, n := range nodes {
		if n.RelationName == "rule_notices" && n.NodeType == "Seq Scan" {
			t.Errorf("%s: plan sequentially scans rule_notices:\n%s", what, describePlan(nodes))
			return
		}
		if strings.HasPrefix(n.IndexName, "rule_notices_") {
			used[n.IndexName] = true
			if n.IndexCond == "" && n.IndexName != "rule_notices_app_idx" {
				t.Errorf("%s: %s is walked end to end (no index condition):\n%s", what, n.IndexName, describePlan(nodes))
			}
		}
	}
	for _, ix := range []string{"rule_notices_user_idx", "rule_notices_org_idx"} {
		if !used[ix] {
			t.Errorf("%s: %s is not used:\n%s", what, ix, describePlan(nodes))
		}
	}
}

// TestPlan_RuleNotices_UseTheScopeIndexes: the inbox's notice list, its unread count and "mark all read" reach
// rule_notices through rule_notices_user_idx and rule_notices_org_idx (and the app notices through their partial
// index), as the app role inside the caller's RLS scope. The pre-v49 predicate named scope and scope_id, so neither
// index could serve it: shown first, so the check tells the two shapes apart.
func TestPlan_RuleNotices_UseTheScopeIndexes(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seedRuleNoticePopulation(t, b)
	const user = planUser

	var oargs sqlArgs
	old := `SELECT id FROM rule_notices WHERE dismissed_at IS NULL AND ` + oldRuleNoticeVisible(oargs.add(user)) + ` ORDER BY created_at DESC, id ASC LIMIT 10`
	onodes := explainAsUser(t, b, user, old, oargs)
	unkeyed := false
	for _, n := range onodes {
		if n.IndexName == "rule_notices_user_idx" || n.IndexName == "rule_notices_org_idx" {
			t.Fatalf("the pre-v49 predicate already uses %s; the check below proves nothing:\n%s", n.IndexName, describePlan(onodes))
		}
		if n.RelationName == "rule_notices" && n.IndexCond == "" {
			unkeyed = true
		}
	}
	if !unkeyed {
		t.Fatalf("the pre-v49 predicate reaches rule_notices keyed; the check below proves nothing:\n%s", describePlan(onodes))
	}

	lq, largs := listRuleNoticesSQL(user, ruleNoticeListDefaultLimit)
	lnodes := explainAsUser(t, b, user, lq, largs)
	t.Logf("ListRuleNotices plan:\n%s", describePlan(lnodes))
	assertRuleNoticesKeyed(t, lnodes, "ListRuleNotices")
	cq, cargs := unreadRuleNoticeCountSQL(user)
	assertRuleNoticesKeyed(t, explainAsUser(t, b, user, cq, cargs), "UnreadRuleNoticeCount")
	mq, margs := markAllRuleNoticesReadSQL(user)
	assertRuleNoticesKeyed(t, scans(explainAsUser(t, b, user, mq, margs)), "MarkAllRuleNoticesRead")
}

// TestRuleNotices_PredicateMatchesThePreV49One: on the pool (no RLS, as under a BYPASSRLS role, where the predicate is
// the only tenant boundary) the generated-column predicate admits exactly the rows the scope/scope_id one did, for a
// member of two organisations, a user with only their own notice, a user in no organisation, a user whose id is also
// an organisation's id, and an unknown id.
func TestRuleNotices_PredicateMatchesThePreV49One(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seedRuleNoticePopulation(t, b)
	ctx := context.Background()
	// An account whose id is also an organisation's (ids are free text): its notices are its own, never the org's.
	for _, s := range []string{
		`INSERT INTO users (id, email, password, role) VALUES ('rn-plan-org-3', 'twin@example.com', 'x', 'member')`,
		`INSERT INTO rule_notices (id, kind, scope, scope_id, rule_set_digest) VALUES ('new_rules|user|rn-plan-org-3|d', 'new_rules', 'user', 'rn-plan-org-3', 'd')`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	visible := func(pred func(string) string, user string) []string {
		t.Helper()
		var args sqlArgs
		ids, err := rowsOfStrings(ctx, b.db, `SELECT id FROM rule_notices WHERE `+pred(args.add(user))+` ORDER BY id`, args...)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	for _, user := range []string{planUser, "rn-plan-u-7", "rn-plan-org-3", "rn-plan-nobody", ""} {
		want, got := visible(oldRuleNoticeVisible, user), visible(ruleNoticeVisible, user)
		if !slices.Equal(got, want) {
			t.Errorf("%q: the predicate admits %d rows %v, the pre-v49 one %d %v", user, len(got), got, len(want), want)
		}
		if len(want) < 2 {
			t.Errorf("%q: only %d rows visible; the fixture must show the app notices to everyone", user, len(want))
		}
	}
}
