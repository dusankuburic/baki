package contract

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// runCreateUserScenarios holds CreateUser: the first user's promotion and the duplicate email.
func runCreateUserScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("CreateUser_promotes_first_to_admin", func(t *testing.T) {
		// Only meaningful on a truly empty backend. Skip when prior state
		// exists (e.g. a Postgres DB shared across test runs).
		count, err := b.CountUsers(ctx)
		if err != nil {
			t.Fatalf("CountUsers: %v", err)
		}
		if count > 0 {
			t.Skip("backend has existing users; cannot exercise first-admin promotion")
		}

		// The bootstrap-admin rule is opt-in via context (the registration path) and gated on an
		// empty table — without the flag the first user must NOT be promoted (SSO-JIT must not let
		// whoever reaches SSO first claim admin).
		noBootstrap := &interfaces.User{
			ID:       "contract-user-0-" + runID,
			Email:    "contract-0-" + runID + "@example.com",
			Password: "hash",
		}
		if err := b.CreateUser(ctx, noBootstrap); err != nil {
			t.Fatalf("CreateUser (no bootstrap flag): %v", err)
		}
		if string(noBootstrap.Role) == "admin" {
			t.Errorf("first user without bootstrap flag: expected non-admin, got admin (SSO-path must not auto-promote)")
		}

		flagged := &interfaces.User{
			ID:       "contract-user-1-" + runID,
			Email:    "contract-1-" + runID + "@example.com",
			Password: "hash",
		}
		if err := b.CreateUser(auth.WithAllowBootstrap(ctx, true), flagged); err != nil {
			t.Fatalf("CreateUser (with bootstrap flag, non-empty table): %v", err)
		}
		if string(flagged.Role) == "admin" {
			t.Errorf("non-first user: expected non-admin, got admin (table was not empty)")
		}
	})

	run("CreateUser_duplicate_email_returns_ErrEmailExists", func(t *testing.T) {
		email := "contract-dup-" + runID + "@example.com"
		u := &interfaces.User{
			ID:       "contract-user-dup-a-" + runID,
			Email:    email,
			Password: "hash",
		}
		if err := b.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser first: %v", err)
		}
		dup := &interfaces.User{
			ID:       "contract-user-dup-b-" + runID,
			Email:    email,
			Password: "hash",
		}
		err := b.CreateUser(ctx, dup)
		if !errors.Is(err, interfaces.ErrEmailExists) {
			t.Errorf("duplicate email: expected ErrEmailExists, got %v", err)
		}
	})
}

