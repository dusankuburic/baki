package database

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// Semantic tests for the second performance round (v27): the rewritten
// visibility, search and alert statements must answer exactly as the old
// ones did. DATABASE_URL-gated (throwaway DB); every id carries a run stamp
// so the tests are independent of whatever else the database holds.

func r2stamp() string { return time.Now().Format("150405.000000000") }

// r2Fixture seeds users, an org and flows for one test and removes them.
type r2Fixture struct {
	t   *testing.T
	b   *PostgresStorageBackend
	ctx context.Context
	run string
}

func newR2Fixture(t *testing.T) *r2Fixture {
	t.Helper()
	return &r2Fixture{t: t, b: openMigrationTestDB(t), ctx: context.Background(), run: r2stamp()}
}

func (f *r2Fixture) id(label string) string { return "r2-" + label + "-" + f.run }

func (f *r2Fixture) user(label string) string {
	f.t.Helper()
	id := f.id(label)
	if err := f.b.CreateUser(f.ctx, &interfaces.User{
		ID: id, Email: id + "@test.local", Password: "$2a$12$testhash", Role: "member",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		f.t.Fatalf("CreateUser %s: %v", id, err)
	}
	f.t.Cleanup(func() { _, _ = f.b.db.ExecContext(f.ctx, `DELETE FROM users WHERE id = $1`, id) })
	return id
}

func (f *r2Fixture) org(label, owner string, members map[string]string) string {
	f.t.Helper()
	id := f.id(label)
	org := &interfaces.Organisation{ID: id, Name: label, OwnerID: owner}
	for uid, role := range members {
		org.Members = append(org.Members, interfaces.OrgMember{UserID: uid, Role: auth.Role(role)})
	}
	if err := f.b.SaveOrg(f.ctx, org); err != nil {
		f.t.Fatalf("SaveOrg %s: %v", id, err)
	}
	f.t.Cleanup(func() { _ = f.b.DeleteOrg(f.ctx, id) })
	return id
}

func (f *r2Fixture) flow(label, owner, orgID, name, content string) string {
	f.t.Helper()
	id := f.id(label)
	if content == "" {
		content = `{}`
	}
	if err := f.b.SaveFlow(f.ctx, &interfaces.FlowDocument{
		ID: id, Name: name, OwnerID: owner, OrganizationID: orgID, Content: []byte(content),
	}); err != nil {
		f.t.Fatalf("SaveFlow %s: %v", id, err)
	}
	f.t.Cleanup(func() { _ = f.b.DeleteFlow(f.ctx, id) })
	return id
}

func (f *r2Fixture) collab(flowID, userID string) {
	f.t.Helper()
	if err := f.b.AddCollaborator(f.ctx, flowID, &interfaces.Collaborator{UserID: userID, Permission: "viewer"}); err != nil {
		f.t.Fatalf("AddCollaborator %s/%s: %v", flowID, userID, err)
	}
}

// rls returns a context whose statements run as userID under RLS; the
// transaction is rolled back at cleanup.
func (f *r2Fixture) rls(userID string) context.Context {
	f.t.Helper()
	tx, err := f.b.BeginRLS(f.ctx, userID)
	if err != nil {
		f.t.Fatalf("BeginRLS: %v", err)
	}
	f.t.Cleanup(func() { _ = tx.Rollback() })
	return WithRLSTx(f.ctx, tx)
}

// ids lists the flow ids ListFlows returns for filter, plus CountFlows.
func (f *r2Fixture) ids(ctx context.Context, filter interfaces.FlowFilter) ([]string, int) {
	f.t.Helper()
	filter.MetadataOnly = true
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	docs, err := f.b.ListFlows(ctx, filter)
	if err != nil {
		f.t.Fatalf("ListFlows: %v", err)
	}
	n, err := f.b.CountFlows(ctx, filter)
	if err != nil {
		f.t.Fatalf("CountFlows: %v", err)
	}
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.ID)
	}
	return out, n
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func assertIDs(t *testing.T, what string, got []string, count int, want ...string) {
	t.Helper()
	g, w := sortedCopy(got), sortedCopy(want)
	if strings.Join(g, ",") != strings.Join(w, ",") {
		t.Errorf("%s: ListFlows = %v, want %v", what, g, w)
	}
	if count != len(want) {
		t.Errorf("%s: CountFlows = %d, want %d", what, count, len(want))
	}
}

