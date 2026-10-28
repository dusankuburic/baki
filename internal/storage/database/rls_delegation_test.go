package database

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Semantic test for the v27 policy rewrite: the seven flow-child policies
// now delegate to rls_flows_visible instead of repeating the owner /
// collaborator / org-member test inline. DATABASE_URL-gated (throwaway DB).

// v27ChildTables are the seven tables whose policies v27 rewrote, each
// keyed by flow_id.
var v27ChildTables = []string{
	"conversations", "flow_versions", "flow_analysis_history",
	"finding_status", "flow_baselines", "gov_alerts", "finding_comments",
}

// oldVisibleFlows evaluates the pre-v27 inline predicate (the one every
// child policy used to repeat) WITHOUT row-level security, as the oracle for
// what each user used to see.
func (f *r2Fixture) oldVisibleFlows(userID string, flowIDs []string) []string {
	f.t.Helper()
	rows, err := f.b.db.QueryContext(f.ctx, `
		SELECT id FROM flows
		WHERE id = ANY($1)
		  AND (owner_id = $2
		       OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = flows.id AND fc.user_id = $2)
		       OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = flows.org_id AND om.user_id = $2))`,
		flowIDs, userID)
	if err != nil {
		f.t.Fatalf("oracle query: %v", err)
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
		f.t.Fatalf("oracle rows: %v", err)
	}
	return sortedCopy(out)
}