// runUserQueryScenarios holds the user lookups, the admin list and its statistics, and the role update.
func runUserQueryScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("LoadUsersByIDs_resolves_present_and_skips_missing", func(t *testing.T) {
		u1 := &interfaces.User{ID: "contract-multi-1-" + runID, Email: "m1-" + runID + "@example.com", Password: "h"}
		u2 := &interfaces.User{ID: "contract-multi-2-" + runID, Email: "m2-" + runID + "@example.com", Password: "h"}
		if err := b.CreateUser(ctx, u1); err != nil {
			t.Fatalf("create u1: %v", err)
		}
		if err := b.CreateUser(ctx, u2); err != nil {
			t.Fatalf("create u2: %v", err)
		}

		// Includes a duplicate and a missing id; both must be handled cleanly.
		got, err := b.LoadUsersByIDs(ctx, []string{u1.ID, u2.ID, "missing-" + runID, u1.ID})
		if err != nil {
			t.Fatalf("LoadUsersByIDs: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 resolved users, got %d", len(got))
		}
		if got[u1.ID] == nil || got[u1.ID].Email != u1.Email {
			t.Errorf("u1 not resolved correctly: %+v", got[u1.ID])
		}
		if got[u2.ID] == nil || got[u2.ID].Email != u2.Email {
			t.Errorf("u2 not resolved correctly: %+v", got[u2.ID])
		}
		if _, ok := got["missing-"+runID]; ok {
			t.Errorf("missing id should be absent from result map")
		}
	})

	run("LoadUsersByIDs_empty_returns_empty_map", func(t *testing.T) {
		got, err := b.LoadUsersByIDs(ctx, nil)
		if err != nil {
			t.Fatalf("LoadUsersByIDs(nil): %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty map, got %d entries", len(got))
		}
	})

	run("QueryUsers_filters_pages_and_counts", func(t *testing.T) {
		// Every seeded email and one display name carry the run id, so the
		// assertions hold on a shared database with other users in it.
		mark := "cq-" + runID
		seed := []*interfaces.User{
			{ID: "contract-q-a-" + runID, Email: "alpha-" + mark + "@example.com", Password: "hash", DisplayName: "Ada " + mark, Role: auth.RoleMember, EmailVerified: true},
			{ID: "contract-q-b-" + runID, Email: "bravo-" + mark + "@example.com", Password: "hash", DisplayName: "Brian", Role: auth.RoleViewer},
			// No role: the admin list shows this account as unassigned.
			{ID: "contract-q-c-" + runID, Email: "charlie-" + mark + "@example.com", Password: "hash"},
		}
		for _, u := range seed {
			if err := b.SaveUser(ctx, u); err != nil {
				t.Fatalf("SaveUser %s: %v", u.ID, err)
			}
			id := u.ID
			t.Cleanup(func() { _ = b.DeleteUser(context.Background(), id) })
		}

		page, total, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: mark, Limit: 10})
		if err != nil {
			t.Fatalf("QueryUsers: %v", err)
		}
		if total != 3 || len(page) != 3 {
			t.Errorf("Q=%q: expected 3 of 3, got %d of %d", mark, len(page), total)
		}

		// Case-insensitive, and the display name counts as much as the email.
		page, total, err = b.QueryUsers(ctx, interfaces.UserQuery{Q: strings.ToUpper("ada " + mark), Limit: 10})
		if err != nil {
			t.Fatalf("QueryUsers by display name: %v", err)
		}
		if total != 1 || len(page) != 1 || page[0].ID != seed[0].ID {
			t.Errorf("display-name search: expected only %s, got %d rows (total %d)", seed[0].ID, len(page), total)
		}
		if len(page) == 1 && page[0].DisplayName != seed[0].DisplayName {
			t.Errorf("display name not returned with the page: got %q", page[0].DisplayName)
		}

		for _, tc := range []struct {
			role string
			want string
		}{
			{string(auth.RoleMember), seed[0].ID},
			{string(auth.RoleViewer), seed[1].ID},
			{interfaces.UserRoleNone, seed[2].ID},
		} {
			page, total, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: mark, Role: tc.role, Limit: 10})
			if err != nil {
				t.Fatalf("QueryUsers role=%s: %v", tc.role, err)
			}
			if total != 1 || len(page) != 1 || page[0].ID != tc.want {
				t.Errorf("role=%s: expected only %s, got %d rows (total %d)", tc.role, tc.want, len(page), total)
			}
		}

		// Paging: the total is the match count, not the page size; the pages
		// do not overlap; a page past the end is empty but never nil.
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
		seen := map[string]bool{}
		for _, u := range append(first, second...) {
			if seen[u.ID] {
				t.Errorf("paging: %s appeared on both pages", u.ID)
			}
			seen[u.ID] = true
		}
		empty, total, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: mark, Limit: 2, Offset: 10})
		if err != nil {
			t.Fatalf("QueryUsers past the end: %v", err)
		}
		if empty == nil || len(empty) != 0 || total != 3 {
			t.Errorf("past the end: expected a non-nil empty page with total 3, got %v (total %d)", empty, total)
		}
	})

	run("LoadUserStats_adds_up", func(t *testing.T) {
		stats, err := b.LoadUserStats(ctx)
		if err != nil {
			t.Fatalf("LoadUserStats: %v", err)
		}
		sum := 0
		for _, role := range []string{"admin", "member", "viewer", "guest", interfaces.UserRoleNone} {
			n, ok := stats.ByRole[role]
			if !ok {
				t.Errorf("ByRole is missing %q: %v", role, stats.ByRole)
			}
			sum += n
		}
		if sum != stats.Total {
			t.Errorf("the role buckets add up to %d, the total is %d", sum, stats.Total)
		}
		if stats.Unverified > stats.Total || stats.Locked > stats.Total {
			t.Errorf("state counts exceed the total: %+v", stats)
		}
	})

	run("UpdateUserRole_missing_returns_ErrNotFound", func(t *testing.T) {
		err := b.UpdateUserRole(ctx, "contract-missing-user-"+runID, auth.RoleMember)
		if !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("unknown user: expected ErrNotFound, got %v", err)
		}
	})
}