// TestVisibility_UnionSemiJoin_MatchesOldSemantics: the id IN (UNION ALL)
// rewrite returns the same set as the OR + EXISTS shape — a flow reachable by
// several paths once; org flows for members that pass the org in the
// filter; shared-only excludes owned flows — and the same under RLS.
func TestVisibility_UnionSemiJoin_MatchesOldSemantics(t *testing.T) {
	f := newR2Fixture(t)
	alice, bob, carol := f.user("alice"), f.user("bob"), f.user("carol")
	org := f.org("org", alice, map[string]string{alice: "admin", bob: "member"})

	own := f.flow("own", alice, "", "Alice personal", "")      // owned by alice
	both := f.flow("both", alice, org, "Org flow", "")         // owned by alice, in org, alice ALSO a collaborator
	shared := f.flow("shared", carol, "", "Carol shares", "")  // owned by carol, shared with alice and bob
	theirs := f.flow("theirs", carol, "", "Carol private", "") // invisible to alice/bob
	f.collab(both, alice)
	f.collab(shared, alice)
	f.collab(shared, bob)
	f.collab(own, alice) // owner + collaborator on the same flow: must still list once

	t.Run("scope all: owner, org and collaborator paths, each flow once", func(t *testing.T) {
		got, n := f.ids(f.ctx, interfaces.FlowFilter{UserID: alice, OrganizationIDs: []string{org}})
		assertIDs(t, "alice all", got, n, own, both, shared)
		got, n = f.ids(f.ctx, interfaces.FlowFilter{UserID: bob, OrganizationIDs: []string{org}})
		assertIDs(t, "bob all", got, n, both, shared)
	})
	t.Run("scope mine: no org widening", func(t *testing.T) {
		got, n := f.ids(f.ctx, interfaces.FlowFilter{UserID: bob})
		assertIDs(t, "bob mine", got, n, shared) // collaborator grants still count, org membership does not
		got, n = f.ids(f.ctx, interfaces.FlowFilter{UserID: alice})
		assertIDs(t, "alice mine", got, n, own, both, shared)
	})
	t.Run("single org", func(t *testing.T) {
		got, n := f.ids(f.ctx, interfaces.FlowFilter{UserID: bob, OrganizationID: org})
		assertIDs(t, "bob org", got, n, both, shared)
		got, n = f.ids(f.ctx, interfaces.FlowFilter{OrganizationID: org})
		assertIDs(t, "org only", got, n, both)
		got, n = f.ids(f.ctx, interfaces.FlowFilter{OrganizationIDs: []string{org, "nope"}})
		assertIDs(t, "org list only", got, n, both)
	})
	t.Run("shared only excludes owned flows", func(t *testing.T) {
		got, n := f.ids(f.ctx, interfaces.FlowFilter{UserID: alice, SharedOnly: true})
		assertIDs(t, "alice shared", got, n, shared) // own/both are owned by alice although she is a collaborator
		got, n = f.ids(f.ctx, interfaces.FlowFilter{UserID: carol, SharedOnly: true})
		assertIDs(t, "carol shared", got, n)
	})
	t.Run("unscoped filter matches nothing", func(t *testing.T) {
		got, n := f.ids(f.ctx, interfaces.FlowFilter{})
		assertIDs(t, "unscoped", got, n)
	})
	t.Run("under RLS as the caller", func(t *testing.T) {
		got, n := f.ids(f.rls(bob), interfaces.FlowFilter{UserID: bob, OrganizationIDs: []string{org}})
		assertIDs(t, "bob all under RLS", got, n, both, shared)
		got, n = f.ids(f.rls(alice), interfaces.FlowFilter{UserID: alice, SharedOnly: true})
		assertIDs(t, "alice shared under RLS", got, n, shared)
	})
	t.Run("name query, tags and keyset cursor still narrow", func(t *testing.T) {
		got, n := f.ids(f.ctx, interfaces.FlowFilter{UserID: alice, OrganizationIDs: []string{org}, Query: "CAROL"})
		assertIDs(t, "alice query", got, n, shared)
		got, n = f.ids(f.ctx, interfaces.FlowFilter{UserID: alice, Query: "%"})
		assertIDs(t, "alice literal %", got, n) // a wildcard in the query is a literal now
		if err := f.b.UpdateFlowTags(f.ctx, own, []string{"prod"}); err != nil {
			t.Fatalf("UpdateFlowTags: %v", err)
		}
		if err := f.b.UpdateFlowTags(f.ctx, both, []string{"production"}); err != nil {
			t.Fatalf("UpdateFlowTags: %v", err)
		}
		got, n = f.ids(f.ctx, interfaces.FlowFilter{UserID: alice, OrganizationIDs: []string{org}, Tags: []string{"prod", "other"}})
		assertIDs(t, "alice tags", got, n, own) // 'prod' must not match 'production'
		all := sortedCopy([]string{own, both, shared})
		got, n = f.ids(f.ctx, interfaces.FlowFilter{UserID: alice, OrganizationIDs: []string{org}, SortBy: interfaces.FlowSortIDAsc, AfterID: all[0]})
		if strings.Join(got, ",") != strings.Join(all[1:], ",") {
			t.Errorf("keyset page after %s = %v, want %v", all[0], got, all[1:])
		}
		if n != 2 {
			t.Errorf("keyset count = %d, want 2", n)
		}
	})
	_ = theirs
}

