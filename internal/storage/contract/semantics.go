package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// refreshTokenStore is the session store of the backends that keep refresh tokens (service.RefreshTokenStore).
type refreshTokenStore interface {
	StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time, userAgent, ip string) error
	IsRefreshTokenValid(ctx context.Context, jti string) (bool, error)
	RevokeRefreshToken(ctx context.Context, jti string) error
	VerifyAndRevokeRefreshToken(ctx context.Context, jti string) (*interfaces.RefreshTokenInfo, error)
	RevokeUserRefreshTokens(ctx context.Context, userID string) error
	ListUserRefreshTokens(ctx context.Context, userID string) ([]interfaces.RefreshTokenInfo, error)
	RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error
}

// flowIDs returns the ids of flows, in order.
func flowIDs(flows []*interfaces.FlowDocument) []string {
	ids := make([]string, 0, len(flows))
	for _, f := range flows {
		ids = append(ids, f.ID)
	}
	return ids
}

// runSemanticsScenarios holds the stores' behaviour beyond return shapes: visibility, what a write stamps or
// keeps, ordering, defaults, cascades and ErrNotFound. Every fixture is scoped by runID, so the scenarios hold
// on a database other runs have written to.
func runSemanticsScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	createUser := func(t *testing.T, name string) string {
		t.Helper()
		id := "contract-" + name + "-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash"}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), id) })
		return id
	}
	saveFlow := func(t *testing.T, f *interfaces.FlowDocument) {
		t.Helper()
		if f.Content == nil {
			f.Content = json.RawMessage(`{}`)
		}
		if err := b.SaveFlow(ctx, f); err != nil {
			t.Fatalf("SaveFlow %s: %v", f.ID, err)
		}
		id := f.ID
		t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), id) })
	}

	run("Users_lookups_and_updates", func(t *testing.T) {
		id := "contract-uupd-" + runID
		email := id + "@example.com"
		u := &interfaces.User{ID: id, Email: email, Password: "hash-1", Role: auth.RoleMember}
		if err := b.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), id) })
		if u.CreatedAt.IsZero() || u.UpdatedAt.IsZero() {
			t.Errorf("CreateUser did not stamp the user's timestamps: %+v", u)
		}

		got, err := b.LoadUserByEmail(ctx, strings.ToUpper(email))
		if err != nil || got.ID != id || got.Password != "hash-1" || got.Role != auth.RoleMember {
			t.Fatalf("LoadUserByEmail(upper-cased) = %+v, %v; want the full row of %s", got, err, id)
		}

		other := &interfaces.User{ID: "contract-uupd-other-" + runID, Email: "contract-uupd-other-" + runID + "@example.com", Password: "h"}
		if err := b.SaveUser(ctx, other); err != nil {
			t.Fatalf("SaveUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), other.ID) })
		other.Email = email
		if err := b.SaveUser(ctx, other); !errors.Is(err, interfaces.ErrEmailExists) {
			t.Errorf("SaveUser with another user's email: got %v, want ErrEmailExists", err)
		}

		if err := b.UpdateUserProfile(ctx, id, "Ada", "https://example.com/ada.png"); err != nil {
			t.Fatalf("UpdateUserProfile: %v", err)
		}
		if err := b.UpdateUserProfile(ctx, "contract-uupd-missing-"+runID, "x", ""); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("UpdateUserProfile(missing): got %v, want ErrNotFound", err)
		}

		// A password change also lifts a lockout.
		got, err = b.LoadUserByID(ctx, id)
		if err != nil {
			t.Fatalf("LoadUserByID: %v", err)
		}
		lockedUntil := time.Now().Add(time.Hour).UTC()
		got.FailedLoginAttempts, got.LockedUntil = 3, &lockedUntil
		if err := b.SaveUser(ctx, got); err != nil {
			t.Fatalf("SaveUser (lock): %v", err)
		}
		if err := b.UpdateUserPassword(ctx, id, "hash-2"); err != nil {
			t.Fatalf("UpdateUserPassword: %v", err)
		}
		if err := b.SetUserEmailVerified(ctx, id); err != nil {
			t.Fatalf("SetUserEmailVerified: %v", err)
		}
		got, err = b.LoadUserByID(ctx, id)
		if err != nil {
			t.Fatalf("LoadUserByID: %v", err)
		}
		if got.Password != "hash-2" || got.FailedLoginAttempts != 0 || got.LockedUntil != nil {
			t.Errorf("after the password change: password %q, %d failed attempts, locked until %v; want hash-2, 0, none",
				got.Password, got.FailedLoginAttempts, got.LockedUntil)
		}
		if got.DisplayName != "Ada" || got.AvatarURL != "https://example.com/ada.png" || !got.EmailVerified {
			t.Errorf("profile or verification lost: %+v", got)
		}
	})

	run("User_lists_omit_the_password_hash", func(t *testing.T) {
		mark := "cpw-" + runID
		u := &interfaces.User{ID: "contract-pw-" + runID, Email: mark + "@example.com", Password: "secret-hash", DisplayName: "Pat"}
		if err := b.SaveUser(ctx, u); err != nil {
			t.Fatalf("SaveUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), u.ID) })
		page, _, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: mark, Limit: 10})
		if err != nil || len(page) != 1 {
			t.Fatalf("QueryUsers = %v, %v; want the one user", page, err)
		}
		if page[0].Password != "" || page[0].DisplayName != "Pat" {
			t.Errorf("QueryUsers row: password %q, display name %q; want no hash and the name", page[0].Password, page[0].DisplayName)
		}
		byID, err := b.LoadUsersByIDs(ctx, []string{u.ID, ""})
		if err != nil || byID[u.ID] == nil {
			t.Fatalf("LoadUsersByIDs = %v, %v", byID, err)
		}
		if byID[u.ID].Password != "" || byID[u.ID].Email != u.Email {
			t.Errorf("LoadUsersByIDs row = %+v; want the email without the hash", byID[u.ID])
		}
	})

	run("Settings_user_and_org_slots_are_separate_with_defaults", func(t *testing.T) {
		user := createUser(t, "uslot")
		fresh := createUser(t, "uslot-fresh")
		orgID := "contract-oslot-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "Slot Org", OwnerID: user}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), orgID) })

		// A user without settings reads the application defaults; an org without settings reads a bare version.
		defaults := models.DefaultSettings()
		us, err := b.LoadUserSettings(ctx, fresh)
		if err != nil || us.Appearance.Density != defaults.Appearance.Density || us.Layout.SidebarWidth != defaults.Layout.SidebarWidth {
			t.Fatalf("LoadUserSettings(no row) = %+v, %v; want the application defaults", us, err)
		}
		orgSet, err := b.LoadOrgSettings(ctx, orgID)
		if err != nil || orgSet.Version != 1 || orgSet.Appearance.Density != "" || orgSet.Analysis.Rules != nil {
			t.Fatalf("LoadOrgSettings(no row) = %+v, %v; want {Version: 1}", orgSet, err)
		}

		if err := b.SaveUserSettings(ctx, user, &interfaces.AppSettings{Version: 1, Appearance: interfaces.AppearanceSettings{Density: "compact"}}); err != nil {
			t.Fatalf("SaveUserSettings: %v", err)
		}
		if err := b.SaveOrgSettings(ctx, orgID, &interfaces.AppSettings{Version: 1, Appearance: interfaces.AppearanceSettings{Density: "spacious"}}); err != nil {
			t.Fatalf("SaveOrgSettings: %v", err)
		}
		if got, _ := b.LoadUserSettings(ctx, user); got == nil || got.Appearance.Density != "compact" {
			t.Errorf("user slot = %+v, want compact", got)
		}
		if got, _ := b.LoadOrgSettings(ctx, orgID); got == nil || got.Appearance.Density != "spacious" {
			t.Errorf("org slot = %+v, want spacious", got)
		}
		if got, _ := b.LoadUserSettings(ctx, fresh); got == nil || got.Appearance.Density != defaults.Appearance.Density {
			t.Errorf("another user's slot = %+v, want the defaults", got)
		}
	})

	run("Orgs_list_for_member_keep_joining_time_and_delete", func(t *testing.T) {
		owner, member, outsider := "contract-oo-"+runID, "contract-om-"+runID, "contract-ox-"+runID
		orgID := "contract-orgl-" + runID
		joined := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		org := &interfaces.Organisation{ID: orgID, Name: "List Org", OwnerID: owner, Members: []interfaces.OrgMember{
			{UserID: owner, Role: auth.RoleAdmin, JoinedAt: joined},
			{UserID: member, Role: auth.RoleMember},
		}}
		if err := b.SaveOrg(ctx, org); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), orgID) })
		if org.CreatedAt.IsZero() || org.UpdatedAt.IsZero() {
			t.Errorf("SaveOrg did not stamp the org's timestamps: %+v", org)
		}

		find := func(t *testing.T, userID string) *interfaces.Organisation {
			t.Helper()
			orgs, err := b.ListOrgsForUser(ctx, userID)
			if err != nil {
				t.Fatalf("ListOrgsForUser(%s): %v", userID, err)
			}
			for _, o := range orgs {
				if o.ID == orgID {
					return o
				}
			}
			return nil
		}
		listed := find(t, member)
		if listed == nil || len(listed.Members) != 2 || listed.Members[0].UserID != owner {
			t.Fatalf("member's org list entry = %+v; want the org with both members, the owner (joined first) first", listed)
		}
		if find(t, outsider) != nil {
			t.Errorf("an outsider's org list includes the org")
		}

		// Saving the roster again keeps each member's joining time; a member left out leaves.
		org.Members = []interfaces.OrgMember{{UserID: owner, Role: auth.RoleAdmin}}
		if err := b.SaveOrg(ctx, org); err != nil {
			t.Fatalf("SaveOrg (roster): %v", err)
		}
		loaded, err := b.LoadOrg(ctx, orgID)
		if err != nil || len(loaded.Members) != 1 || !loaded.Members[0].JoinedAt.Equal(joined) {
			t.Fatalf("after the roster save: %+v, %v; want the owner alone, joined at %v", loaded, err, joined)
		}
		if find(t, member) != nil {
			t.Errorf("a removed member still lists the org")
		}

		// Deleting the org takes its settings with it, and is idempotent.
		if err := b.SaveOrgSettings(ctx, orgID, &interfaces.AppSettings{Version: 1, Appearance: interfaces.AppearanceSettings{Density: "compact"}}); err != nil {
			t.Fatalf("SaveOrgSettings: %v", err)
		}
		if err := b.DeleteOrg(ctx, orgID); err != nil {
			t.Fatalf("DeleteOrg: %v", err)
		}
		if _, err := b.LoadOrg(ctx, orgID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("LoadOrg after delete: %v, want ErrNotFound", err)
		}
		if find(t, owner) != nil {
			t.Errorf("the owner still lists the deleted org")
		}
		if err := b.DeleteOrg(ctx, orgID); err != nil {
			t.Errorf("DeleteOrg (missing): %v, want nil", err)
		}
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "Again", OwnerID: owner}); err != nil {
			t.Fatalf("SaveOrg (recreate): %v", err)
		}
		if got, _ := b.LoadOrgSettings(ctx, orgID); got == nil || got.Appearance.Density != "" {
			t.Errorf("a recreated org reads its predecessor's settings: %+v", got)
		}
	})

	run("Collaborators_grant_list_update_remove", func(t *testing.T) {
		owner, alice, bob := createUser(t, "cowner"), createUser(t, "calice"), createUser(t, "cbob")
		flowID := "contract-collab-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: flowID, Name: "Shared", OwnerID: owner})

		t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		// The email comes from the user, not from the grant.
		if err := b.AddCollaborator(ctx, flowID, &interfaces.Collaborator{UserID: alice, Email: "ignored@example.com", Permission: "viewer", GrantedAt: t0}); err != nil {
			t.Fatalf("AddCollaborator alice: %v", err)
		}
		if err := b.AddCollaborator(ctx, flowID, &interfaces.Collaborator{UserID: bob, Permission: "viewer", GrantedAt: t0.Add(-time.Minute)}); err != nil {
			t.Fatalf("AddCollaborator bob: %v", err)
		}
		list, err := b.ListCollaborators(ctx, flowID, 0)
		if err != nil || len(list) != 2 || list[0].UserID != bob || list[1].UserID != alice {
			t.Fatalf("ListCollaborators = %+v, %v; want bob then alice (grant order)", list, err)
		}
		if list[1].Email != alice+"@example.com" {
			t.Errorf("alice's email = %q, want her account's", list[1].Email)
		}
		if one, _ := b.ListCollaborators(ctx, flowID, 1); len(one) != 1 || one[0].UserID != bob {
			t.Errorf("ListCollaborators(limit 1) = %+v, want bob", one)
		}

		// A second grant replaces the first.
		if err := b.AddCollaborator(ctx, flowID, &interfaces.Collaborator{UserID: alice, Permission: "editor", GrantedAt: t0}); err != nil {
			t.Fatalf("AddCollaborator (again): %v", err)
		}
		if err := b.UpdateCollaborator(ctx, flowID, bob, "editor"); err != nil {
			t.Fatalf("UpdateCollaborator: %v", err)
		}
		if err := b.UpdateCollaborator(ctx, flowID, "contract-nobody-"+runID, "editor"); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("UpdateCollaborator(no grant): %v, want ErrNotFound", err)
		}
		batch, err := b.ListCollaboratorsBatch(ctx, []string{flowID, "contract-no-flow-" + runID, flowID, ""})
		if err != nil || len(batch) != 1 || len(batch[flowID]) != 2 {
			t.Fatalf("ListCollaboratorsBatch = %+v, %v; want only the shared flow, with 2 grants", batch, err)
		}
		for _, c := range batch[flowID] {
			if c.Permission != "editor" {
				t.Errorf("%s's permission = %q, want editor", c.UserID, c.Permission)
			}
		}

		// The grant is a way to see the flow; SharedOnly is that way alone.
		listIDs := func(f interfaces.FlowFilter) []string {
			flows, err := b.ListFlows(ctx, f)
			if err != nil {
				t.Fatalf("ListFlows(%+v): %v", f, err)
			}
			return flowIDs(flows)
		}
		if got := listIDs(interfaces.FlowFilter{UserID: alice}); !slices.Equal(got, []string{flowID}) {
			t.Errorf("collaborator's flows = %v, want the shared flow", got)
		}
		if got := listIDs(interfaces.FlowFilter{UserID: alice, SharedOnly: true}); !slices.Equal(got, []string{flowID}) {
			t.Errorf("collaborator's shared flows = %v, want the shared flow", got)
		}
		if got := listIDs(interfaces.FlowFilter{UserID: owner, SharedOnly: true}); len(got) != 0 {
			t.Errorf("owner's shared-with-me flows = %v, want none", got)
		}

		if err := b.RemoveCollaborator(ctx, flowID, alice); err != nil {
			t.Fatalf("RemoveCollaborator: %v", err)
		}
		if err := b.RemoveCollaborator(ctx, flowID, alice); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("RemoveCollaborator (again): %v, want ErrNotFound", err)
		}
		if got := listIDs(interfaces.FlowFilter{UserID: alice}); len(got) != 0 {
			t.Errorf("a removed collaborator still lists %v", got)
		}
	})

	run("ListFlows_scopes_filters_sorts_and_counts_alike", func(t *testing.T) {
		owner, other := "contract-lfo-"+runID, "contract-lfx-"+runID
		orgA, orgB := "contract-lfa-"+runID, "contract-lfb-"+runID
		id := func(n string) string { return "contract-lf-" + runID + "-" + n }
		before := time.Now().Add(-time.Minute)
		for _, f := range []*interfaces.FlowDocument{
			{ID: id("1"), Name: "Bravo report", OwnerID: owner, Metadata: interfaces.FlowMetadata{BlockCount: 3}},
			{ID: id("2"), Name: "alpha Report", OwnerID: owner, OrganizationID: orgA, Metadata: interfaces.FlowMetadata{BlockCount: 1}},
			{ID: id("3"), Name: "Charlie", OwnerID: other, OrganizationID: orgA, Metadata: interfaces.FlowMetadata{BlockCount: 4}},
			{ID: id("4"), Name: "Delta", OwnerID: other, OrganizationID: orgB, Metadata: interfaces.FlowMetadata{BlockCount: 2}},
		} {
			saveFlow(t, f)
		}
		for flow, tags := range map[string][]string{id("1"): {"prod"}, id("2"): {"eu_west"}, id("3"): {"prod", "eu"}, id("4"): {"eu-west"}} {
			if err := b.UpdateFlowTags(ctx, flow, tags); err != nil {
				t.Fatalf("UpdateFlowTags %s: %v", flow, err)
			}
		}
		both := []string{orgA, orgB}
		for _, tc := range []struct {
			name   string
			filter interfaces.FlowFilter
			want   []string
		}{
			{"owner", interfaces.FlowFilter{UserID: owner}, []string{id("1"), id("2")}},
			{"owner and org", interfaces.FlowFilter{UserID: owner, OrganizationID: orgA}, []string{id("1"), id("2"), id("3")}},
			{"owner and orgs", interfaces.FlowFilter{UserID: owner, OrganizationIDs: both}, []string{id("1"), id("2"), id("3"), id("4")}},
			{"org", interfaces.FlowFilter{OrganizationID: orgA}, []string{id("2"), id("3")}},
			{"orgs", interfaces.FlowFilter{OrganizationIDs: []string{orgB}}, []string{id("4")}},
			{"name query", interfaces.FlowFilter{UserID: owner, OrganizationIDs: both, Query: "REPORT"}, []string{id("1"), id("2")}},
			{"any tag", interfaces.FlowFilter{UserID: owner, OrganizationIDs: both, Tags: []string{"PROD", "none"}}, []string{id("1"), id("3")}},
			{"invalid tag", interfaces.FlowFilter{UserID: owner, OrganizationIDs: both, Tags: []string{"no spaces"}}, nil},
			// "_" is a character of the tag, not a wildcard: "eu_west" does not match "eu-west".
			{"tag with an underscore", interfaces.FlowFilter{UserID: owner, OrganizationIDs: both, Tags: []string{"eu_west"}}, []string{id("2")}},
			{"created after", interfaces.FlowFilter{UserID: owner, CreatedAfter: &before}, []string{id("1"), id("2")}},
			{"created before", interfaces.FlowFilter{UserID: owner, CreatedBefore: &before}, nil},
			{"after id", interfaces.FlowFilter{UserID: owner, OrganizationIDs: both, AfterID: id("2")}, []string{id("3"), id("4")}},
			{"shared only, nothing shared", interfaces.FlowFilter{UserID: owner, SharedOnly: true}, nil},
			{"shared only without a user", interfaces.FlowFilter{SharedOnly: true}, nil},
		} {
			flows, err := b.ListFlows(ctx, tc.filter)
			if err != nil {
				t.Fatalf("%s: ListFlows: %v", tc.name, err)
			}
			got := flowIDs(flows)
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("%s: ListFlows = %v, want %v", tc.name, got, tc.want)
			}
			if n, err := b.CountFlows(ctx, tc.filter); err != nil || n != len(tc.want) {
				t.Errorf("%s: CountFlows = %d, %v; want %d", tc.name, n, err, len(tc.want))
			}
		}

		all := interfaces.FlowFilter{UserID: owner, OrganizationIDs: both}
		for _, tc := range []struct {
			sort interfaces.FlowSort
			want []string
		}{
			{interfaces.FlowSortNameAsc, []string{id("2"), id("1"), id("3"), id("4")}},
			{interfaces.FlowSortNameDesc, []string{id("4"), id("3"), id("1"), id("2")}},
			{interfaces.FlowSortBlocksDesc, []string{id("3"), id("1"), id("4"), id("2")}},
			{interfaces.FlowSortIDAsc, []string{id("1"), id("2"), id("3"), id("4")}},
		} {
			f := all
			f.SortBy = tc.sort
			flows, err := b.ListFlows(ctx, f)
			if err != nil {
				t.Fatalf("ListFlows(sort %d): %v", tc.sort, err)
			}
			if got := flowIDs(flows); !slices.Equal(got, tc.want) {
				t.Errorf("sort %d: %v, want %v", tc.sort, got, tc.want)
			}
		}
		f := all
		f.SortBy, f.Limit, f.Offset = interfaces.FlowSortIDAsc, 2, 1
		if flows, err := b.ListFlows(ctx, f); err != nil || !slices.Equal(flowIDs(flows), []string{id("2"), id("3")}) {
			t.Errorf("limit 2 offset 1 = %v, %v; want %v", flowIDs(flows), err, []string{id("2"), id("3")})
		}
		if n, _ := b.CountFlows(ctx, f); n != 4 {
			t.Errorf("CountFlows ignores Limit and Offset: got %d, want 4", n)
		}
	})

	run("SaveFlow_inserts_at_version_0_checks_every_version_and_keeps_ownership", func(t *testing.T) {
		owner, orgID := "contract-sv-owner-"+runID, "contract-sv-org-"+runID
		flowID := "contract-sv-" + runID
		flow := &interfaces.FlowDocument{ID: flowID, Name: "v0", OwnerID: owner, OrganizationID: orgID, Version: 7}
		saveFlow(t, flow)
		if flow.Version != 0 || flow.UpdatedAt.IsZero() {
			t.Fatalf("a new flow saved at version %d (updated %v), want 0 and a stamp", flow.Version, flow.UpdatedAt)
		}
		update := &interfaces.FlowDocument{ID: flowID, Name: "v1", OwnerID: "contract-sv-thief-" + runID, Content: json.RawMessage(`{}`)}
		if err := b.SaveFlow(ctx, update); err != nil {
			t.Fatalf("update expecting version 0: %v", err)
		}
		if update.Version != 1 {
			t.Errorf("updated version = %d, want 1", update.Version)
		}
		got, err := b.LoadFlow(ctx, flowID)
		if err != nil || got.Name != "v1" || got.Version != 1 || got.OwnerID != owner || got.OrganizationID != orgID {
			t.Fatalf("after the update: %+v, %v; want v1 at version 1, owner and org unchanged", got, err)
		}
		update.Version = 0
		if err := b.SaveFlow(ctx, update); !errors.Is(err, interfaces.ErrVersionConflict) {
			t.Errorf("a save expecting version 0 of a flow at 1: %v, want ErrVersionConflict", err)
		}
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: "contract/" + runID, Name: "slash"}); err == nil {
			_ = b.DeleteFlow(ctx, "contract/"+runID)
			t.Errorf("a flow id with '/' was accepted")
		}
	})

	run("DeleteFlow_cascades_and_a_missing_flow_is_ErrNotFound", func(t *testing.T) {
		flowID := "contract-del-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: flowID, Name: "Doomed", OwnerID: "contract-del-owner-" + runID, Content: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
		msgs := []interfaces.ChatMessage{{ID: "m1", Role: "user", Content: "hi", Timestamp: time.Now().UTC().Format(time.RFC3339)}}
		if err := b.SaveConversation(ctx, flowID, "scope", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: flowID, FindingKey: "r:b", Status: "resolved"}); err != nil {
			t.Fatalf("SetFindingStatus: %v", err)
		}
		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: flowID, Keys: []string{"r:b"}}); err != nil {
			t.Fatalf("SetFlowBaseline: %v", err)
		}
		if err := b.SaveFlowVersion(ctx, &interfaces.FlowVersion{ID: "contract-del-ver-" + runID, FlowID: flowID,
			Content: json.RawMessage(`{}`), CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("SaveFlowVersion: %v", err)
		}
		if err := b.AddCollaborator(ctx, flowID, &interfaces.Collaborator{UserID: createUser(t, "cdel"), Permission: "viewer"}); err != nil {
			t.Fatalf("AddCollaborator: %v", err)
		}
		if err := b.AddFindingComment(ctx, &interfaces.FindingComment{FlowID: flowID, FindingKey: "r:b", AuthorID: "contract-del-owner-" + runID, Body: "b"}); err != nil {
			t.Fatalf("AddFindingComment: %v", err)
		}
		shareHash := "contract-del-share-" + runID
		if err := b.CreateShareToken(ctx, &interfaces.ShareToken{FlowID: flowID, TokenHash: shareHash, CreatedBy: "contract-del-owner-" + runID}); err != nil {
			t.Fatalf("CreateShareToken: %v", err)
		}
		if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: flowID, HealthScore: 80, AnalyzedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("SaveFlowAnalysis: %v", err)
		}
		if err := b.DeleteFlow(ctx, flowID); err != nil {
			t.Fatalf("DeleteFlow: %v", err)
		}
		if got, _ := b.ListFlowVersions(ctx, flowID, 10); len(got) != 0 {
			t.Errorf("the flow's versions survived: %d", len(got))
		}
		if got, _ := b.ListCollaborators(ctx, flowID, 0); len(got) != 0 {
			t.Errorf("the flow's grants survived: %+v", got)
		}
		if got, _ := b.ListFindingComments(ctx, flowID, "r:b"); len(got) != 0 {
			t.Errorf("the flow's comments survived: %d", len(got))
		}
		if _, err := b.GetShareTokenByHash(ctx, shareHash); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the flow's share link: %v, want ErrNotFound", err)
		}
		if got, _ := b.LoadFlowHealth(ctx, flowID); got != nil {
			t.Errorf("the flow's health survived: %+v", got)
		}
		if _, err := b.LoadFlow(ctx, flowID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("LoadFlow after delete: %v, want ErrNotFound", err)
		}
		if got, _ := b.LoadConversation(ctx, flowID, "scope"); len(got) != 0 {
			t.Errorf("the flow's conversation survived: %d messages", len(got))
		}
		if got, _ := b.ListFindingStatuses(ctx, flowID); len(got) != 0 {
			t.Errorf("the flow's triage survived: %+v", got)
		}
		if got, _ := b.GetFlowBaseline(ctx, flowID); got != nil {
			t.Errorf("the flow's baseline survived: %+v", got)
		}
		if err := b.DeleteFlow(ctx, flowID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("DeleteFlow (missing): %v, want ErrNotFound", err)
		}
		if err := b.UpdateFlowTags(ctx, flowID, []string{"x"}); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("UpdateFlowTags (missing): %v, want ErrNotFound", err)
		}
	})

	run("FlowVersions_numbered_listed_newest_first_and_loaded", func(t *testing.T) {
		flowID := "contract-ver-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: flowID, Name: "Versioned", OwnerID: "contract-ver-owner-" + runID})
		for i := 1; i <= 3; i++ {
			v := &interfaces.FlowVersion{
				ID: fmt.Sprintf("contract-ver-%s-%d", runID, i), FlowID: flowID, Version: 99,
				Comment: fmt.Sprintf("v%d", i), Content: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
				CreatedBy: "contract-ver-owner-" + runID, CreatedAt: time.Now().UTC(),
			}
			if err := b.SaveFlowVersion(ctx, v); err != nil {
				t.Fatalf("SaveFlowVersion %d: %v", i, err)
			}
			if v.Version != i {
				t.Errorf("snapshot %d numbered %d", i, v.Version)
			}
		}
		list, err := b.ListFlowVersions(ctx, flowID, 2)
		if err != nil || len(list) != 2 || list[0].Version != 3 || list[1].Version != 2 {
			t.Fatalf("ListFlowVersions(limit 2) = %+v, %v; want versions 3 and 2", list, err)
		}
		if len(list[0].Content) != 0 || list[0].Comment != "v3" {
			t.Errorf("listed snapshot = %+v; want its comment without the content", list[0])
		}
		v2, err := b.LoadFlowVersion(ctx, flowID, 2)
		if err != nil {
			t.Fatalf("LoadFlowVersion: %v", err)
		}
		var content struct{ N int }
		if err := json.Unmarshal(v2.Content, &content); err != nil || content.N != 2 || v2.Comment != "v2" {
			t.Errorf("snapshot 2 = %+v (%s); want comment v2 with its content", v2, v2.Content)
		}
		if _, err := b.LoadFlowVersion(ctx, flowID, 9); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("LoadFlowVersion(missing): %v, want ErrNotFound", err)
		}
		if err := b.SaveFlowVersion(ctx, &interfaces.FlowVersion{ID: "contract-ver-orphan-" + runID, FlowID: "contract-no-flow-" + runID}); err == nil {
			t.Errorf("a snapshot of a missing flow was saved")
		}
		if none, err := b.ListFlowVersions(ctx, "contract-no-flow-"+runID, 0); err != nil || none == nil || len(none) != 0 {
			t.Errorf("ListFlowVersions(no history) = %v, %v; want a non-nil empty list", none, err)
		}
	})

	run("FindingComments_thread_order_and_author_rules", func(t *testing.T) {
		flowID := "contract-cmt-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: flowID, Name: "Commented", OwnerID: "contract-cmt-owner-" + runID})
		t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		second := &interfaces.FindingComment{FlowID: flowID, FindingKey: "r:b", AuthorID: "bob", Body: "second", CreatedAt: t0.Add(time.Second)}
		first := &interfaces.FindingComment{FlowID: flowID, FindingKey: "r:b", AuthorID: "alice", AuthorName: "Alice", Body: "first", CreatedAt: t0}
		for _, c := range []*interfaces.FindingComment{second, first} {
			if err := b.AddFindingComment(ctx, c); err != nil {
				t.Fatalf("AddFindingComment: %v", err)
			}
			if c.ID == "" {
				t.Fatalf("AddFindingComment did not assign an id")
			}
		}
		thread, err := b.ListFindingComments(ctx, flowID, "r:b")
		if err != nil || len(thread) != 2 || thread[0].ID != first.ID || thread[1].ID != second.ID || thread[0].AuthorName != "Alice" {
			t.Fatalf("thread = %+v, %v; want first then second", thread, err)
		}
		if other, err := b.ListFindingComments(ctx, flowID, "r:other"); err != nil || len(other) != 0 {
			t.Errorf("another finding's thread = %+v, %v; want empty", other, err)
		}
		if err := b.DeleteFindingComment(ctx, flowID, first.ID, "bob"); !errors.Is(err, interfaces.ErrNotCommentAuthor) {
			t.Errorf("deleting someone else's comment: %v, want ErrNotCommentAuthor", err)
		}
		if err := b.DeleteFindingComment(ctx, flowID, first.ID, "alice"); err != nil {
			t.Fatalf("deleting one's own comment: %v", err)
		}
		if err := b.DeleteFindingComment(ctx, flowID, first.ID, "alice"); err != nil {
			t.Errorf("deleting an absent comment: %v, want nil", err)
		}
		if thread, _ := b.ListFindingComments(ctx, flowID, "r:b"); len(thread) != 1 || thread[0].ID != second.ID {
			t.Errorf("thread after the delete = %+v, want second alone", thread)
		}
	})

	run("Policies_upsert_get_list_delete_per_org", func(t *testing.T) {
		orgA, orgB := "contract-pola-"+runID, "contract-polb-"+runID
		seedOrg(ctx, t, b, orgA, "contract-pol-admin-"+runID)
		seedOrg(ctx, t, b, orgB, "contract-pol-admin-"+runID)
		zeta := &models.Policy{ID: "contract-pol-z-" + runID, OrgID: orgA, Name: "Zeta", GateSeverity: models.SeverityError,
			Rules: []models.PolicyRule{{RuleID: "hardcoded-credential", Enabled: true}}}
		alpha := &models.Policy{ID: "contract-pol-a-" + runID, OrgID: orgA, Name: "Alpha"}
		for _, p := range []*models.Policy{zeta, alpha} {
			if err := b.SavePolicy(ctx, p); err != nil {
				t.Fatalf("SavePolicy %s: %v", p.Name, err)
			}
			if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
				t.Errorf("SavePolicy did not stamp %s: %+v", p.Name, p)
			}
		}
		t.Cleanup(func() {
			_ = b.DeletePolicy(context.Background(), orgA, zeta.ID)
			_ = b.DeletePolicy(context.Background(), orgA, alpha.ID)
		})
		got, err := b.GetPolicy(ctx, orgA, zeta.ID)
		if err != nil || got.Name != "Zeta" || got.GateSeverity != models.SeverityError || len(got.Rules) != 1 || got.Rules[0].RuleID != "hardcoded-credential" {
			t.Fatalf("GetPolicy = %+v, %v", got, err)
		}
		if _, err := b.GetPolicy(ctx, orgB, zeta.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("GetPolicy through another org: %v, want ErrNotFound", err)
		}
		list, err := b.ListPolicies(ctx, orgA)
		if err != nil || len(list) != 2 || list[0].Name != "Alpha" || list[1].Name != "Zeta" {
			t.Fatalf("ListPolicies = %+v, %v; want Alpha then Zeta", list, err)
		}
		if other, err := b.ListPolicies(ctx, orgB); err != nil || len(other) != 0 {
			t.Errorf("another org's policies = %+v, %v; want none", other, err)
		}

		created := got.CreatedAt
		zeta.Name = "Eta"
		if err := b.SavePolicy(ctx, zeta); err != nil {
			t.Fatalf("SavePolicy (update): %v", err)
		}
		if again, _ := b.GetPolicy(ctx, orgA, zeta.ID); again == nil || again.Name != "Eta" || !again.CreatedAt.Equal(created) {
			t.Errorf("after the update: %+v; want Eta, created at %v", again, created)
		}
		if err := b.DeletePolicy(ctx, orgB, zeta.ID); err != nil {
			t.Fatalf("DeletePolicy through another org: %v", err)
		}
		if _, err := b.GetPolicy(ctx, orgA, zeta.ID); err != nil {
			t.Errorf("another org's delete removed the policy: %v", err)
		}
		if err := b.DeletePolicy(ctx, orgA, zeta.ID); err != nil {
			t.Fatalf("DeletePolicy: %v", err)
		}
		if _, err := b.GetPolicy(ctx, orgA, zeta.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("GetPolicy after delete: %v, want ErrNotFound", err)
		}
	})

	run("RefreshTokens_rotate_revoke_and_list", func(t *testing.T) {
		rs, ok := b.(refreshTokenStore)
		if !ok {
			t.Skip("backend keeps no refresh tokens")
		}
		user := "contract-rt-user-" + runID
		seedUser(ctx, t, b, user)
		jti := func(n string) string { return "contract-rt-" + n + "-" + runID }
		in := time.Now().Add(time.Hour)
		for _, tok := range []struct {
			jti, user string
			exp       time.Time
			ua        string
		}{
			{jti("1"), user, in, "agent-1"},
			{jti("1"), "contract-rt-other-" + runID, in, "agent-x"}, // a known jti is left as it is
			{jti("2"), user, in, "agent-2"},
			{jti("old"), user, time.Now().Add(-time.Hour), "agent-old"},
		} {
			if err := rs.StoreRefreshToken(ctx, tok.jti, tok.user, tok.exp, tok.ua, "192.0.2.1"); err != nil {
				t.Fatalf("StoreRefreshToken %s: %v", tok.jti, err)
			}
		}
		t.Cleanup(func() { _ = rs.RevokeUserRefreshTokens(context.Background(), user) })
		for _, tc := range []struct {
			jti  string
			want bool
		}{{jti("1"), true}, {jti("old"), false}, {jti("none"), false}} {
			if ok, err := rs.IsRefreshTokenValid(ctx, tc.jti); err != nil || ok != tc.want {
				t.Errorf("IsRefreshTokenValid(%s) = %v, %v; want %v", tc.jti, ok, err, tc.want)
			}
		}
		sessions, err := rs.ListUserRefreshTokens(ctx, user)
		if err != nil || len(sessions) != 2 {
			t.Fatalf("ListUserRefreshTokens = %+v, %v; want the two active sessions", sessions, err)
		}
		for _, s := range sessions {
			if s.ID == jti("1") && s.UserAgent != "agent-1" {
				t.Errorf("session 1 = %+v; want its own user agent", s)
			}
		}

		info, err := rs.VerifyAndRevokeRefreshToken(ctx, jti("1"))
		if err != nil || info.UserID != user || info.ID != jti("1") {
			t.Fatalf("VerifyAndRevokeRefreshToken = %+v, %v", info, err)
		}
		if _, err := rs.VerifyAndRevokeRefreshToken(ctx, jti("1")); !errors.Is(err, interfaces.ErrTokenAlreadyRevoked) {
			t.Errorf("second rotation: %v, want ErrTokenAlreadyRevoked", err)
		}
		if _, err := rs.VerifyAndRevokeRefreshToken(ctx, jti("old")); !errors.Is(err, interfaces.ErrTokenAlreadyRevoked) {
			t.Errorf("rotating an expired token: %v, want ErrTokenAlreadyRevoked", err)
		}
		if err := rs.RevokeRefreshToken(ctx, jti("1")); !errors.Is(err, interfaces.ErrTokenAlreadyRevoked) {
			t.Errorf("revoking a revoked token: %v, want ErrTokenAlreadyRevoked", err)
		}
		if err := rs.RevokeRefreshTokenForUser(ctx, jti("2"), "contract-rt-other-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("revoking another user's session: %v, want ErrNotFound", err)
		}
		if err := rs.RevokeRefreshTokenForUser(ctx, jti("2"), user); err != nil {
			t.Errorf("revoking one's own session: %v", err)
		}
		if err := rs.RevokeRefreshTokenForUser(ctx, jti("2"), user); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("revoking it again: %v, want ErrNotFound", err)
		}
		if err := rs.StoreRefreshToken(ctx, jti("3"), user, in, "agent-3", ""); err != nil {
			t.Fatalf("StoreRefreshToken 3: %v", err)
		}
		if err := rs.RevokeUserRefreshTokens(ctx, user); err != nil {
			t.Fatalf("RevokeUserRefreshTokens: %v", err)
		}
		if ok, _ := rs.IsRefreshTokenValid(ctx, jti("3")); ok {
			t.Errorf("a session survived the user's logout everywhere")
		}
		if sessions, _ := rs.ListUserRefreshTokens(ctx, user); len(sessions) != 0 {
			t.Errorf("sessions after logout everywhere = %+v, want none", sessions)
		}
	})

	run("GovernanceAlerts_visible_per_user_through_flow_access", func(t *testing.T) {
		owner, member, outsider := "contract-ga-owner-"+runID, "contract-ga-member-"+runID, "contract-ga-out-"+runID
		orgID := "contract-ga-org-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "Alert Org", OwnerID: owner, Members: []interfaces.OrgMember{
			{UserID: owner, Role: auth.RoleAdmin}, {UserID: member, Role: auth.RoleMember},
		}}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), orgID) })
		flowID := "contract-ga-flow-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: flowID, Name: "Watched", OwnerID: owner, OrganizationID: orgID})
		now := time.Now().UTC()
		team := &interfaces.GovernanceAlert{ID: flowID + "|team", FlowID: flowID, Type: "drift", Title: "drift", Severity: "error", CreatedAt: now}
		personal := &interfaces.GovernanceAlert{ID: flowID + "|mine", FlowID: flowID, Type: "finding_assigned", Title: "assigned",
			Severity: "warning", TargetUser: member, CreatedAt: now.Add(time.Second)}
		for _, a := range []*interfaces.GovernanceAlert{team, personal} {
			if err := b.RecordGovernanceAlert(ctx, a); err != nil {
				t.Fatalf("RecordGovernanceAlert: %v", err)
			}
		}
		visible := func(userID string) []string {
			alerts, err := b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{UserID: userID})
			if err != nil {
				t.Fatalf("ListGovernanceAlerts(%s): %v", userID, err)
			}
			var ids []string
			for _, a := range alerts {
				ids = append(ids, a.ID)
			}
			return ids
		}
		if got := visible(member); !slices.Equal(got, []string{personal.ID, team.ID}) {
			t.Errorf("the org member sees %v, want their alert and the team's", got)
		}
		if got := visible(owner); !slices.Equal(got, []string{team.ID}) {
			t.Errorf("the owner sees %v, want the team's alert only", got)
		}
		if got := visible(outsider); len(got) != 0 {
			t.Errorf("an outsider sees %v, want nothing", got)
		}
		if n, err := b.UnreadGovernanceAlertCountFor(ctx, outsider); err != nil || n != 0 {
			t.Errorf("an outsider's unread count = %d, %v; want 0", n, err)
		}

		// An outsider's acknowledgements touch nothing they cannot see.
		if err := b.MarkAllGovernanceAlertsRead(ctx, outsider); err != nil {
			t.Fatalf("MarkAllGovernanceAlertsRead: %v", err)
		}
		if err := b.DismissGovernanceAlert(ctx, outsider, team.ID); err != nil {
			t.Fatalf("DismissGovernanceAlert: %v", err)
		}
		if n, err := b.UnreadGovernanceAlertCountFor(ctx, member); err != nil || n != 2 {
			t.Errorf("the member's unread count after an outsider's acknowledgements = %d, %v; want 2", n, err)
		}
		if err := b.DismissGovernanceAlert(ctx, member, team.ID); err != nil {
			t.Fatalf("DismissGovernanceAlert: %v", err)
		}
		if err := b.ClearGovernanceAlerts(ctx, outsider); err != nil {
			t.Fatalf("ClearGovernanceAlerts: %v", err)
		}
		all, err := b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{UserID: member, IncludeDismissed: true})
		if err != nil || len(all) != 2 {
			t.Errorf("an outsider's clear deleted the member's dismissed alert: %+v, %v", all, err)
		}
	})

	run("ShareTokens_list_live_links_newest_first", func(t *testing.T) {
		flowID := "contract-share-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: flowID, Name: "Shared report", OwnerID: "contract-share-owner-" + runID})
		t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
		past := time.Now().Add(-time.Minute)
		older := &interfaces.ShareToken{FlowID: flowID, TokenHash: "contract-share-h1-" + runID, CreatedAt: t0}
		newer := &interfaces.ShareToken{FlowID: flowID, TokenHash: "contract-share-h2-" + runID, CreatedAt: t0.Add(time.Second)}
		expired := &interfaces.ShareToken{FlowID: flowID, TokenHash: "contract-share-h3-" + runID, CreatedAt: t0, ExpiresAt: &past}
		for _, tok := range []*interfaces.ShareToken{older, newer, expired} {
			if err := b.CreateShareToken(ctx, tok); err != nil {
				t.Fatalf("CreateShareToken: %v", err)
			}
			if tok.ID == "" {
				t.Fatalf("CreateShareToken did not assign an id")
			}
		}
		list, err := b.ListShareTokens(ctx, flowID)
		if err != nil || len(list) != 2 || list[0].ID != newer.ID || list[1].ID != older.ID {
			t.Fatalf("ListShareTokens = %+v, %v; want the two live links, newest first", list, err)
		}
		if _, err := b.GetShareTokenByHash(ctx, expired.TokenHash); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("an expired link resolved: %v, want ErrNotFound", err)
		}
		if err := b.RevokeShareToken(ctx, "contract-other-flow-"+runID, older.ID); err != nil {
			t.Fatalf("RevokeShareToken (other flow): %v", err)
		}
		if _, err := b.GetShareTokenByHash(ctx, older.TokenHash); err != nil {
			t.Errorf("a revoke through another flow removed the link: %v", err)
		}
		if err := b.RevokeShareToken(ctx, flowID, older.ID); err != nil {
			t.Fatalf("RevokeShareToken: %v", err)
		}
		if _, err := b.GetShareTokenByHash(ctx, older.TokenHash); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("a revoked link resolved: %v, want ErrNotFound", err)
		}
	})

	run("FindingStatus_stamps_the_time_and_keeps_an_omitted_justification", func(t *testing.T) {
		flowID := "contract-just-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: flowID, Name: "Triaged", OwnerID: "contract-just-owner-" + runID})
		start := time.Now().Add(-time.Minute)
		if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: flowID, FindingKey: "k1", Status: "suppressed",
			Justification: "accepted risk", UpdatedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatalf("SetFindingStatus: %v", err)
		}
		if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: flowID, FindingKey: "k1", Status: "acknowledged"}); err != nil {
			t.Fatalf("SetFindingStatus (status only): %v", err)
		}
		if err := b.BatchSetFindingStatus(ctx, flowID, "", []*interfaces.FindingStatus{
			{FindingKey: "k1", Status: "in_progress"},
			{FindingKey: "k2", Status: "resolved", Justification: "fixed"},
		}); err != nil {
			t.Fatalf("BatchSetFindingStatus: %v", err)
		}
		got, err := b.ListFindingStatuses(ctx, flowID)
		if err != nil || len(got) != 2 {
			t.Fatalf("ListFindingStatuses = %+v, %v", got, err)
		}
		if got[0].FindingKey != "k1" || got[0].Status != "in_progress" || got[0].Justification != "accepted risk" {
			t.Errorf("k1 = %+v; want in_progress, still justified by the accepted risk", got[0])
		}
		if got[0].UpdatedAt.Before(start) {
			t.Errorf("k1 updated at %v; the store stamps the write time", got[0].UpdatedAt)
		}
		if got[1].FindingKey != "k2" || got[1].Justification != "fixed" {
			t.Errorf("k2 = %+v", got[1])
		}
	})

	run("Conversation_saved_as_nil_loads_empty", func(t *testing.T) {
		flowID := "contract-nilconv-" + runID
		if err := b.SaveConversation(ctx, flowID, "scope", nil); err != nil {
			t.Fatalf("SaveConversation(nil): %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteConversation(context.Background(), flowID, "scope") })
		got, err := b.LoadConversation(ctx, flowID, "scope")
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("LoadConversation = %v, %v; want a non-nil empty slice", got, err)
		}
	})

	run("SearchFlowContents_matches_within_the_filter", func(t *testing.T) {
		owner := "contract-sfc-owner-" + runID
		needle := "needle" + strings.ReplaceAll(runID, "-", "")
		saveFlow(t, &interfaces.FlowDocument{ID: "contract-sfc-src-" + runID, Name: "Source hit", OwnerID: owner,
			Content: json.RawMessage(`{"source":"Display.ShowMessage Message: '` + strings.ToUpper(needle) + `'"}`)})
		saveFlow(t, &interfaces.FlowDocument{ID: "contract-sfc-doc-" + runID, Name: "Document hit", OwnerID: owner,
			Content: json.RawMessage(`{"name":"x ` + needle + `"}`)})
		saveFlow(t, &interfaces.FlowDocument{ID: "contract-sfc-miss-" + runID, Name: "Miss", OwnerID: owner, Content: json.RawMessage(`{"name":"x"}`)})
		saveFlow(t, &interfaces.FlowDocument{ID: "contract-sfc-other-" + runID, Name: "Other's hit", OwnerID: "contract-sfc-x-" + runID,
			Content: json.RawMessage(`{"name":"` + needle + `"}`)})
		hits, err := b.SearchFlowContents(ctx, interfaces.FlowFilter{UserID: owner}, needle, 0)
		if errors.Is(err, interfaces.ErrContentSearchUnsupported) {
			t.Skip("backend has no content search")
		}
		if err != nil {
			t.Fatalf("SearchFlowContents: %v", err)
		}
		got := flowIDs(hits)
		slices.Sort(got)
		want := []string{"contract-sfc-doc-" + runID, "contract-sfc-src-" + runID}
		if !slices.Equal(got, want) {
			t.Errorf("hits = %v, want %v", got, want)
		}
		for _, h := range hits {
			if len(h.Content) != 0 {
				t.Errorf("hit %s carries content", h.ID)
			}
		}
		if hits, err := b.SearchFlowContents(ctx, interfaces.FlowFilter{}, needle, 0); err != nil || len(hits) != 0 {
			t.Errorf("unscoped search = %v, %v; want nothing", flowIDs(hits), err)
		}
		if hits, err := b.SearchFlowContents(ctx, interfaces.FlowFilter{UserID: owner}, "  ", 0); err != nil || hits == nil || len(hits) != 0 {
			t.Errorf("blank needle = %v, %v; want a non-nil empty list", hits, err)
		}
	})

	run("SaveFlowAnalysis_is_the_flows_health", func(t *testing.T) {
		flowID := "contract-health-" + runID
		saveFlow(t, &interfaces.FlowDocument{ID: flowID, Name: "Analyzed", OwnerID: "contract-health-owner-" + runID})
		if h, err := b.LoadFlowHealth(ctx, flowID); err != nil || h != nil {
			t.Fatalf("LoadFlowHealth before any analysis = %+v, %v; want nil, nil", h, err)
		}
		at := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
		if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: flowID, HealthScore: 80, Errors: 1, Warnings: 2, Info: 3, AnalyzedAt: at}); err != nil {
			t.Fatalf("SaveFlowAnalysis: %v", err)
		}
		h, err := b.LoadFlowHealth(ctx, flowID)
		if err != nil || h == nil || h.HealthScore != 80 || h.Errors != 1 || h.Warnings != 2 || h.Info != 3 || !h.AnalyzedAt.Equal(at) {
			t.Fatalf("LoadFlowHealth = %+v, %v", h, err)
		}
		batch, err := b.LoadFlowHealthBatch(ctx, []string{flowID, "contract-no-flow-" + runID})
		if err != nil || len(batch) != 1 || batch[flowID] == nil || batch[flowID].HealthScore != 80 {
			t.Errorf("LoadFlowHealthBatch = %+v, %v; want the analyzed flow only", batch, err)
		}
	})
}
