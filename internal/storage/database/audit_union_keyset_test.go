package database

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Tests for the fourth performance round's audit log statements
// (listAuditEventsSQL): the UNION ALL actor-or-subject list and the export's
// keyset walk. Semantics checked against the pre-round statement run verbatim;
// plans as the app role under RLS. DB tests are DATABASE_URL-gated.

// oldAuditListSQL is ListAuditEvents' statement before the round, verbatim
// (the OR shape for IncludeSubject), projected to the id: the oracle.
func oldAuditListSQL(filter interfaces.AuditFilter, limit, offset int) (string, []any) {
	args := []any{}
	where := []string{}
	i := 1
	if filter.UserID != "" {
		if filter.IncludeSubject {
			where = append(where, fmt.Sprintf("(user_id = $%d OR (resource_type = 'user' AND resource_id = $%d))", i, i))
		} else {
			where = append(where, fmt.Sprintf("user_id = $%d", i))
		}
		args = append(args, filter.UserID)
		i++
	}
	if filter.Action != "" {
		where = append(where, fmt.Sprintf("action = $%d", i))
		args = append(args, filter.Action)
		i++
	}
	if filter.Since != nil {
		where = append(where, fmt.Sprintf("created_at >= $%d", i))
		args = append(args, filter.Since.UTC())
		i++
	}
	q := "SELECT id FROM audit_events"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC, id ASC LIMIT $%d OFFSET $%d", i, i+1)
	return q, append(args, limit, offset)
}

// TestListAuditEventsSQL_Shapes pins the statement text without a database.
func TestListAuditEventsSQL_Shapes(t *testing.T) {
	cols := auditEventColumns
	// Action only: exactly the statement the round-3 plan test pins.
	if q, args := listAuditEventsSQL(interfaces.AuditFilter{Action: "x"}, 100, 0, nil); q != auditListByActionSQL || len(args) != 3 {
		t.Errorf("action-only statement changed:\n%s\n%v", q, args)
	}
	if q, args := listAuditEventsSQL(interfaces.AuditFilter{}, 100, 7, nil); q != "SELECT "+cols+" FROM audit_events ORDER BY created_at DESC, id ASC LIMIT $1 OFFSET $2" || fmt.Sprint(args) != "[100 7]" {
		t.Errorf("unfiltered statement:\n%s\n%v", q, args)
	}
	if q, args := listAuditEventsSQL(interfaces.AuditFilter{UserID: "u"}, 100, 0, nil); q != "SELECT "+cols+" FROM audit_events WHERE user_id = $1 ORDER BY created_at DESC, id ASC LIMIT $2 OFFSET $3" || fmt.Sprint(args) != "[u 100 0]" {
		t.Errorf("user statement:\n%s\n%v", q, args)
	}

	// Actor or subject: two index-ordered arms, each cut to LIMIT+OFFSET,
	// merged and paged; the subject arm excludes the actor's own rows.
	q, args := listAuditEventsSQL(interfaces.AuditFilter{UserID: "u", IncludeSubject: true}, 20, 40, nil)
	want := "SELECT " + cols + " FROM (" +
		"(SELECT " + cols + " FROM audit_events WHERE user_id = $1 ORDER BY created_at DESC, id ASC LIMIT $2)" +
		" UNION ALL " +
		"(SELECT " + cols + " FROM audit_events WHERE resource_type = 'user' AND resource_id = $1 AND user_id <> $1 ORDER BY created_at DESC, id ASC LIMIT $2)" +
		") u ORDER BY created_at DESC, id ASC LIMIT $3 OFFSET $4"
	if q != want {
		t.Errorf("actor-or-subject statement:\n%s\nwant\n%s", q, want)
	}
	if fmt.Sprint(args) != "[u 60 20 40]" {
		t.Errorf("actor-or-subject args = %v, want [u 60 20 40] (each arm keeps LIMIT+OFFSET rows)", args)
	}
	if strings.Contains(q, " OR ") {
		t.Errorf("the actor-or-subject statement must not fall back to the OR shape:\n%s", q)
	}
	// The extra filters apply to both arms.
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	q, args = listAuditEventsSQL(interfaces.AuditFilter{UserID: "u", IncludeSubject: true, Action: "a", Since: &since}, 10, 0, nil)
	if strings.Count(q, "AND action = $2 AND created_at >= $3") != 2 {
		t.Errorf("both arms must carry the action and since filters:\n%s", q)
	}
	if fmt.Sprint(args) != "[u a 2026-01-02 03:04:05 +0000 UTC 10 10 0]" {
		t.Errorf("filtered args = %v", args)
	}

	// Keyset continuation: the cut after (created_at, id), no OFFSET.
	after := &auditCursor{createdAt: since, id: "last"}
	q, args = listAuditEventsSQL(interfaces.AuditFilter{UserID: "u"}, 500, 0, after)
	if q != "SELECT "+cols+" FROM audit_events WHERE user_id = $1 AND created_at <= $2 AND (created_at < $2 OR id > $3) ORDER BY created_at DESC, id ASC LIMIT $4" {
		t.Errorf("keyset statement:\n%s", q)
	}
	if fmt.Sprint(args) != "[u 2026-01-02 03:04:05 +0000 UTC last 500]" {
		t.Errorf("keyset args = %v", args)
	}
}

