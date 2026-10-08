package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// Service-side tests for the second performance round: the library page
// resolves the caller's org memberships ONCE (list + count used to resolve
// them twice), and the page's permission flags reuse the roles those
// memberships carry instead of querying the role per distinct org again.

// r2Roles is the test user's role per org.
var r2Roles = map[string]auth.Role{"org-admin": auth.RoleAdmin, "org-member": auth.RoleMember, "org-viewer": auth.RoleViewer}

// r2LibraryBackend is a FakeBackend whose roster-free membership list holds
// r2Roles, counting reads.
type r2LibraryBackend struct {
	*testutil.FakeBackend
	membershipReads int
}

func (b *r2LibraryBackend) ListOrgMembershipsForUser(context.Context, string) ([]interfaces.OrgMembership, error) {
	b.membershipReads++
	out := []interfaces.OrgMembership{}
	for org, role := range r2Roles {
		out = append(out, interfaces.OrgMembership{OrgID: org, OrgName: org, Role: role})
	}
	return out, nil
}

// r2OrgStore is the org store behind the OrgService, its point role lookup
// answering r2Roles and counting lookups — the query BatchFlowPermissions
// issues per distinct org.
type r2OrgStore struct {
	collaboration.OrgStore
	roleLookups int
}

func (s *r2OrgStore) OrgMemberRole(_ context.Context, orgID, _ string) (auth.Role, bool, error) {
	s.roleLookups++
	role, ok := r2Roles[orgID]
	return role, ok, nil
}

func newR2Library(t *testing.T) (*LibraryService, *r2LibraryBackend, *r2OrgStore) {
	t.Helper()
	b := &r2LibraryBackend{FakeBackend: testutil.NewFakeBackend()}
	store := &r2OrgStore{OrgStore: collaboration.NewMemOrgStore()}
	ctx := context.Background()
	for _, f := range []*interfaces.FlowDocument{
		{ID: "f-own", Name: "own", OwnerID: "u1"},
		{ID: "f-admin", Name: "admin org", OwnerID: "someone", OrganizationID: "org-admin"},
		{ID: "f-member", Name: "member org", OwnerID: "someone", OrganizationID: "org-member"},
		{ID: "f-viewer", Name: "viewer org", OwnerID: "someone", OrganizationID: "org-viewer"},
		{ID: "f-foreign", Name: "foreign org", OwnerID: "someone", OrganizationID: "org-foreign"},
		{ID: "f-ownerless", Name: "ownerless", OwnerID: ""},
	} {
		if err := b.SaveFlow(ctx, f); err != nil {
			t.Fatalf("seed %s: %v", f.ID, err)
		}
	}
	authz := NewAuthzService(b, collaboration.NewOrgService(store))
	return NewLibraryService(b, nil, authz, nil), b, store
}

// TestListLibraryFlowsPage_ResolvesMembershipsOnce: one page = one
// membership read, and the page carries the memberships (with roles) for
// the permission step; list and count share the one filter.
func TestListLibraryFlowsPage_ResolvesMembershipsOnce(t *testing.T) {
	lib, b, _ := newR2Library(t)
	ctx := context.Background()

	page, err := lib.ListLibraryFlowsPage(ctx, "u1", "", ScopeAll, "", "updated_desc", 25, 0, nil)
	if err != nil {
		t.Fatalf("ListLibraryFlowsPage: %v", err)
	}
	if b.membershipReads != 1 {
		t.Errorf("memberships resolved %d times for one page, want 1", b.membershipReads)
	}
	if page.Memberships == nil || len(page.Memberships) != 3 {
		t.Errorf("page.Memberships = %v, want the 3 memberships", page.Memberships)
	}
	// u1 sees its own flow and the three orgs' flows, not the foreign org's or the ownerless one.
	if page.Total != 4 || len(page.Docs) != 4 {
		t.Errorf("page: %d docs, total %d; want 4 and 4", len(page.Docs), page.Total)
	}
	filters := b.ListFlowsFilterLog()
	if n := len(filters); n != 1 {
		t.Fatalf("ListFlows called %d times, want 1", n)
	}
	f := filters[0]
	if len(f.OrganizationIDs) != 3 || f.Limit != 25 || !f.MetadataOnly {
		t.Errorf("list filter = %+v: org scope, limit or MetadataOnly lost", f)
	}

	// The separate list + count path (still used by other callers) resolves
	// the scope per call — the page method is what removes the second read.
	b.membershipReads = 0
	if _, err := lib.ListLibraryFlowsWithTagFilter(ctx, "u1", "", ScopeAll, "", "", 25, 0, nil); err != nil {
		t.Fatalf("ListLibraryFlowsWithTagFilter: %v", err)
	}
	if _, err := lib.CountLibraryFlowsWithTagFilter(ctx, "u1", "", ScopeAll, "", nil); err != nil {
		t.Fatalf("CountLibraryFlowsWithTagFilter: %v", err)
	}
	if b.membershipReads != 2 {
		t.Errorf("list + count resolved memberships %d times, want 2 (one each)", b.membershipReads)
	}

	// Scopes that do not widen by org carry no memberships.
	for _, scope := range []LibraryScope{ScopeMine, ScopeShared} {
		p, err := lib.ListLibraryFlowsPage(ctx, "u1", "", scope, "", "", 25, 0, nil)
		if err != nil {
			t.Fatalf("ListLibraryFlowsPage(%s): %v", scope, err)
		}
		if p.Memberships != nil {
			t.Errorf("scope %s carried memberships %v, want nil", scope, p.Memberships)
		}
	}
}

