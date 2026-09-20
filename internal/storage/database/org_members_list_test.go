package database_test

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_ListOrgsForUser_CarriesMembers: /api/orgs is the only org
// endpoint the app calls, and the organisations panel reads org.members to
// decide who is an admin and to render the roster. The list left Members nil
// "to keep it fast", so the panel hit a nil list on its first row for anyone
// who belongs to an org.
func TestPostgres_ListOrgsForUser_CarriesMembers(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	run := itoa(time.Now().UnixNano())

	owner, member := "orgm-owner-"+run, "orgm-member-"+run
	for _, u := range []*interfaces.User{
		{ID: owner, Email: "orgm-owner-" + run + "@example.com", Password: "x"},
		{ID: member, Email: "orgm-member-" + run + "@example.com", Password: "x"},
	} {
		if err := b.SaveUser(ctx, u); err != nil {
			t.Fatalf("SaveUser: %v", err)
		}
	}
	orgID := "orgm-org-" + run
	t.Cleanup(func() { _ = b.DeleteOrg(ctx, orgID); cleanupUser(t, b, owner); cleanupUser(t, b, member) })

	if err := b.SaveOrg(ctx, &interfaces.Organisation{
		ID: orgID, Name: "Roster Org", OwnerID: owner,
		Members: []interfaces.OrgMember{{UserID: owner, Role: "admin"}, {UserID: member, Role: "member"}},
	}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}

	for _, who := range []string{owner, member} {
		orgs, err := b.ListOrgsForUser(ctx, who)
		if err != nil {
			t.Fatalf("ListOrgsForUser(%s): %v", who, err)
		}
		var found *interfaces.Organisation
		for _, o := range orgs {
			if o.ID == orgID {
				found = o
			}
		}
		if found == nil {
			t.Fatalf("%s should see the org they belong to", who)
		}
		if len(found.Members) != 2 {
			t.Errorf("%s: listed org must carry its roster, got %d member(s)", who, len(found.Members))
		}
		roles := map[string]string{}
		for _, m := range found.Members {
			roles[m.UserID] = string(m.Role)
		}
		if roles[owner] != "admin" || roles[member] != "member" {
			t.Errorf("roles in the listed org = %v", roles)
		}
	}
}
