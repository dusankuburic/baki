package collaboration

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// pointLookupStore is an OrgStore that also offers the single-row membership
// lookup (as the Postgres backend does) and counts roster loads.
type pointLookupStore struct {
	OrgStore
	roles    map[string]auth.Role // userID → role in org "o1"
	lookups  int
	loadOrgs int
}

func (s *pointLookupStore) OrgMemberRole(_ context.Context, orgID, userID string) (auth.Role, bool, error) {
	s.lookups++
	if orgID != "o1" {
		return "", false, interfaces.ErrNotFound
	}
	role, ok := s.roles[userID]
	return role, ok, nil
}

func (s *pointLookupStore) LoadOrg(ctx context.Context, id string) (*interfaces.Organisation, error) {
	s.loadOrgs++
	return s.OrgStore.LoadOrg(ctx, id)
}

// TestMembershipChecks_UsePointLookup: MemberRole / IsMember / IsAdmin back
// every flow-authz org check and used to load the org plus its WHOLE roster
// to find one member. With a store that offers the point lookup they must not
// load the roster, and the error split (missing org vs non-member) holds.
func TestMembershipChecks_UsePointLookup(t *testing.T) {
	store := &pointLookupStore{
		OrgStore: NewMemOrgStore(),
		roles:    map[string]auth.Role{"admin-1": auth.RoleAdmin, "member-1": auth.RoleMember},
	}
	svc := NewOrgService(store)
	ctx := context.Background()

	if role, err := svc.MemberRole(ctx, "o1", "member-1"); err != nil || role != auth.RoleMember {
		t.Errorf("MemberRole(member) = %q, %v", role, err)
	}
	if _, err := svc.MemberRole(ctx, "o1", "stranger"); !errors.Is(err, ErrMemberNotFound) {
		t.Errorf("MemberRole(non-member) err = %v, want ErrMemberNotFound", err)
	}
	if _, err := svc.MemberRole(ctx, "nope", "member-1"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("MemberRole(missing org) err = %v, want the org-not-found error", err)
	}
	if !svc.IsMember(ctx, "o1", "member-1") || svc.IsMember(ctx, "o1", "stranger") || svc.IsMember(ctx, "nope", "member-1") {
		t.Error("IsMember answered wrongly")
	}
	if !svc.IsAdmin(ctx, "o1", "admin-1") || svc.IsAdmin(ctx, "o1", "member-1") || svc.IsAdmin(ctx, "nope", "admin-1") {
		t.Error("IsAdmin answered wrongly")
	}
	if store.loadOrgs != 0 {
		t.Errorf("membership checks loaded the org roster %d times, want 0", store.loadOrgs)
	}
	if store.lookups != 9 {
		t.Errorf("point lookups = %d, want 9", store.lookups)
	}
}