// TestBatchFlowPermissionsWithRoles_NoRoleQueries: seeded with the
// memberships, the flags come out as from the per-org lookups — admin org:
// everything; member org: edit; viewer and foreign orgs: nothing — with ZERO
// role queries, foreign orgs included (the list is complete, so absence
// means non-member). nil memberships keep the lookups.
func TestBatchFlowPermissionsWithRoles_NoRoleQueries(t *testing.T) {
	lib, b, store := newR2Library(t)
	ctx := context.Background()
	page, err := lib.ListLibraryFlowsPage(ctx, "u1", "", ScopeAll, "", "", 25, 0, nil)
	if err != nil {
		t.Fatalf("ListLibraryFlowsPage: %v", err)
	}
	// Every seeded flow, the foreign-org and ownerless ones too (u1's page holds neither).
	docs := make([]*interfaces.FlowDocument, 0, len(b.Flows))
	for _, f := range b.Flows {
		docs = append(docs, f)
	}

	store.roleLookups = 0
	seeded := lib.BatchFlowPermissionsWithRoles(ctx, docs, "u1", page.Memberships)
	if store.roleLookups != 0 {
		t.Errorf("seeded permissions ran %d org-role queries, want 0", store.roleLookups)
	}
	want := map[string]PermFlags{
		"f-own":       {CanEdit: true, CanDelete: true, CanShare: true},
		"f-admin":     {CanEdit: true, CanDelete: true, CanShare: true},
		"f-member":    {CanEdit: true},
		"f-viewer":    {},
		"f-foreign":   {},
		"f-ownerless": {},
	}
	for id, w := range want {
		if seeded[id] != w {
			t.Errorf("seeded %s = %+v, want %+v", id, seeded[id], w)
		}
	}

	store.roleLookups = 0
	looked := lib.BatchFlowPermissions(ctx, docs, "u1")
	if store.roleLookups != 4 {
		t.Errorf("unseeded permissions ran %d org-role queries, want 4 (one per distinct org)", store.roleLookups)
	}
	for id := range want {
		if looked[id] != seeded[id] {
			t.Errorf("%s: seeded %+v differs from looked-up %+v", id, seeded[id], looked[id])
		}
	}

	// An empty, non-nil list is still "complete": no lookups, no org grants.
	store.roleLookups = 0
	none := lib.BatchFlowPermissionsWithRoles(ctx, docs, "u1", []interfaces.OrgMembership{})
	if store.roleLookups != 0 {
		t.Errorf("empty memberships ran %d org-role queries, want 0", store.roleLookups)
	}
	if none["f-admin"] != (PermFlags{}) || none["f-own"] != (PermFlags{CanEdit: true, CanDelete: true, CanShare: true}) {
		t.Errorf("empty memberships: admin-org flow = %+v (want none), own flow = %+v (want all)", none["f-admin"], none["f-own"])
	}
}