// childFlows lists the distinct flow_ids of table rows reachable through
// ctx (an RLS transaction, or the pool).
func (f *r2Fixture) childFlows(ctx context.Context, table string, flowIDs []string) []string {
	f.t.Helper()
	rows, err := f.b.query(ctx).QueryContext(ctx,
		`SELECT DISTINCT flow_id FROM `+table+` WHERE flow_id = ANY($1)`, flowIDs)
	if err != nil {
		f.t.Fatalf("%s: %v", table, err)
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

// seedChildren writes one row per child table for flowID through the public
// backend methods (autocommit, no RLS) and registers their removal.
func (f *r2Fixture) seedChildren(flowID, author, orgID string) {
	f.t.Helper()
	b, ctx := f.b, f.ctx
	key := f.id("finding")
	if err := b.SaveConversation(ctx, flowID, "flow", []interfaces.ChatMessage{{ID: "m1", Role: "user", Content: "hi"}}); err != nil {
		f.t.Fatalf("SaveConversation: %v", err)
	}
	if err := b.SaveFlowVersion(ctx, &interfaces.FlowVersion{
		ID: f.id("ver-" + flowID), FlowID: flowID, Version: 1, Content: json.RawMessage(`{}`), CreatedBy: author, CreatedAt: time.Now().UTC(),
	}); err != nil {
		f.t.Fatalf("SaveFlowVersion: %v", err)
	}
	if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: flowID, HealthScore: 90, AnalyzedAt: time.Now().UTC()}); err != nil {
		f.t.Fatalf("SaveFlowAnalysis: %v", err)
	}
	if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: flowID, FindingKey: key, Status: "acknowledged", UpdatedBy: author, UpdatedAt: time.Now().UTC()}); err != nil {
		f.t.Fatalf("SetFindingStatus: %v", err)
	}
	if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: flowID, Keys: []string{key}, CreatedBy: author}); err != nil {
		f.t.Fatalf("SetFlowBaseline: %v", err)
	}
	if err := b.RecordGovernanceAlert(ctx, &interfaces.GovernanceAlert{
		ID: f.id("alert-" + flowID), FlowID: flowID, OrgID: orgID, Type: "drift", Title: "drift", Severity: "warning",
	}); err != nil {
		f.t.Fatalf("RecordGovernanceAlert: %v", err)
	}
	if err := b.AddFindingComment(ctx, &interfaces.FindingComment{FlowID: flowID, FindingKey: key, AuthorID: author, Body: "note"}); err != nil {
		f.t.Fatalf("AddFindingComment: %v", err)
	}
	f.t.Cleanup(func() {
		for _, table := range v27ChildTables {
			_, _ = b.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE flow_id = $1`, flowID)
		}
	})
}

// TestPolicyDelegation_V27_MatchesOldVisibility: for an owner, a
// collaborator, an org member and a stranger, every one of the seven child
// tables shows exactly the rows the pre-v27 inline predicate allowed —
// the parent flow's visibility, nothing more — and the WITH CHECK side
// still refuses a write against a flow the caller cannot see.
func TestPolicyDelegation_V27_MatchesOldVisibility(t *testing.T) {
	f := newR2Fixture(t)
	if v, _ := f.b.CurrentSchemaVersion(f.ctx); v < 27 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	owner, collab, member, stranger := f.user("owner"), f.user("collab"), f.user("member"), f.user("stranger")
	org := f.org("org", owner, map[string]string{member: "member"})
	shared := f.flow("shared", owner, org, "Shared", "") // owner's, in org, collab shared
	f.collab(shared, collab)
	private := f.flow("private", stranger, "", "Private", "") // stranger's own, no org
	flows := []string{shared, private}
	f.seedChildren(shared, owner, org)
	f.seedChildren(private, stranger, "")

	// Seeding must have produced a row per table per flow, or the RLS
	// comparison below could pass vacuously.
	for _, table := range v27ChildTables {
		if got := f.childFlows(f.ctx, table, flows); strings.Join(got, ",") != strings.Join(sortedCopy(flows), ",") {
			t.Fatalf("%s seeded for %v, want both flows", table, got)
		}
	}

	users := map[string]string{"owner": owner, "collab": collab, "member": member, "stranger": stranger}
	wantByUser := map[string][]string{
		"owner": {shared}, "collab": {shared}, "member": {shared}, "stranger": {private},
	}
	names := make([]string, 0, len(users))
	for n := range users {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		uid := users[name]
		oracle := f.oldVisibleFlows(uid, flows)
		if strings.Join(oracle, ",") != strings.Join(sortedCopy(wantByUser[name]), ",") {
			t.Fatalf("%s: old predicate sees %v, want %v (fixture wrong)", name, oracle, wantByUser[name])
		}
		rctx := f.rls(uid)
		for _, table := range v27ChildTables {
			if got := f.childFlows(rctx, table, flows); strings.Join(got, ",") != strings.Join(oracle, ",") {
				t.Errorf("%s under RLS sees %s rows for flows %v, want (old semantics) %v", name, table, got, oracle)
			}
		}
	}

	// WITH CHECK: the stranger cannot write a child row for the shared flow;
	// the collaborator can. Each in its own transaction (an error aborts it).
	if err := f.b.SaveConversation(f.rls(stranger), shared, "stranger-"+f.run, nil); err == nil {
		t.Error("stranger wrote a conversation for a flow they cannot see; WITH CHECK is not enforced")
	}
	if err := f.b.SaveConversation(f.rls(collab), shared, "collab-"+f.run, nil); err != nil {
		t.Errorf("collaborator write refused: %v", err)
	}
}

// TestPolicyDelegation_V27_CatalogBodies pins the catalog: each of the seven
// tables has exactly its one named FOR ALL policy, whose USING and WITH
// CHECK reference flows and no longer repeat the collaborator / org-member
// / owner test. conversations' policy is v52's from that step on (owned conversations, R-security-3), pinned by
// TestMigration0052_Policies instead.
func TestPolicyDelegation_V27_CatalogBodies(t *testing.T) {
	b := openMigrationTestDB(t)
	v, _ := b.CurrentSchemaVersion(context.Background())
	if v < 27 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	for _, p := range v27DelegatedPolicies {
		if p.table == "conversations" && v >= 52 {
			continue
		}
		pols := tablePolicies(t, b, p.table)
		if len(pols) != 1 {
			t.Errorf("%s has %d policies, want 1: %+v", p.table, len(pols), pols)
			continue
		}
		got := pols[0]
		if got.name != p.name || got.cmd != "ALL" {
			t.Errorf("%s policy = %s FOR %s, want %s FOR ALL", p.table, got.name, got.cmd, p.name)
		}
		for _, body := range []string{got.qual, got.check} {
			if !strings.Contains(body, "FROM flows f") || !strings.Contains(body, "f.id = "+p.table+".flow_id") {
				t.Errorf("%s policy body does not delegate to flows: %q", p.table, body)
			}
			for _, banned := range []string{"flow_collaborators", "org_members", "owner_id"} {
				if strings.Contains(body, banned) {
					t.Errorf("%s policy body still repeats the flows visibility check (%s): %q", p.table, banned, body)
				}
			}
		}
	}
}
