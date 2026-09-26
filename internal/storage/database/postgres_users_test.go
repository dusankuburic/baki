package database

import (
	"context"
	"os"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// The admin user list and stats against a real Postgres: the SQL paths the
// contract suite cannot tell apart from the in-memory backends (the strpos
// search over email and display name, the NOT IN "none" role filter, the
// FILTER counts, the subject match on audit_events). Skipped unless
// DATABASE_URL is set. The database is shared and never reset, so every
// fixture carries a per-run mark and the assertions stay relative to it.

func openUsersTestDB(t *testing.T) *PostgresStorageBackend {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping users integration tests")
	}
	b, err := New(context.Background(), DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func runMark(prefix string) string {
	return prefix + "-" + time.Now().Format("150405.000000000")
}

func TestQueryUsers_Postgres(t *testing.T) {
	b := openUsersTestDB(t)
	ctx := context.Background()
	mark := runMark("pgq")
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(), `DELETE FROM users WHERE email LIKE '%' || $1 || '%'`, mark)
	})
	seed := []*interfaces.User{
		{ID: mark + "-a", Email: "a-" + mark + "@example.com", Password: "hash", DisplayName: "Grace " + mark, Role: auth.RoleAdmin},
		{ID: mark + "-b", Email: "b-" + mark + "@example.com", Password: "hash", Role: auth.RoleMember},
		// A role the app never issues: the "none" bucket.
		{ID: mark + "-c", Email: "c-" + mark + "@example.com", Password: "hash", Role: "legacy"},
	}
	for _, u := range seed {
		if err := b.SaveUser(ctx, u); err != nil {
			t.Fatalf("SaveUser %s: %v", u.ID, err)
		}
	}

	page, total, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: "GRACE " + mark, Limit: 5})
	if err != nil {
		t.Fatalf("QueryUsers by display name: %v", err)
	}
	if total != 1 || len(page) != 1 || page[0].ID != seed[0].ID {
		t.Fatalf("display-name search: expected only %s, got %d rows (total %d)", seed[0].ID, len(page), total)
	}
	if page[0].DisplayName != seed[0].DisplayName {
		t.Errorf("display_name is not selected: got %q", page[0].DisplayName)
	}

	page, total, err = b.QueryUsers(ctx, interfaces.UserQuery{Q: mark, Role: interfaces.UserRoleNone, Limit: 5})
	if err != nil {
		t.Fatalf("QueryUsers role=none: %v", err)
	}
	if total != 1 || len(page) != 1 || page[0].ID != seed[2].ID {
		t.Errorf("role=none: expected only %s, got %d rows (total %d)", seed[2].ID, len(page), total)
	}

	first, total, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: mark, Limit: 2})
	if err != nil {
		t.Fatalf("QueryUsers page 1: %v", err)
	}
	second, _, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: mark, Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("QueryUsers page 2: %v", err)
	}
	if total != 3 || len(first) != 2 || len(second) != 1 {
		t.Errorf("paging: expected 2 + 1 of 3, got %d + %d of %d", len(first), len(second), total)
	}
	empty, total, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: mark, Limit: 2, Offset: 10})
	if err != nil {
		t.Fatalf("QueryUsers past the end: %v", err)
	}
	if empty == nil || len(empty) != 0 || total != 3 {
		t.Errorf("past the end: expected a non-nil empty page with total 3, got %v (total %d)", empty, total)
	}
}

func TestLoadUserStats_Postgres(t *testing.T) {
	b := openUsersTestDB(t)
	ctx := context.Background()
	mark := runMark("pgs")
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(), `DELETE FROM users WHERE email LIKE '%' || $1 || '%'`, mark)
	})
	lockedUntil := time.Now().Add(time.Hour).UTC()
	seed := []*interfaces.User{
		{ID: mark + "-locked", Email: "locked-" + mark + "@example.com", Password: "hash", Role: auth.RoleMember, EmailVerified: true, LockedUntil: &lockedUntil},
		{ID: mark + "-unverified", Email: "unverified-" + mark + "@example.com", Password: "hash", Role: auth.RoleMember},
		{ID: mark + "-roleless", Email: "roleless-" + mark + "@example.com", Password: "hash", EmailVerified: true},
	}
	for _, u := range seed {
		if err := b.SaveUser(ctx, u); err != nil {
			t.Fatalf("SaveUser %s: %v", u.ID, err)
		}
	}

	// Other packages' tests share the database, so only invariants and lower
	// bounds are asserted.
	stats, err := b.LoadUserStats(ctx)
	if err != nil {
		t.Fatalf("LoadUserStats: %v", err)
	}
	sum := 0
	for _, role := range []string{"admin", "member", "viewer", "guest", interfaces.UserRoleNone} {
		sum += stats.ByRole[role]
	}
	if sum != stats.Total {
		t.Errorf("the role buckets add up to %d, the total is %d", sum, stats.Total)
	}
	if stats.ByRole[interfaces.UserRoleNone] < 1 {
		t.Errorf("the roleless account is not counted under none: %v", stats.ByRole)
	}
	if stats.Locked < 1 {
		t.Errorf("the locked account is not counted: %+v", stats)
	}
	if stats.Unverified < 1 {
		t.Errorf("the unverified account is not counted: %+v", stats)
	}
}

// TestListAuditEvents_IncludeSubject_Postgres: a user's audit trail includes
// the pre-auth events that name them as the subject only when asked, so the
// admin log sees sign-ins and lockouts while the GDPR export does not.
func TestListAuditEvents_IncludeSubject_Postgres(t *testing.T) {
	b := openUsersTestDB(t)
	ctx := context.Background()
	user := runMark("pga")
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(), `DELETE FROM audit_events WHERE id LIKE $1 || '%'`, user)
	})
	now := time.Now().UTC()
	events := []*interfaces.AuditEvent{
		{ID: user + "-actor", UserID: user, Email: "x@example.com", Action: "flow.upload", ResourceType: "flow", ResourceID: "f1", IP: "127.0.0.1", CreatedAt: now},
		{ID: user + "-subject", Action: "auth.login_failure", ResourceType: "user", ResourceID: user, IP: "10.0.0.9", CreatedAt: now},
		{ID: user + "-someone-else", Action: "auth.login", ResourceType: "user", ResourceID: user + "-other", IP: "10.0.0.8", CreatedAt: now},
	}
	if err := b.SaveAuditEvents(ctx, events); err != nil {
		t.Fatalf("SaveAuditEvents: %v", err)
	}

	plain, err := b.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: user, Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(plain) != 1 || plain[0].ID != user+"-actor" {
		t.Errorf("plain user filter: expected only the actor row, got %d rows", len(plain))
	}

	withSubject, err := b.ListAuditEvents(ctx, interfaces.AuditFilter{UserID: user, Limit: 10, IncludeSubject: true})
	if err != nil {
		t.Fatalf("ListAuditEvents IncludeSubject: %v", err)
	}
	if len(withSubject) != 2 {
		t.Errorf("IncludeSubject: expected the actor and the subject rows, got %d", len(withSubject))
	}
	for _, ev := range withSubject {
		if ev.ID == user+"-someone-else" {
			t.Errorf("IncludeSubject matched another user's row")
		}
	}
}
