package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// runLookupScenarios holds every backend to the methods that were optional capabilities of the Postgres backend
// before Phase 3 folded them into the role interfaces: the point lookups, the streamed export, refresh tokens and
// identity links. The page with its total is runKeysetPageScenarios' (ListFlowsPage).
func runLookupScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(name string, fn func(t *testing.T))) {
	run("GetFindingStatus_reads_one_row_or_nil", func(t *testing.T) {
		flowID := "contract-flow-getstatus-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: flowID, Name: "Status Flow", Content: json.RawMessage(`{}`), OwnerID: "contract-getstatus-owner-" + runID,
		}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
		if st, err := b.GetFindingStatus(ctx, flowID, "r1:b1"); err != nil || st != nil {
			t.Fatalf("no state yet: (%+v, %v), want (nil, nil)", st, err)
		}
		if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{
			FlowID: flowID, FindingKey: "r1:b1", RuleID: "r1", Status: "in_progress", AssigneeID: "contract-assignee-" + runID,
		}); err != nil {
			t.Fatalf("SetFindingStatus: %v", err)
		}
		st, err := b.GetFindingStatus(ctx, flowID, "r1:b1")
		if err != nil || st == nil {
			t.Fatalf("GetFindingStatus = (%+v, %v), want the row", st, err)
		}
		if st.FlowID != flowID || st.RuleID != "r1" || st.Status != "in_progress" || st.AssigneeID != "contract-assignee-"+runID {
			t.Errorf("row = %+v", st)
		}
		if other, err := b.GetFindingStatus(ctx, flowID, "r1:b2"); err != nil || other != nil {
			t.Errorf("another finding of the flow: (%+v, %v), want (nil, nil)", other, err)
		}
		if err := b.DeleteFindingStatus(ctx, flowID, "r1:b1"); err != nil {
			t.Fatalf("DeleteFindingStatus: %v", err)
		}
		if st, err := b.GetFindingStatus(ctx, flowID, "r1:b1"); err != nil || st != nil {
			t.Errorf("after delete: (%+v, %v), want (nil, nil)", st, err)
		}
	})

	run("GetAPITokenWithOwnerByHash_joins_the_current_owner", func(t *testing.T) {
		owner := "contract-patjoin-owner-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{
			ID: owner, Email: "contract-patjoin-" + runID + "@example.com", Password: "hash", Role: auth.RoleMember,
		}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		hash := "contract-patjoin-hash-" + runID
		if err := b.CreateAPIToken(ctx, &interfaces.APIToken{ID: "contract-patjoin-tok-" + runID, UserID: owner, Name: "ci", TokenHash: hash}); err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}
		tok, user, err := b.GetAPITokenWithOwnerByHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetAPITokenWithOwnerByHash: %v", err)
		}
		if tok.ID != "contract-patjoin-tok-"+runID || tok.UserID != owner {
			t.Errorf("token = %+v", tok)
		}
		if user.ID != owner || user.Email != "contract-patjoin-"+runID+"@example.com" || user.Role != auth.RoleMember || user.Password != "" {
			t.Errorf("owner = %+v (the join carries no password hash)", user)
		}
		// The owner's current row: a role change shows on the next lookup.
		if err := b.UpdateUserRole(ctx, owner, auth.RoleViewer); err != nil {
			t.Fatalf("UpdateUserRole: %v", err)
		}
		if _, user, err := b.GetAPITokenWithOwnerByHash(ctx, hash); err != nil || user.Role != auth.RoleViewer {
			t.Errorf("after the role change: (%+v, %v), want the viewer role", user, err)
		}
		if _, _, err := b.GetAPITokenWithOwnerByHash(ctx, "no-such-hash-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("unknown hash: %v, want ErrNotFound", err)
		}
	})

	run("OrgMemberRole_member_nonmember_and_missing_org", func(t *testing.T) {
		orgID := "contract-memberrole-org-" + runID
		if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "Member Role", OwnerID: "contract-mr-admin-" + runID, Members: []interfaces.OrgMember{
			{UserID: "contract-mr-admin-" + runID, Role: auth.RoleAdmin},
			{UserID: "contract-mr-viewer-" + runID, Role: auth.RoleViewer},
		}}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		if role, member, err := b.OrgMemberRole(ctx, orgID, "contract-mr-viewer-"+runID); err != nil || !member || role != auth.RoleViewer {
			t.Errorf("viewer: (%q, %v, %v), want (viewer, true, nil)", role, member, err)
		}
		if role, member, err := b.OrgMemberRole(ctx, orgID, "contract-mr-stranger-"+runID); err != nil || member || role != "" {
			t.Errorf("non-member: (%q, %v, %v), want (\"\", false, nil)", role, member, err)
		}
		if _, member, err := b.OrgMemberRole(ctx, "contract-no-such-org-"+runID, "contract-mr-admin-"+runID); !errors.Is(err, interfaces.ErrNotFound) || member {
			t.Errorf("missing org: (member %v, %v), want ErrNotFound", member, err)
		}
	})

	run("ListOrgMembershipsForUser_roles_without_rosters", func(t *testing.T) {
		user := "contract-ms-user-" + runID
		// Saved in this order, the second organisation was updated last: it lists first.
		first := &interfaces.Organisation{ID: "contract-ms-org-b-" + runID, Name: "Org B", OwnerID: user, Members: []interfaces.OrgMember{
			{UserID: user, Role: auth.RoleAdmin}, {UserID: "contract-ms-other-" + runID, Role: auth.RoleMember},
		}}
		second := &interfaces.Organisation{ID: "contract-ms-org-a-" + runID, Name: "Org A", OwnerID: "contract-ms-other-" + runID, Members: []interfaces.OrgMember{
			{UserID: "contract-ms-other-" + runID, Role: auth.RoleAdmin}, {UserID: user, Role: auth.RoleViewer},
		}}
		elsewhere := &interfaces.Organisation{ID: "contract-ms-org-c-" + runID, Name: "Org C", OwnerID: "contract-ms-other-" + runID, Members: []interfaces.OrgMember{
			{UserID: "contract-ms-other-" + runID, Role: auth.RoleAdmin},
		}}
		for _, o := range []*interfaces.Organisation{first, second, elsewhere} {
			if err := b.SaveOrg(ctx, o); err != nil {
				t.Fatalf("SaveOrg %s: %v", o.ID, err)
			}
		}
		got, err := b.ListOrgMembershipsForUser(ctx, user)
		if err != nil {
			t.Fatalf("ListOrgMembershipsForUser: %v", err)
		}
		want := []interfaces.OrgMembership{
			{OrgID: second.ID, OrgName: "Org A", Role: auth.RoleViewer},
			{OrgID: first.ID, OrgName: "Org B", Role: auth.RoleAdmin},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("memberships = %+v, want %+v", got, want)
		}
		none, err := b.ListOrgMembershipsForUser(ctx, "contract-ms-nobody-"+runID)
		if err != nil || none == nil || len(none) != 0 {
			t.Errorf("a user in no organisation: (%v, %v), want a non-nil empty list", none, err)
		}
	})

	run("ExportUserDataTo_streams_the_bundle_in_field_order", func(t *testing.T) {
		user := "contract-export-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: user, Email: "contract-export-" + runID + "@example.com", Password: "hash"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		var buf bytes.Buffer
		if err := b.ExportUserDataTo(ctx, user, &buf); err != nil {
			t.Fatalf("ExportUserDataTo: %v", err)
		}
		var export interfaces.UserDataExport
		if err := json.Unmarshal(buf.Bytes(), &export); err != nil {
			t.Fatalf("the stream is not the export bundle: %v", err)
		}
		if export.User == nil || export.User.ID != user || export.ExportedAt.IsZero() {
			t.Errorf("export = user %+v, exportedAt %v", export.User, export.ExportedAt)
		}
		if got, want := topLevelKeys(t, buf.Bytes()), jsonFieldNames(reflect.TypeOf(export)); !reflect.DeepEqual(got, want) {
			t.Errorf("keys = %v, want UserDataExport's order %v", got, want)
		}
		if err := b.ExportUserDataTo(ctx, "contract-export-nobody-"+runID, &bytes.Buffer{}); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("unknown user: %v, want ErrNotFound", err)
		}
	})

	run("RefreshTokens_store_rotate_list_revoke", func(t *testing.T) {
		user := "contract-rt-user-" + runID
		seedUser(ctx, t, b, user)
		jti := func(n string) string { return "contract-rt-" + n + "-" + runID }
		later := time.Now().Add(time.Hour)
		for _, tok := range []struct {
			jti     string
			expires time.Time
			ua      string
		}{
			{jti("a"), later, "agent-a"},
			{jti("b"), later, "agent-b"},
			{jti("expired"), time.Now().Add(-time.Hour), "agent-x"},
		} {
			if err := b.StoreRefreshToken(ctx, tok.jti, user, tok.expires, tok.ua, "203.0.113.9"); err != nil {
				t.Fatalf("StoreRefreshToken %s: %v", tok.jti, err)
			}
		}
		// A jti stored again keeps its first row.
		if err := b.StoreRefreshToken(ctx, jti("a"), "contract-rt-other-"+runID, later, "agent-z", ""); err != nil {
			t.Fatalf("StoreRefreshToken again: %v", err)
		}
		if ok, err := b.IsRefreshTokenValid(ctx, jti("a")); err != nil || !ok {
			t.Errorf("valid token: (%v, %v)", ok, err)
		}
		if ok, err := b.IsRefreshTokenValid(ctx, jti("expired")); err != nil || ok {
			t.Errorf("expired token: (%v, %v), want invalid", ok, err)
		}
		sessions, err := b.ListUserRefreshTokens(ctx, user)
		if err != nil {
			t.Fatalf("ListUserRefreshTokens: %v", err)
		}
		agents := map[string]string{}
		for _, s := range sessions {
			agents[s.ID] = s.UserAgent
			if s.UserID != "" || s.IP != "203.0.113.9" || s.CreatedAt.IsZero() {
				t.Errorf("session %+v: want no user id, the IP and a creation time", s)
			}
		}
		if len(sessions) != 2 || agents[jti("a")] != "agent-a" || agents[jti("b")] != "agent-b" {
			t.Errorf("sessions = %+v, want a and b (not the expired one), each with its first user agent", sessions)
		}

		info, err := b.VerifyAndRevokeRefreshToken(ctx, jti("a"))
		if err != nil || info.ID != jti("a") || info.UserID != user || info.ExpiresAt.IsZero() {
			t.Fatalf("VerifyAndRevokeRefreshToken = (%+v, %v)", info, err)
		}
		if _, err := b.VerifyAndRevokeRefreshToken(ctx, jti("a")); !errors.Is(err, interfaces.ErrTokenAlreadyRevoked) {
			t.Errorf("replayed token: %v, want ErrTokenAlreadyRevoked", err)
		}
		if _, err := b.VerifyAndRevokeRefreshToken(ctx, jti("expired")); !errors.Is(err, interfaces.ErrTokenAlreadyRevoked) {
			t.Errorf("expired token: %v, want ErrTokenAlreadyRevoked", err)
		}
		if err := b.RevokeRefreshToken(ctx, jti("a")); !errors.Is(err, interfaces.ErrTokenAlreadyRevoked) {
			t.Errorf("revoking a revoked token: %v, want ErrTokenAlreadyRevoked", err)
		}
		if err := b.RevokeRefreshToken(ctx, jti("unknown")); !errors.Is(err, interfaces.ErrTokenAlreadyRevoked) {
			t.Errorf("revoking an unknown token: %v, want ErrTokenAlreadyRevoked", err)
		}

		if err := b.RevokeRefreshTokenForUser(ctx, jti("b"), "contract-rt-other-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("another user's session: %v, want ErrNotFound", err)
		}
		if err := b.RevokeRefreshTokenForUser(ctx, jti("b"), user); err != nil {
			t.Errorf("own session: %v", err)
		}
		if err := b.RevokeRefreshTokenForUser(ctx, jti("b"), user); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("own session again: %v, want ErrNotFound", err)
		}
		if sessions, err := b.ListUserRefreshTokens(ctx, user); err != nil || len(sessions) != 0 {
			t.Errorf("after revoking both: (%+v, %v), want none", sessions, err)
		}

		if err := b.StoreRefreshToken(ctx, jti("c"), user, later, "", ""); err != nil {
			t.Fatalf("StoreRefreshToken c: %v", err)
		}
		if err := b.RevokeUserRefreshTokens(ctx, user); err != nil {
			t.Fatalf("RevokeUserRefreshTokens: %v", err)
		}
		if ok, err := b.IsRefreshTokenValid(ctx, jti("c")); err != nil || ok {
			t.Errorf("after revoking every session: (%v, %v), want invalid", ok, err)
		}
	})

	run("IdentityLinks_link_lookup_list_unlink", func(t *testing.T) {
		user := "contract-idl-user-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: user, Email: "contract-idl-" + runID + "@example.com", Password: "hash"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		subject := "contract-subject-" + runID
		if err := b.SaveIdentityLink(ctx, &interfaces.IdentityLink{Provider: "oidc", Subject: subject, UserID: user, Email: "idl@example.com"}); err != nil {
			t.Fatalf("SaveIdentityLink: %v", err)
		}
		if err := b.SaveIdentityLink(ctx, &interfaces.IdentityLink{Provider: "oidc", Subject: subject, UserID: "contract-idl-other-" + runID}); err == nil {
			t.Error("an identity already linked must not be linked again")
		}
		link, err := b.LoadIdentityLink(ctx, "oidc", subject)
		if err != nil || link.UserID != user || link.Email != "idl@example.com" || link.CreatedAt.IsZero() {
			t.Fatalf("LoadIdentityLink = (%+v, %v)", link, err)
		}
		if _, err := b.LoadIdentityLink(ctx, "oidc", "no-such-subject-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("unknown identity: %v, want ErrNotFound", err)
		}
		links, err := b.ListIdentityLinks(ctx, user)
		if err != nil || len(links) != 1 || links[0].Subject != subject {
			t.Errorf("ListIdentityLinks = (%+v, %v), want the one link", links, err)
		}
		if none, err := b.ListIdentityLinks(ctx, "contract-idl-nobody-"+runID); err != nil || none == nil || len(none) != 0 {
			t.Errorf("a user without links: (%v, %v), want a non-nil empty list", none, err)
		}
		if err := b.DeleteIdentityLink(ctx, user, "oidc"); err != nil {
			t.Fatalf("DeleteIdentityLink: %v", err)
		}
		if _, err := b.LoadIdentityLink(ctx, "oidc", subject); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("after unlinking: %v, want ErrNotFound", err)
		}
		if err := b.DeleteIdentityLink(ctx, user, "oidc"); err != nil {
			t.Errorf("unlinking again: %v, want a no-op", err)
		}
	})

	run("SaveUserWithIdentityLink_writes_both_or_neither", func(t *testing.T) {
		email := func(name string) string { return "contract-signup-" + name + "-" + runID + "@example.com" }
		subject := func(name string) string { return "contract-signup-subject-" + name + "-" + runID }
		user := &interfaces.User{ID: "contract-signup-user-" + runID, Email: email("user"), EmailVerified: true, Role: auth.RoleMember, DisplayName: "Signed Up"}
		if err := b.SaveUserWithIdentityLink(ctx, user, &interfaces.IdentityLink{Provider: "oidc", Subject: subject("a"), UserID: user.ID, Email: user.Email}); err != nil {
			t.Fatalf("SaveUserWithIdentityLink: %v", err)
		}
		if got, err := b.LoadUserByID(ctx, user.ID); err != nil || got.Email != user.Email || !got.EmailVerified || got.Password != "" || got.DisplayName != "Signed Up" {
			t.Errorf("the signed-up user = (%+v, %v)", got, err)
		}
		if link, err := b.LoadIdentityLink(ctx, "oidc", subject("a")); err != nil || link.UserID != user.ID {
			t.Errorf("the sign-up's link = (%+v, %v), want one to %s", link, err, user.ID)
		}

		// An identity already linked: the new account is not written either.
		other := &interfaces.User{ID: "contract-signup-other-" + runID, Email: email("other"), EmailVerified: true, Role: auth.RoleMember}
		if err := b.SaveUserWithIdentityLink(ctx, other, &interfaces.IdentityLink{Provider: "oidc", Subject: subject("a"), UserID: other.ID}); err == nil {
			t.Error("an identity already linked was linked again")
		}
		if got, err := b.LoadUserByID(ctx, other.ID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the refused sign-up's account = (%+v, %v), want ErrNotFound", got, err)
		}
		// A taken email: ErrEmailExists, and the identity is not linked.
		taken := &interfaces.User{ID: "contract-signup-taken-" + runID, Email: user.Email, EmailVerified: true, Role: auth.RoleMember}
		if err := b.SaveUserWithIdentityLink(ctx, taken, &interfaces.IdentityLink{Provider: "oidc", Subject: subject("b"), UserID: taken.ID}); !errors.Is(err, interfaces.ErrEmailExists) {
			t.Errorf("a taken email: %v, want ErrEmailExists", err)
		}
		if link, err := b.LoadIdentityLink(ctx, "oidc", subject("b")); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the refused sign-up's link = (%+v, %v), want ErrNotFound", link, err)
		}

		// An existing account is updated (an adopted unverified account) and linked in the same write.
		adopted := &interfaces.User{ID: "contract-signup-adopted-" + runID, Email: email("adopted"), Password: "hash", Role: auth.RoleMember}
		if err := b.CreateUser(ctx, adopted); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		before, err := b.LoadUserByID(ctx, adopted.ID)
		if err != nil {
			t.Fatalf("LoadUserByID: %v", err)
		}
		before.EmailVerified, before.Password = true, ""
		if err := b.SaveUserWithIdentityLink(ctx, before, &interfaces.IdentityLink{Provider: "oidc", Subject: subject("c"), UserID: adopted.ID}); err != nil {
			t.Fatalf("SaveUserWithIdentityLink (existing account): %v", err)
		}
		if got, err := b.LoadUserByID(ctx, adopted.ID); err != nil || !got.EmailVerified || got.Password != "" || !got.CreatedAt.Equal(before.CreatedAt) {
			t.Errorf("the adopted account = (%+v, %v); want verified, no password, created at %v", got, err, before.CreatedAt)
		}
		if links, err := b.ListIdentityLinks(ctx, adopted.ID); err != nil || len(links) != 1 || links[0].Subject != subject("c") {
			t.Errorf("the adopted account's links = (%+v, %v), want the one", links, err)
		}
	})

	// The adoption writes from a row read before it: only its verification and password change, and only while the
	// stored row is still unverified. It upserted the whole stale row, restoring the role, display name and lockout
	// as read, and stripped the password of an account its owner had verified in between.
	run("SaveUserWithIdentityLink_adopts_only_a_still_unverified_account", func(t *testing.T) {
		subject := func(name string) string { return "contract-adopt-subject-" + name + "-" + runID }
		account := func(name string) *interfaces.User {
			t.Helper()
			u := &interfaces.User{ID: "contract-adopt-" + name + "-" + runID, Email: "contract-adopt-" + name + "-" + runID + "@example.com", Password: "hash", Role: auth.RoleMember, DisplayName: "As read"}
			if err := b.CreateUser(ctx, u); err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			t.Cleanup(func() { _ = b.DeleteUser(context.Background(), u.ID) }) // it is made an admin below
			read, err := b.LoadUserByID(ctx, u.ID)
			if err != nil {
				t.Fatalf("LoadUserByID: %v", err)
			}
			return read
		}
		changeStored := func(id string, change func(*interfaces.User)) {
			t.Helper()
			u, err := b.LoadUserByID(ctx, id)
			if err != nil {
				t.Fatalf("LoadUserByID: %v", err)
			}
			change(u)
			if err := b.SaveUser(ctx, u); err != nil {
				t.Fatalf("SaveUser: %v", err)
			}
		}

		// Changed in between, still unverified: adopted, and the other changes stand.
		stale := account("changed")
		until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		changeStored(stale.ID, func(u *interfaces.User) {
			u.Role, u.DisplayName, u.FailedLoginAttempts, u.LockedUntil = auth.RoleAdmin, "Renamed", 3, &until
		})
		stale.EmailVerified, stale.Password = true, ""
		if err := b.SaveUserWithIdentityLink(ctx, stale, &interfaces.IdentityLink{Provider: "oidc", Subject: subject("changed"), UserID: stale.ID}); err != nil {
			t.Fatalf("adopting an account changed in between: %v", err)
		}
		got, err := b.LoadUserByID(ctx, stale.ID)
		if err != nil || !got.EmailVerified || got.Password != "" {
			t.Fatalf("the adopted account = (%+v, %v), want verified without a password", got, err)
		}
		if got.Role != auth.RoleAdmin || got.DisplayName != "Renamed" || got.FailedLoginAttempts != 3 || got.LockedUntil == nil || !got.LockedUntil.Equal(until) {
			t.Errorf("the adoption reverted what changed in between: role %q, name %q, failed %d, locked until %v",
				got.Role, got.DisplayName, got.FailedLoginAttempts, got.LockedUntil)
		}

		// Verified in between: refused, nothing written.
		stale = account("verified")
		changeStored(stale.ID, func(u *interfaces.User) { u.EmailVerified = true })
		stale.EmailVerified, stale.Password = true, ""
		if err := b.SaveUserWithIdentityLink(ctx, stale, &interfaces.IdentityLink{Provider: "oidc", Subject: subject("verified"), UserID: stale.ID}); !errors.Is(err, interfaces.ErrAccountVerified) {
			t.Errorf("adopting an account verified in between: %v, want ErrAccountVerified", err)
		}
		if got, err := b.LoadUserByID(ctx, stale.ID); err != nil || got.Password != "hash" {
			t.Errorf("the verified account after the refused adoption = (%+v, %v), want its password kept", got, err)
		}
		if link, err := b.LoadIdentityLink(ctx, "oidc", subject("verified")); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the refused adoption's link = (%+v, %v), want ErrNotFound", link, err)
		}
	})

	run("DeleteUser_removes_sessions_and_identity_links", func(t *testing.T) {
		user := "contract-erase-sessions-" + runID
		if err := b.CreateUser(ctx, &interfaces.User{ID: user, Email: "contract-erase-sessions-" + runID + "@example.com", Password: "hash"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if err := b.StoreRefreshToken(ctx, "contract-erase-jti-"+runID, user, time.Now().Add(time.Hour), "", ""); err != nil {
			t.Fatalf("StoreRefreshToken: %v", err)
		}
		if err := b.SaveIdentityLink(ctx, &interfaces.IdentityLink{Provider: "oidc", Subject: "contract-erase-subject-" + runID, UserID: user}); err != nil {
			t.Fatalf("SaveIdentityLink: %v", err)
		}
		if err := b.DeleteUser(ctx, user); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		if sessions, err := b.ListUserRefreshTokens(ctx, user); err != nil || len(sessions) != 0 {
			t.Errorf("sessions after erasure: (%+v, %v), want none", sessions, err)
		}
		if ok, err := b.IsRefreshTokenValid(ctx, "contract-erase-jti-"+runID); err != nil || ok {
			t.Errorf("token after erasure: (%v, %v), want invalid", ok, err)
		}
		if _, err := b.LoadIdentityLink(ctx, "oidc", "contract-erase-subject-"+runID); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("identity link after erasure: %v, want ErrNotFound", err)
		}
	})

	// The empty id names no account, but rows keyed by it: erasing it took every flow without an owner and every usage
	// row of the local user or of an erased member.
	run("DeleteUser_refuses_the_empty_id", func(t *testing.T) {
		flow := &interfaces.FlowDocument{ID: "contract-erase-unowned-" + runID, Name: "Unowned", Content: []byte(`{"subflows":[]}`)}
		if err := b.SaveFlow(ctx, flow); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), flow.ID) })
		if err := b.DeleteUser(ctx, ""); !errors.Is(err, interfaces.ErrUserIDRequired) {
			t.Errorf("DeleteUser of the empty id: %v, want ErrUserIDRequired", err)
		}
		if _, err := b.LoadFlow(ctx, flow.ID); err != nil {
			t.Errorf("the flow without an owner after DeleteUser of the empty id: %v", err)
		}
	})
}

// topLevelKeys returns the keys of the JSON object data, in order.
func topLevelKeys(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %v %v", tok, err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("read %v: %v", tok, err)
		}
	}
	return keys
}

// jsonFieldNames returns the JSON names of a struct type's fields, in declaration order.
func jsonFieldNames(typ reflect.Type) []string {
	var names []string
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		names = append(names, name)
	}
	return names
}
