package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/storage/interfaces"
)

// seedAuditRow inserts an audit row as a pre-v33 binary wrote it (no organisation, no actor), or with orgID set.
func seedAuditRow(t *testing.T, b *PostgresStorageBackend, id, resourceType, resourceID, orgID string) {
	t.Helper()
	ev := &interfaces.AuditEvent{ID: id, Action: "test.backfill", ResourceType: resourceType, ResourceID: resourceID,
		OrgID: orgID, CreatedAt: time.Now().UTC()}
	if err := b.SaveAuditEvent(context.Background(), ev); err != nil {
		t.Fatalf("seed audit row %s: %v", id, err)
	}
}

func seedFlow(t *testing.T, b *PostgresStorageBackend, id, orgID string) {
	t.Helper()
	if _, err := b.db.ExecContext(context.Background(),
		`INSERT INTO flows (id, name, owner_id, org_id) VALUES ($1, $1, 'u-backfill', $2)`, id, orgID); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
}

// auditOrgIDs reads org_id of every seeded row (NULL as "<null>").
func auditOrgIDs(t *testing.T, b *PostgresStorageBackend) map[string]string {
	t.Helper()
	rows, err := b.db.QueryContext(context.Background(), `SELECT id, org_id FROM audit_events WHERE action = 'test.backfill'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		var org sql.NullString
		if err := rows.Scan(&id, &org); err != nil {
			t.Fatal(err)
		}
		out[id] = "<null>"
		if org.Valid {
			out[id] = org.String
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBackfillAuditOrgIDs_FillsOrgAndFlowRows: an org resource's row gets its resource_id, a flow's row the
// flow's organisation; personal and deleted flows, other resources and rows that already carry an
// organisation are left as they are. A second run finds nothing to do.
func TestBackfillAuditOrgIDs_FillsOrgAndFlowRows(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	seedFlow(t, b, "flow-org", "org-a")
	seedFlow(t, b, "flow-personal", "")
	seedAuditRow(t, b, "org-row", "org", "org-b", "")
	seedAuditRow(t, b, "org-row-empty", "org", "", "")
	seedAuditRow(t, b, "org-row-set", "org", "org-b", "org-kept")
	seedAuditRow(t, b, "flow-row-1", "flow", "flow-org", "")
	seedAuditRow(t, b, "flow-row-2", "flow", "flow-org", "")
	seedAuditRow(t, b, "flow-row-personal", "flow", "flow-personal", "")
	seedAuditRow(t, b, "flow-row-deleted", "flow", "flow-gone", "")
	seedAuditRow(t, b, "user-row", "user", "u-1", "")

	n, err := b.BackfillAuditOrgIDs(ctx)
	if err != nil || n != 3 {
		t.Fatalf("backfill filled %d rows (err %v), want 3", n, err)
	}
	want := map[string]string{
		"org-row": "org-b", "org-row-empty": "<null>", "org-row-set": "org-kept",
		"flow-row-1": "org-a", "flow-row-2": "org-a", "flow-row-personal": "<null>", "flow-row-deleted": "<null>",
		"user-row": "<null>",
	}
	if got := auditOrgIDs(t, b); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("org_id after the backfill = %v\nwant %v", got, want)
	}
	if n, err := b.BackfillAuditOrgIDs(ctx); err != nil || n != 0 {
		t.Errorf("second run filled %d rows (err %v), want 0", n, err)
	}
}

// TestBackfillAuditOrgIDs_ResumesAfterInterruption: the batches a run committed before it was stopped stay
// written, and the next run fills exactly the rest.
func TestBackfillAuditOrgIDs_ResumesAfterInterruption(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	for i := range 5 {
		seedAuditRow(t, b, fmt.Sprintf("org-%d", i), "org", fmt.Sprintf("org-%d", i), "")
	}
	for i := range 4 {
		seedFlow(t, b, fmt.Sprintf("flow-%d", i), "org-f")
		seedAuditRow(t, b, fmt.Sprintf("flow-row-%d", i), "flow", fmt.Sprintf("flow-%d", i), "")
	}
	const total = 9

	ctx, cancel := context.WithCancel(context.Background())
	batches := 0
	first, err := b.backfillAuditOrgIDs(ctx, 2, 1, func() {
		if batches++; batches == 3 { // two org-row batches and one more
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || first != 5 {
		t.Fatalf("interrupted run: filled %d err %v, want 5 and context.Canceled", first, err)
	}
	filled := 0
	for _, org := range auditOrgIDs(t, b) {
		if org != "<null>" {
			filled++
		}
	}
	if filled != first {
		t.Fatalf("%d rows carry an organisation after the interrupted run, which reported %d", filled, first)
	}

	rest, err := b.backfillAuditOrgIDs(context.Background(), 2, 1, nil)
	if err != nil || first+rest != total {
		t.Fatalf("resumed run filled %d (err %v), want %d", rest, err, total-first)
	}
	for id, org := range auditOrgIDs(t, b) {
		if org == "<null>" {
			t.Errorf("%s still has no organisation", id)
		}
	}
}

// TestBackfillAuditOrgIDs_BoundsEachStatementByAuditRows: a page of busy flows is filled a bounded number of
// rows at a time (one statement per batch), so no statement grows with a flow's history and a page always
// finishes within the pool's statement_timeout.
func TestBackfillAuditOrgIDs_BoundsEachStatementByAuditRows(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seedFlow(t, b, "flow-busy", "org-busy")
	seedFlow(t, b, "flow-quiet", "org-quiet")
	for i := range 5 {
		seedAuditRow(t, b, fmt.Sprintf("busy-%d", i), "flow", "flow-busy", "")
	}
	seedAuditRow(t, b, "quiet-0", "flow", "flow-quiet", "")

	before := map[string]int{}
	statements := 0
	n, err := b.backfillAuditOrgIDs(context.Background(), 2, 10, func() {
		statements++
		filled := 0
		for _, org := range auditOrgIDs(t, b) {
			if org != "<null>" {
				filled++
			}
		}
		before[fmt.Sprint(statements)] = filled
	})
	if err != nil || n != 6 {
		t.Fatalf("backfill filled %d rows (err %v), want 6", n, err)
	}
	// One org-row statement, then the page in batches of at most 2: 2, 2, 2, 0.
	if statements != 5 {
		t.Errorf("%d statements, want 5 (one org-row batch, four flow-row batches of at most 2)", statements)
	}
	for i := 2; i <= statements; i++ {
		if step := before[fmt.Sprint(i)] - before[fmt.Sprint(i-1)]; step > 2 {
			t.Errorf("statement %d filled %d rows, more than the batch of 2", i, step)
		}
	}
	for id, org := range auditOrgIDs(t, b) {
		if want := map[bool]string{true: "org-busy", false: "org-quiet"}[id != "quiet-0"]; org != want {
			t.Errorf("%s: org %s, want %s", id, org, want)
		}
	}
}

// TestBackfillOrgIDs_SkipsWhileAnotherReplicaHoldsTheLock: with the advisory lock held by another session, the
// boot-time run does nothing; once it is free, the run fills the rows.
func TestBackfillOrgIDs_SkipsWhileAnotherReplicaHoldsTheLock(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	seedAuditRow(t, b, "org-row", "org", "org-l", "")

	other, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	const orgBackfillLockKey int64 = 0x62616b696175646f // audit's key, "bakiaudo"
	release, acquired, err := other.TryGlobalLock(ctx, orgBackfillLockKey)
	if err != nil || !acquired {
		t.Fatalf("hold the lock: acquired %v err %v", acquired, err)
	}
	if n, ran, err := audit.BackfillOrgIDs(ctx, b, b); ran || n != 0 || err != nil {
		t.Fatalf("with the lock held elsewhere: filled %d ran %v err %v, want a skip", n, ran, err)
	}
	if got := auditOrgIDs(t, b)["org-row"]; got != "<null>" {
		t.Fatalf("org-row = %q while the lock was held elsewhere", got)
	}
	release()
	if n, ran, err := audit.BackfillOrgIDs(ctx, b, b); !ran || n != 1 || err != nil {
		t.Fatalf("with the lock free: filled %d ran %v err %v, want 1 true nil", n, ran, err)
	}
}

// TestDeleteUser_AnonymisesAuditActorColumns: erasure strips the v33 personal columns (user agent, token) with
// the user id, email and IP, and keeps what describes the action (organisation, actor kind).
func TestDeleteUser_AnonymisesAuditActorColumns(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	const uid = "erase-audit-user"
	if err := b.SaveUser(ctx, &interfaces.User{ID: uid, Email: "erase@example.com", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveAuditEvent(ctx, &interfaces.AuditEvent{ID: "erase-evt", UserID: uid, Email: "erase@example.com",
		Action: "flow.save", IP: "203.0.113.7", OrgID: "org-e", ActorKind: "token", TokenID: "tok-e",
		UserAgent: "Mozilla/5.0", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteUser(ctx, uid); err != nil {
		t.Fatal(err)
	}
	var user, email, ip string
	var org, kind, token, agent sql.NullString
	if err := b.db.QueryRowContext(ctx, `SELECT user_id, email, ip, org_id, actor_kind, token_id, user_agent
		FROM audit_events WHERE id = 'erase-evt'`).Scan(&user, &email, &ip, &org, &kind, &token, &agent); err != nil {
		t.Fatal(err)
	}
	if user != "" || email != "" || ip != "" || token.Valid || agent.Valid {
		t.Errorf("personal columns survived erasure: user %q email %q ip %q token %v agent %v", user, email, ip, token, agent)
	}
	if org.String != "org-e" || kind.String != "token" {
		t.Errorf("erasure dropped the action's organisation %v or actor kind %v", org, kind)
	}
}

// TestDeleteUser_ClearsTheUserAgentOfPreAuthRows: the rows written before the user authenticated (login,
// register, password reset: no user_id, the user as their resource) lose the user agent they carry since v33;
// their other columns stay as before v33.
func TestDeleteUser_ClearsTheUserAgentOfPreAuthRows(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	const uid = "erase-preauth-user"
	if err := b.SaveUser(ctx, &interfaces.User{ID: uid, Email: "pre@example.com", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []*interfaces.AuditEvent{
		{ID: "preauth-login", Action: "auth.login", ResourceType: "user", ResourceID: uid, IP: "203.0.113.9",
			UserAgent: "Mozilla/5.0 (pre-auth)", CreatedAt: time.Now().UTC()},
		{ID: "other-user-login", Action: "auth.login", ResourceType: "user", ResourceID: "someone-else",
			IP: "203.0.113.10", UserAgent: "Mozilla/5.0 (other)", CreatedAt: time.Now().UTC()},
	} {
		if err := b.SaveAuditEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.DeleteUser(ctx, uid); err != nil {
		t.Fatal(err)
	}
	read := func(id string) (ip string, agent sql.NullString) {
		t.Helper()
		if err := b.db.QueryRowContext(ctx, `SELECT ip, user_agent FROM audit_events WHERE id = $1`, id).Scan(&ip, &agent); err != nil {
			t.Fatal(err)
		}
		return ip, agent
	}
	if ip, agent := read("preauth-login"); agent.Valid || ip != "203.0.113.9" {
		t.Errorf("the erased user's pre-auth row: ip %q agent %v, want the ip kept and no agent", ip, agent)
	}
	if ip, agent := read("other-user-login"); agent.String != "Mozilla/5.0 (other)" || ip != "203.0.113.10" {
		t.Errorf("another user's row changed: ip %q agent %v", ip, agent)
	}
}
