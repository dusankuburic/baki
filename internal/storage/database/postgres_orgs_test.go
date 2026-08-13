package database_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestSaveOrg_MemberSyncPreservesJoinedAt proves the diff-based member sync:
// re-saving an org with a changed member set must keep unchanged members'
// joined_at, drop removed members, and add new ones — not rewrite every row via
// delete-all-then-reinsert.
func TestSaveOrg_MemberSyncPreservesJoinedAt(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	suffix := time.Now().Format("150405.000000000")
	orgID := "org-membersync-" + suffix
	userA := "ms-a-" + suffix
	userB := "ms-b-" + suffix
	userC := "ms-c-" + suffix

	for _, id := range []string{userA, userB, userC} {
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: id, Email: id + "@test.com", Password: "$2a$12$testhash",
			Role: "member", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		b.DeleteOrg(ctx, orgID)
		for _, id := range []string{userA, userB, userC} {
			cleanupUser(t, b, id)
		}
	})

	// Initial: members {A (admin), B (member)}.
	if err := b.SaveOrg(ctx, &interfaces.Organisation{
		ID: orgID, Name: "Sync Org", OwnerID: userA,
		Members: []interfaces.OrgMember{
			{UserID: userA, Role: "admin"},
			{UserID: userB, Role: "member"},
		},
	}); err != nil {
		t.Fatalf("SaveOrg initial: %v", err)
	}

	loaded, err := b.LoadOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	bJoinedBefore, ok := memberJoinedAt(loaded, userB)
	if !ok {
		t.Fatalf("member B missing after initial save")
	}

	// Re-save with {B (now admin), C (member)}: A removed, B role-changed, C added.
	// (SQL joined_at has microsecond resolution; the suffix already makes this a
	// distinct wall-clock, but the point is B's stored value must not change.)
	time.Sleep(2 * time.Millisecond)
	if err := b.SaveOrg(ctx, &interfaces.Organisation{
		ID: orgID, Name: "Sync Org", OwnerID: userB,
		Members: []interfaces.OrgMember{
			{UserID: userB, Role: "admin"},
			{UserID: userC, Role: "member"},
		},
	}); err != nil {
		t.Fatalf("SaveOrg update: %v", err)
	}

	after, err := b.LoadOrg(ctx, orgID)
	if err != nil {
		t.Fatalf("LoadOrg after: %v", err)
	}
	if _, present := memberJoinedAt(after, userA); present {
		t.Error("removed member A should be gone")
	}
	if _, present := memberJoinedAt(after, userC); !present {
		t.Error("added member C should be present")
	}
	bJoinedAfter, present := memberJoinedAt(after, userB)
	if !present {
		t.Fatal("retained member B should still be present")
	}
	if !bJoinedAfter.Equal(bJoinedBefore) {
		t.Errorf("member B joined_at rewritten: before=%s after=%s (delete-reinsert regression)",
			bJoinedBefore, bJoinedAfter)
	}
	// Role change must still have taken effect via the upsert.
	if role := memberRole(after, userB); role != "admin" {
		t.Errorf("member B role = %q, want admin (upsert should update role)", role)
	}
}

// TestLoadUsersByIDs_LargeN exercises the `= ANY($1)` array path with many ids —
// the hand-built IN-list it replaced would have needed one bind param per id.
func TestLoadUsersByIDs_LargeN(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	suffix := time.Now().Format("150405.000000000")
	const n = 200
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bulk-%d-%s", i, suffix)
		ids[i] = id
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: id, Email: id + "@test.com", Password: "$2a$12$testhash",
			Role: "member", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			cleanupUser(t, b, id)
		}
	})

	// Include a duplicate and a missing id to exercise dedup + skip-missing.
	query := append([]string{ids[0], "definitely-missing-" + suffix}, ids...)
	got, err := b.LoadUsersByIDs(ctx, query)
	if err != nil {
		t.Fatalf("LoadUsersByIDs: %v", err)
	}
	if len(got) != n {
		t.Errorf("resolved %d users, want %d", len(got), n)
	}
	for _, id := range ids {
		if got[id] == nil {
			t.Fatalf("missing resolved user %s", id)
		}
	}
}

func memberJoinedAt(org *interfaces.Organisation, userID string) (time.Time, bool) {
	for _, m := range org.Members {
		if m.UserID == userID {
			return m.JoinedAt, true
		}
	}
	return time.Time{}, false
}

func memberRole(org *interfaces.Organisation, userID string) string {
	for _, m := range org.Members {
		if m.UserID == userID {
			return string(m.Role)
		}
	}
	return ""
}