// TestFlowSearch_SideTableFollowsTheFlow: name and content search find a
// flow after save and after rename/edit, through SearchFlowContents (name or
// body) and the library's name filter; the stored body is the content::text
// form the old pushdown matched; deleting the flow removes the search row.
func TestFlowSearch_SideTableFollowsTheFlow(t *testing.T) {
	f := newR2Fixture(t)
	alice := f.user("alice")
	flowID := f.flow("flow", alice, "", "Alpha Report", `{"blocks":[{"name":"SetVariable zebra","kind":"Excel"}]}`)

	search := func(ctx context.Context, needle string) []string {
		t.Helper()
		docs, err := f.b.SearchFlowContents(ctx, interfaces.FlowFilter{UserID: alice}, needle, 50)
		if err != nil {
			t.Fatalf("SearchFlowContents(%q): %v", needle, err)
		}
		out := []string{}
		for _, d := range docs {
			out = append(out, d.ID)
		}
		return out
	}
	byName := func(ctx context.Context, q string) []string {
		t.Helper()
		ids, _ := f.ids(ctx, interfaces.FlowFilter{UserID: alice, Query: q})
		return ids
	}
	want := func(what string, got []string, hit bool) {
		t.Helper()
		found := len(got) == 1 && got[0] == flowID
		if found != hit {
			t.Errorf("%s: got %v, want hit=%v", what, got, hit)
		}
		if len(got) > 1 {
			t.Errorf("%s: duplicate rows %v", what, got)
		}
	}

	want("content zebra", search(f.ctx, "zebra"), true)
	want("content case-insensitive", search(f.ctx, "setvariable"), true)
	// Body only, as the old content::text predicate: a needle that occurs in
	// the name alone is the name filter's business, not a content hit
	// (SearchLibrary re-ranks a capped candidate list; name-only hits would
	// displace content hits beyond the cap).
	want("name-only needle is not a content hit", search(f.ctx, "alpha rep"), false)
	want("name filter", byName(f.ctx, "alpha rep"), true)
	want("name filter miss", byName(f.ctx, "beta"), false)
	want("under RLS", search(f.rls(alice), "zebra"), true)

	var same bool
	if err := f.b.db.QueryRowContext(f.ctx,
		`SELECT s.body = f.content::text AND s.name = f.name FROM flow_search s JOIN flows f ON f.id = s.flow_id WHERE s.flow_id = $1`,
		flowID).Scan(&same); err != nil {
		t.Fatalf("read flow_search: %v", err)
	}
	if !same {
		t.Error("flow_search body/name differ from the flow's content::text / name")
	}

	// Rename + edit through the OCC upsert.
	hdr, err := f.b.LoadFlowHeader(f.ctx, flowID)
	if err != nil {
		t.Fatalf("LoadFlowHeader: %v", err)
	}
	if err := f.b.SaveFlow(f.ctx, &interfaces.FlowDocument{
		ID: flowID, Name: "Beta Ledger", OwnerID: alice, Version: hdr.Version,
		Content: []byte(`{"blocks":[{"name":"giraffe"}]}`),
	}); err != nil {
		t.Fatalf("SaveFlow (rename): %v", err)
	}
	want("old content gone", search(f.ctx, "zebra"), false)
	want("new content", search(f.ctx, "giraffe"), true)
	want("old name gone", byName(f.ctx, "alpha"), false)
	want("new name", byName(f.ctx, "beta led"), true)
	want("new name is not a content hit", search(f.ctx, "Beta"), false)

	if err := f.b.DeleteFlow(f.ctx, flowID); err != nil {
		t.Fatalf("DeleteFlow: %v", err)
	}
	var left int
	if err := f.b.db.QueryRowContext(f.ctx, `SELECT count(*) FROM flow_search WHERE flow_id = $1`, flowID).Scan(&left); err != nil {
		t.Fatalf("count flow_search: %v", err)
	}
	if left != 0 {
		t.Errorf("flow_search row survived the flow's deletion")
	}
}