// r4AuditFixture seeds, for one user U and another O, every actor/subject
// combination the list distinguishes, with timestamp ties whose id order
// crosses the two arms; rows are removed at cleanup.
func r4AuditFixture(t *testing.T, b *PostgresStorageBackend) (user string, t0 time.Time) {
	t.Helper()
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000000")
	user = "r4-audit-u-" + stamp
	other := user + "-o"
	t0 = time.Now().UTC().Truncate(time.Second)
	prefix := "r4-audit-" + stamp + "-"
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM audit_events WHERE id LIKE $1 || '%'`, prefix) })
	rows := []struct {
		id, actor, rtype, rid, action string
		ago                           int // seconds before t0
	}{
		{"01", user, "flow", "f1", "a", 0},   // actor U on a flow
		{"02", user, "user", user, "b", 0},   // actor U on their own account (actor = subject): tie with 01
		{"03", "", "user", user, "a", 1},     // pre-auth: no actor, subject U
		{"04", other, "user", user, "b", 2},  // O acting on U
		{"05", other, "user", other, "a", 3}, // O on O: not U's
		{"06", user, "flow", "f2", "b", 4},   // actor U
		{"05z", "", "user", user, "b", 4},    // subject U, tie with 06, id sorts BEFORE 06
		{"07", user, "user", user, "a", 5},   // actor = subject again
		{"08", other, "flow", "f3", "a", 6},  // not U's
		{"09", user, "flow", "f1", "a", 7},   // actor U
		{"10", "", "user", user, "a", 7},     // subject U, tie with 09, id sorts AFTER 09
		{"11", user, "flow", "f1", "b", 9},   // actor U
	}
	for _, r := range rows {
		if _, err := b.db.ExecContext(ctx, `
			INSERT INTO audit_events (id, user_id, action, resource_type, resource_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, prefix+r.id, r.actor, r.action, r.rtype, r.rid, t0.Add(-time.Duration(r.ago)*time.Second)); err != nil {
			t.Fatalf("seed audit row %s: %v", r.id, err)
		}
	}
	return user, t0
}

