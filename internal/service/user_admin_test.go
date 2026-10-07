package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// adminStore records role changes over a fixed admin list and, through its session store, refresh-token
// revocation. The RefreshTokenStore methods are spelled out: FakeBackend's own do not match the interface.
type adminStore struct {
	*testutil.FakeBackend
	refresh *memRefreshStore
	admins  []*storageif.User
	updated map[string]auth.Role
}

func (s *adminStore) StoreRefreshToken(ctx context.Context, jti, userID string, exp time.Time, ua, ip string) error {
	return s.refresh.StoreRefreshToken(ctx, jti, userID, exp, ua, ip)
}

func (s *adminStore) IsRefreshTokenValid(ctx context.Context, jti string) (bool, error) {
	return s.refresh.IsRefreshTokenValid(ctx, jti)
}

func (s *adminStore) RevokeRefreshToken(ctx context.Context, jti string) error {
	return s.refresh.RevokeRefreshToken(ctx, jti)
}

func (s *adminStore) VerifyAndRevokeRefreshToken(ctx context.Context, jti string) (*storageif.RefreshTokenInfo, error) {
	return s.refresh.VerifyAndRevokeRefreshToken(ctx, jti)
}

func (s *adminStore) RevokeUserRefreshTokens(ctx context.Context, userID string) error {
	return s.refresh.RevokeUserRefreshTokens(ctx, userID)
}

func (s *adminStore) ListUserRefreshTokens(ctx context.Context, userID string) ([]storageif.RefreshTokenInfo, error) {
	return s.refresh.ListUserRefreshTokens(ctx, userID)
}

func (s *adminStore) RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error {
	return s.refresh.RevokeRefreshTokenForUser(ctx, jti, userID)
}

func (s *adminStore) ListAdmins(context.Context) ([]*storageif.User, error) { return s.admins, nil }

func (s *adminStore) UpdateUserRole(_ context.Context, id string, role auth.Role) error {
	s.updated[id] = role
	return nil
}

func TestUserAdminService_ChangeRole(t *testing.T) {
	ctx := context.Background()
	store := &adminStore{FakeBackend: testutil.NewFakeBackend(), refresh: newMemRefreshStore(),
		admins: []*storageif.User{{ID: "a1"}}, updated: map[string]auth.Role{}}
	svc := NewUserAdminService(store)

	if err := svc.ChangeRole(ctx, "a1", auth.RoleMember); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote the last admin: %v, want ErrLastAdmin", err)
	}
	if len(store.updated) != 0 {
		t.Errorf("refused change wrote %v", store.updated)
	}
	if err := svc.ChangeRole(ctx, "a1", auth.RoleAdmin); err != nil {
		t.Errorf("keep the last admin an admin: %v", err)
	}
	if err := svc.ChangeRole(ctx, "u2", auth.RoleViewer); err != nil {
		t.Fatalf("change role: %v", err)
	}
	if store.updated["u2"] != auth.RoleViewer {
		t.Errorf("updated = %v", store.updated)
	}
	if got := store.refresh.revokedAll; len(got) != 2 || got[1] != "u2" {
		t.Errorf("revoked sessions of %v, want each changed user", got)
	}
}

// A store without refresh tokens still changes the role.
func TestUserAdminService_ChangeRole_NoSessionStore(t *testing.T) {
	if err := NewUserAdminService(testutil.NewFakeBackend()).ChangeRole(context.Background(), "u1", auth.RoleMember); err != nil {
		t.Errorf("ChangeRole: %v", err)
	}
}

func TestUserAdminService_Reads(t *testing.T) {
	ctx := context.Background()
	backend := &testutil.FakeBackend{Users: map[string]*storageif.User{"u1": {ID: "u1", Role: auth.RoleAdmin}}}
	svc := NewUserAdminService(backend)
	users, total, err := svc.QueryUsers(ctx, storageif.UserQuery{Limit: 10})
	if err != nil || total != 1 || len(users) != 1 {
		t.Errorf("QueryUsers = %v, %d, %v", users, total, err)
	}
	empty, _, err := NewUserAdminService(testutil.NewFakeBackend()).QueryUsers(ctx, storageif.UserQuery{Limit: 10})
	if err != nil || empty == nil {
		t.Errorf("empty page = %v, %v; want non-nil", empty, err)
	}
	if stats, err := svc.Stats(ctx); err != nil || stats.Total != 1 {
		t.Errorf("Stats = %+v, %v", stats, err)
	}
	if events, err := svc.AuditEvents(ctx, storageif.AuditFilter{}); err != nil || events == nil {
		t.Errorf("AuditEvents = %v, %v", events, err)
	}
}