// TestListCollaboratorsBatch_OneArrayBind: the batch collaborator read binds
// the flow ids as one array and still answers for every flow asked (ids
// deduplicated, blanks dropped, flows without grants absent).
func TestListCollaboratorsBatch_OneArrayBind(t *testing.T) {
	f := newR2Fixture(t)
	alice, bob, carol := f.user("alice"), f.user("bob"), f.user("carol")
	f1 := f.flow("f1", alice, "", "one", "")
	f2 := f.flow("f2", alice, "", "two", "")
	f3 := f.flow("f3", alice, "", "three", "")
	f.collab(f1, bob)
	f.collab(f1, carol)
	f.collab(f2, carol)

	got, err := f.b.ListCollaboratorsBatch(f.ctx, []string{f1, "", f2, f3, f1})
	if err != nil {
		t.Fatalf("ListCollaboratorsBatch: %v", err)
	}
	users := func(id string) []string {
		out := []string{}
		for _, c := range got[id] {
			out = append(out, c.UserID)
		}
		return sortedCopy(out)
	}
	if u := users(f1); strings.Join(u, ",") != strings.Join(sortedCopy([]string{bob, carol}), ",") {
		t.Errorf("f1 collaborators = %v, want bob+carol", u)
	}
	if u := users(f2); strings.Join(u, ",") != carol {
		t.Errorf("f2 collaborators = %v, want carol", u)
	}
	if _, ok := got[f3]; ok {
		t.Errorf("f3 has no collaborators but is present: %v", got[f3])
	}
	if len(got) != 2 {
		t.Errorf("batch returned %d flows, want 2", len(got))
	}
}