func auditIDs(events []*interfaces.AuditEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

// TestListAuditEvents_UnionAllMatchesOrShape: for every filter combination
// the rewritten list returns the same rows in the same order as the OR
// statement did — each row once, actor-equals-subject rows included.
func TestListAuditEvents_UnionAllMatchesOrShape(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	user, t0 := r4AuditFixture(t, b)
	since := t0.Add(-4 * time.Second)
	for _, subject := range []bool{false, true} {
		for _, action := range []string{"", "a"} {
			for _, sinceP := range []*time.Time{nil, &since} {
				for _, page := range [][2]int{{100, 0}, {3, 0}, {3, 2}, {2, 5}, {4, 3}, {1, 1}} {
					filter := interfaces.AuditFilter{UserID: user, IncludeSubject: subject, Action: action, Since: sinceP, Limit: page[0], Offset: page[1]}
					name := fmt.Sprintf("subject=%v action=%q since=%v limit=%d offset=%d", subject, action, sinceP != nil, page[0], page[1])
					got, err := b.ListAuditEvents(ctx, filter)
					if err != nil {
						t.Fatalf("%s: ListAuditEvents: %v", name, err)
					}
					q, args := oldAuditListSQL(filter, page[0], page[1])
					rows, err := b.db.QueryContext(ctx, q, args...)
					if err != nil {
						t.Fatalf("%s: oracle: %v", name, err)
					}
					var want []string
					func() {
						defer rows.Close()
						for rows.Next() {
							var id string
							if err := rows.Scan(&id); err != nil {
								t.Fatal(err)
							}
							want = append(want, id)
						}
						if err := rows.Err(); err != nil {
							t.Fatal(err)
						}
					}()
					if strings.Join(auditIDs(got), ",") != strings.Join(want, ",") {
						t.Errorf("%s:\n got %v\nwant %v", name, auditIDs(got), want)
					}
				}
			}
		}
	}
	// The unpaged subject list is the full ten rows of U, once each (the two
	// actor-equals-subject rows included, neither twice).
	all, err := b.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: user, IncludeSubject: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10 {
		t.Errorf("U's actor-or-subject trail has %d rows, want 10 (fixture wrong?): %v", len(all), auditIDs(all))
	}
}

// auditSpreadUser is the account the spread plan tests ask about: one of
// fifty, 2% of the rows as actor and 2% as subject — a page is a small prefix
// of a long personal history and the time index yields one hit in 25 rows.
const auditSpreadUser = "r4-spread-user-1"

// auditSpreadRows is the spread population's size; at this size the planner's
// choice between an ordered index walk and a bitmap-then-sort is a real one.
const auditSpreadRows = 100000

// seedAuditSpread writes auditSpreadRows audit rows, one per second back from
// now, across fifty actors each naming another of the fifty as subject, then
// ANALYZEs.
func seedAuditSpread(t *testing.T, b *PostgresStorageBackend) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM audit_events WHERE id LIKE 'r4-spread-%'`) })
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, user_id, action, resource_type, resource_id, created_at)
		SELECT 'r4-spread-' || g,
		       'r4-spread-user-' || (g % 50),
		       CASE WHEN g % 50 = 0 THEN 'auth.login_failure' ELSE 'flow.save' END,
		       'user',
		       'r4-spread-user-' || ((g * 7 + 3) % 50),
		       NOW() - (g || ' seconds')::interval
		FROM generate_series(1, $1::int) g ON CONFLICT (id) DO NOTHING`, auditSpreadRows); err != nil {
		t.Fatalf("seed spread audit population: %v", err)
	}
	if _, err := b.db.ExecContext(ctx, `ANALYZE audit_events`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// TestPlan_AuditActorOrSubject_MergeAppendNoBitmap: the actor-or-subject list
// is two index-ordered arms (user_created_idx, resource_created_idx) merged by
// a Merge Append — no BitmapOr, no walk of created_at_idx, every access keyed.
func TestPlan_AuditActorOrSubject_MergeAppendNoBitmap(t *testing.T) {
	b := openMigrationTestDB(t)
	seedAuditSpread(t, b)
	for _, tc := range []struct {
		name   string
		filter interfaces.AuditFilter
	}{
		{"subject", interfaces.AuditFilter{UserID: auditSpreadUser, IncludeSubject: true}},
		{"subject page 3", interfaces.AuditFilter{UserID: auditSpreadUser, IncludeSubject: true, Offset: 200}},
		{"subject with action", interfaces.AuditFilter{UserID: auditSpreadUser, IncludeSubject: true, Action: "flow.save"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, args := listAuditEventsSQL(tc.filter, 100, tc.filter.Offset, nil)
			nodes := explainAsUser(t, b, planUser, q, args)
			t.Logf("ListAuditEvents(%s) plan:\n%s", tc.name, describePlan(nodes))
			merge := false
			for _, n := range nodes {
				if n.NodeType == "Merge Append" {
					merge = true
				}
				if strings.Contains(n.NodeType, "Bitmap") {
					t.Errorf("%s: plan has a %s node (the arms must be ordered index walks):\n%s", tc.name, n.NodeType, describePlan(nodes))
				}
				if n.IndexName == "audit_events_created_at_idx" {
					t.Errorf("%s: plan walks audit_events_created_at_idx:\n%s", tc.name, describePlan(nodes))
				}
			}
			if !merge {
				t.Errorf("%s: no Merge Append in the plan:\n%s", tc.name, describePlan(nodes))
			}
			assertKeyedAccess(t, nodes, "audit_events", "ListAuditEvents "+tc.name)
			assertIndexUsed(t, nodes, "audit_events_user_created_idx", "ListAuditEvents "+tc.name)
			assertIndexUsed(t, nodes, "audit_events_resource_created_idx", "ListAuditEvents "+tc.name)
		})
	}
}

// TestPlan_OldAuditOrShapeIsBitmapOr proves the check observes the planner:
// the OR statement on the same population is a BitmapOr with a sort of the
// user's whole history, or a filtered walk of created_at_idx — shapes the
// Merge Append check rejects.
func TestPlan_OldAuditOrShapeIsBitmapOr(t *testing.T) {
	b := openMigrationTestDB(t)
	seedAuditSpread(t, b)
	q, args := oldAuditListSQL(interfaces.AuditFilter{UserID: auditSpreadUser, IncludeSubject: true}, 100, 0)
	nodes := explainAsUser(t, b, planUser, q, args)
	t.Logf("old OR shape plan:\n%s", describePlan(nodes))
	for _, n := range nodes {
		if strings.Contains(n.NodeType, "Bitmap") || n.NodeType == "Sort" || n.IndexName == "audit_events_created_at_idx" {
			return
		}
	}
	t.Fatalf("the OR shape is neither a bitmap, a sort nor a time-index walk any more; the Merge Append check cannot tell the shapes apart:\n%s", describePlan(nodes))
}

// TestPlan_AuditKeysetPage_KeyedOnUserAndTime: a continuation page of the
// export walk keys audit_events_user_created_idx on the user AND the cursor's
// created_at — resuming at the cursor instead of re-reading earlier pages —
// under a Limit, with no bitmap and no walk of the time index. (Near the
// oldest rows a bitmap of the few remaining entries, still keyed on both, is
// the planner's legitimate choice.)
func TestPlan_AuditKeysetPage_KeyedOnUserAndTime(t *testing.T) {
	b := openMigrationTestDB(t)
	seedAuditSpread(t, b)
	after := &auditCursor{createdAt: time.Now().Add(-10 * time.Minute), id: "r4-spread-600"}
	q, args := listAuditEventsSQL(interfaces.AuditFilter{UserID: auditSpreadUser}, maxAuditLimit, 0, after)
	nodes := explainAsUser(t, b, planUser, q, args)
	t.Logf("export keyset page plan:\n%s", describePlan(nodes))
	assertIndexUsed(t, nodes, "audit_events_user_created_idx", "export keyset page")
	assertKeyedAccess(t, nodes, "audit_events", "export keyset page")
	scan, _ := nodeOn(nodes, "audit_events")
	if !strings.Contains(scan.IndexCond, "user_id") || !strings.Contains(scan.IndexCond, "created_at") {
		t.Errorf("index condition %q must key on user_id AND created_at:\n%s", scan.IndexCond, describePlan(nodes))
	}
	if limitSeen, _ := limitScope(nodes[0], "audit_events"); !limitSeen {
		t.Errorf("no Limit node in the keyset page plan:\n%s", describePlan(nodes))
	}
	for _, n := range nodes {
		if strings.Contains(n.NodeType, "Bitmap") || n.IndexName == "audit_events_created_at_idx" {
			t.Errorf("keyset page plan has %s using %q:\n%s", n.NodeType, n.IndexName, describePlan(nodes))
		}
	}
}

// TestExportAuditEvents_KeysetWalk: through the fake driver, page one is the
// plain user list (OFFSET 0) and every later page binds the previous page's
// last (created_at, id) as cursor with no OFFSET; the walk carries every
// event once, in order, and stops at the first short page.
func TestExportAuditEvents_KeysetWalk(t *testing.T) {
	const total = 2*maxAuditLimit + 203
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// Three events share each second; ids ascend within a second, so the
	// list's order (created_at DESC, id ASC) is the index order.
	stamp := func(i int) time.Time { return base.Add(-time.Duration(i/3) * time.Second) }
	ident := func(i int) string { return fmt.Sprintf("ev-%05d", i) }
	cols := []string{"id", "user_id", "email", "action", "resource_type", "resource_id", "ip", "meta", "created_at"}
	var statements []string
	var cursors []auditCursor
	b, _ := newFakeBackend(t, 1, func(query string, args []driver.NamedValue) fakeResult {
		if !strings.Contains(query, "FROM audit_events") {
			return fakeResult{}
		}
		statements = append(statements, query)
		from, limit := 0, int64(0)
		if strings.Contains(query, "created_at <= $2 AND (created_at < $2 OR id > $3)") {
			ts, _ := args[1].Value.(time.Time)
			id, _ := args[2].Value.(string)
			cursors = append(cursors, auditCursor{createdAt: ts, id: id})
			limit = argInt(args[3].Value)
			from = total
			for i := 0; i < total; i++ {
				if stamp(i).Before(ts) || (stamp(i).Equal(ts) && ident(i) > id) {
					from = i
					break
				}
			}
		} else {
			limit = argInt(args[1].Value)
			from = int(argInt(args[2].Value))
		}
		var rows [][]driver.Value
		for i := from; i < total && i < from+int(limit); i++ {
			rows = append(rows, []driver.Value{ident(i), "u", "", "test", "", "", "", []byte(`{}`), stamp(i)})
		}
		return fakeResult{cols: cols, rows: rows}
	})
	got, err := b.exportAuditEvents(context.Background(), "u")
	if err != nil {
		t.Fatalf("exportAuditEvents: %v", err)
	}
	if len(got) != total {
		t.Fatalf("export carries %d audit events, want all %d", len(got), total)
	}
	for i, ev := range got {
		if ev.ID != ident(i) {
			t.Fatalf("export order broken at %d: %s", i, ev.ID)
		}
	}
	if len(statements) != 3 {
		t.Fatalf("export sent %d statements, want 3 (500 + 500 + 203)", len(statements))
	}
	if !strings.Contains(statements[0], "OFFSET") || strings.Contains(statements[0], "created_at <=") {
		t.Errorf("the first page must be the plain list:\n%s", statements[0])
	}
	for _, s := range statements[1:] {
		if strings.Contains(s, "OFFSET") {
			t.Errorf("a continuation page still pages by OFFSET:\n%s", s)
		}
	}
	if len(cursors) != 2 {
		t.Fatalf("%d continuation cursors, want 2", len(cursors))
	}
	for k, c := range cursors {
		last := (k+1)*maxAuditLimit - 1 // the previous page's last row
		if !c.createdAt.Equal(stamp(last)) || c.id != ident(last) {
			t.Errorf("cursor %d = (%v, %s), want the previous page's last row (%v, %s)", k, c.createdAt, c.id, stamp(last), ident(last))
		}
	}
}

// TestExportAuditEvents_DB_TiesAndOrder seeds more than two pages with four
// events per second and asserts the export carries every event once, in the
// list's order — the keyset cut on a tie resumes after the id.
func TestExportAuditEvents_DB_TiesAndOrder(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000000")
	user := "r4-export-" + stamp
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM audit_events WHERE user_id = $1`, user) })
	const total = 2*maxAuditLimit + 3
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, user_id, action, created_at)
		SELECT $1 || '-' || lpad(g::text, 5, '0'), $1, 'test', NOW() - ((g / 4) || ' seconds')::interval
		FROM generate_series(1, $2::int) g`, user, total); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := b.exportAuditEvents(ctx, user)
	if err != nil {
		t.Fatalf("exportAuditEvents: %v", err)
	}
	rows, err := b.db.QueryContext(ctx, `SELECT id FROM audit_events WHERE user_id = $1 ORDER BY created_at DESC, id ASC`, user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var want []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(want) != total {
		t.Fatalf("seeded %d rows, want %d", len(want), total)
	}
	if strings.Join(auditIDs(got), ",") != strings.Join(want, ",") {
		t.Errorf("export (%d rows) differs from the ordered trail (%d rows)", len(got), len(want))
	}
}
