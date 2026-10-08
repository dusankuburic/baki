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
// before Phase 3 folded them into the role interfaces: the point lookups, the page with its total, the streamed
// export, refresh tokens and identity links.
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

	run("ListFlowsWithTotal_pages_with_the_unpaginated_total", func(t *testing.T) {
		owner := "contract-pagetotal-owner-" + runID
		var ids []string
		for _, suffix := range []string{"a", "b", "c"} {
			id := "contract-pagetotal-" + suffix + "-" + runID
			ids = append(ids, id)
			if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: id, Content: json.RawMessage(`{}`), OwnerID: owner}); err != nil {
				t.Fatalf("SaveFlow %s: %v", id, err)
			}
		}
		filter := interfaces.FlowFilter{UserID: owner, SortBy: interfaces.FlowSortIDAsc, Limit: 2, MetadataOnly: true}
		docs, total, err := b.ListFlowsWithTotal(ctx, filter)
		if err != nil {
			t.Fatalf("ListFlowsWithTotal: %v", err)
		}
		if len(docs) != 2 || docs[0].ID != ids[0] || docs[1].ID != ids[1] || total != 3 {
			t.Fatalf("page = %d docs, total %d; want %s, %s of 3", len(docs), total, ids[0], ids[1])
		}
		if count, err := b.CountFlows(ctx, filter); err != nil || count != total {
			t.Errorf("CountFlows = (%d, %v), want the page's total %d", count, err, total)
		}
		filter.Offset = 2
		if docs, total, err := b.ListFlowsWithTotal(ctx, filter); err != nil || len(docs) != 1 || docs[0].ID != ids[2] || total != 3 {
			t.Errorf("second page = %d docs, total %d, %v; want %s of 3", len(docs), total, err, ids[2])
		}
		// A page past the end has no rows to carry the total; it still reports the real one.
		filter.Offset = 10
		if docs, total, err := b.ListFlowsWithTotal(ctx, filter); err != nil || len(docs) != 0 || total != 3 {
			t.Errorf("past the end = %d docs, total %d, %v; want none of 3", len(docs), total, err)
		}
		filter.UserID = owner + "-nobody"
		for _, offset := range []int{0, 10} {
			filter.Offset = offset
			if docs, total, err := b.ListFlowsWithTotal(ctx, filter); err != nil || len(docs) != 0 || total != 0 {
				t.Errorf("no match at offset %d = %d docs, total %d, %v; want none of 0", offset, len(docs), total, err)
			}
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