// legacyGovAlertIDs runs the pre-v27 list statement — gov_alerts filtered by
// govAlertWhere's nested EXISTS, which the mutations still use — and returns
// the ids in the list's order.
func legacyGovAlertIDs(t *testing.T, b *PostgresStorageBackend, ctx context.Context, userID string, includeDismissed bool) []string {
	t.Helper()
	where := []string{}
	args := sqlArgs{}
	if !includeDismissed {
		where = append(where, "dismissed_at IS NULL")
	}
	where = append(where, govAlertWhere(userID, &args))
	q := "SELECT id FROM gov_alerts WHERE " + strings.Join(where, " AND ") + " ORDER BY created_at DESC, id ASC"
	rows, err := b.query(ctx).QueryContext(ctx, q, args...)
	if err != nil {
		t.Fatalf("legacy list: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan legacy: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("legacy list rows: %v", err)
	}
	return out
}

func legacyUnreadCount(t *testing.T, b *PostgresStorageBackend, ctx context.Context, userID string) int {
	t.Helper()
	var args sqlArgs
	vis := govAlertWhere(userID, &args)
	var n int
	if err := b.query(ctx).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gov_alerts WHERE read_at IS NULL AND dismissed_at IS NULL AND `+vis, args...).Scan(&n); err != nil {
		t.Fatalf("legacy count: %v", err)
	}
	return n
}

// TestGovernanceAlerts_VisibleFlowsFirst_MatchesLegacyPredicate: the list
// and the unread count, rewritten to start from the caller's flows, answer
// exactly as the nested-EXISTS predicate for every caller kind (owner,
// collaborator, org member, outsider, local mode), for team-wide, targeted,
// read and dismissed alerts — with and without IncludeDismissed, and under
// RLS.
func TestGovernanceAlerts_VisibleFlowsFirst_MatchesLegacyPredicate(t *testing.T) {
	f := newR2Fixture(t)
	owner, collab, member, outsider, other := f.user("owner"), f.user("collab"), f.user("member"), f.user("outsider"), f.user("other")
	org := f.org("org", owner, map[string]string{owner: "admin", member: "member"})
	f1 := f.flow("f1", owner, org, "Org flow", "")   // owner + member see it
	f2 := f.flow("f2", other, "", "Shared flow", "") // other owns, collab is a collaborator
	f3 := f.flow("f3", other, "", "Private flow", "")
	f.collab(f2, collab)

	base := time.Now().UTC().Add(-time.Hour)
	record := func(label, flowID, target string, offset int) string {
		t.Helper()
		id := f.id("alert-" + label)
		if err := f.b.RecordGovernanceAlert(f.ctx, &interfaces.GovernanceAlert{
			ID: id, FlowID: flowID, OrgID: "", Type: "drift", Title: label, Severity: "warning",
			CreatedAt: base.Add(time.Duration(offset) * time.Second), TargetUser: target,
		}); err != nil {
			t.Fatalf("RecordGovernanceAlert %s: %v", label, err)
		}
		return id // cascades with the flow
	}
	record("f1-team", f1, "", 1)
	record("f1-team-tie", f1, "", 1) // same created_at: exercises the id tiebreaker
	aMember := record("f1-to-member", f1, member, 2)
	record("f1-to-outsider", f1, outsider, 3)
	aDismissed := record("f1-dismissed", f1, "", 4)
	aRead := record("f2-read", f2, "", 5)
	record("f2-team", f2, "", 6)
	record("f3-team", f3, "", 7)
	record("f3-to-member", f3, member, 8)
	if err := f.b.DismissGovernanceAlert(f.ctx, owner, aDismissed); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	if err := f.b.MarkGovernanceAlertRead(f.ctx, other, aRead); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}

	// Only this run's alerts matter; the legacy and new lists are compared
	// after dropping everything else the database holds.
	mine := func(ids []string) []string {
		out := []string{}
		for _, id := range ids {
			if strings.HasSuffix(id, f.run) {
				out = append(out, id)
			}
		}
		return out
	}
	listNew := func(ctx context.Context, userID string, dismissed bool) []string {
		t.Helper()
		alerts, err := f.b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{UserID: userID, IncludeDismissed: dismissed, Limit: 1000})
		if err != nil {
			t.Fatalf("ListGovernanceAlerts: %v", err)
		}
		ids := []string{}
		for _, a := range alerts {
			ids = append(ids, a.ID)
		}
		return mine(ids)
	}

	for _, userID := range []string{owner, collab, member, outsider, other, ""} {
		for _, dismissed := range []bool{false, true} {
			label := fmt.Sprintf("user=%s dismissed=%v", strings.TrimSuffix(strings.TrimPrefix(userID, "r2-"), "-"+f.run), dismissed)
			got := listNew(f.ctx, userID, dismissed)
			want := mine(legacyGovAlertIDs(t, f.b, f.ctx, userID, dismissed))
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s: list = %v, want (legacy) %v", label, got, want)
			}
			if userID == "" {
				continue
			}
			// An alert on an invisible flow or targeted at someone else must
			// not leak through either form (sanity on the fixture itself).
			for _, id := range got {
				if strings.Contains(id, "f3-") && userID != other {
					t.Errorf("%s: alert on an invisible flow listed: %s", label, id)
				}
				if strings.Contains(id, "to-outsider") && userID != outsider {
					t.Errorf("%s: another user's targeted alert listed: %s", label, id)
				}
			}
		}
		gotN, err := f.b.UnreadGovernanceAlertCountFor(f.ctx, userID)
		if err != nil {
			t.Fatalf("UnreadGovernanceAlertCountFor: %v", err)
		}
		if wantN := legacyUnreadCount(t, f.b, f.ctx, userID); gotN != wantN {
			t.Errorf("user=%s: unread = %d, want (legacy) %d", userID, gotN, wantN)
		}
	}

	// Pin the expected sets explicitly too, so the comparison above is not
	// two wrong answers agreeing.
	got := listNew(f.ctx, member, false)
	if len(got) != 3 || got[0] != aMember || !strings.Contains(got[1], "f1-team") || !strings.Contains(got[2], "f1-team") {
		t.Errorf("member list = %v, want [to-member, f1-team-tie, f1-team] (newest first, id tiebreak)", got)
	}
	if n, _ := f.b.UnreadGovernanceAlertCountFor(f.ctx, collab); n != 1 {
		t.Errorf("collab unread = %d, want 1 (f2-team; f2-read is read)", n)
	}
	if n, _ := f.b.UnreadGovernanceAlertCountFor(f.ctx, outsider); n != 0 {
		t.Errorf("outsider unread = %d, want 0 (targeted alert on a flow they cannot see)", n)
	}

	// Under RLS as the member: same answer as without.
	rlsCtx := f.rls(member)
	if got := listNew(rlsCtx, member, true); len(got) != 4 {
		t.Errorf("member list under RLS (dismissed included) = %v, want 4 alerts", got)
	}
	if n, err := f.b.UnreadGovernanceAlertCountFor(rlsCtx, member); err != nil || n != 3 {
		t.Errorf("member unread under RLS = %d, %v; want 3", n, err)
	}
}
