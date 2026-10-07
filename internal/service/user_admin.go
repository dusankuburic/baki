package service

import (
	"context"
	"errors"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ErrLastAdmin refuses demoting the deployment's only administrator.
var ErrLastAdmin = errors.New("cannot demote the last administrator")

// userAdminStore is the slice of StorageBackend the deployment admin pages read and write.
type userAdminStore interface {
	storageif.UserStore
	storageif.AuditStore
}

// UserAdminService backs the deployment administration pages: the user list and statistics, role changes and
// the audit trail.
type UserAdminService struct {
	store userAdminStore
}

func NewUserAdminService(store userAdminStore) *UserAdminService {
	return &UserAdminService{store: store}
}

// QueryUsers returns one page of the user list plus the number of users matching q; the page is never nil.
func (s *UserAdminService) QueryUsers(ctx context.Context, q storageif.UserQuery) ([]*storageif.User, int, error) {
	users, total, err := s.store.QueryUsers(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	if users == nil {
		users = []*storageif.User{}
	}
	return users, total, nil
}

// Stats counts every user by role and state.
func (s *UserAdminService) Stats(ctx context.Context) (*storageif.UserStats, error) {
	return s.store.LoadUserStats(ctx)
}

// ChangeRole sets userID's role; demoting the only admin is ErrLastAdmin. The user's refresh tokens are then
// revoked (best effort) so they re-authenticate and pick up the new role: an existing access JWT keeps the old
// role until it expires, and no new one may be minted stale.
func (s *UserAdminService) ChangeRole(ctx context.Context, userID string, role auth.Role) error {
	if role != auth.RoleAdmin {
		admins, err := s.store.ListAdmins(ctx)
		if err != nil {
			return err
		}
		if len(admins) == 1 && admins[0].ID == userID {
			return ErrLastAdmin
		}
	}
	if err := s.store.UpdateUserRole(ctx, userID, role); err != nil {
		return err
	}
	if rs, ok := s.store.(RefreshTokenStore); ok {
		if err := rs.RevokeUserRefreshTokens(ctx, userID); err != nil {
			logger.FromContext(ctx).Warn("failed to revoke refresh tokens after role change", "userID", userID, "error", err)
		}
	}
	return nil
}

// AuditEvents returns the audit trail matching filter, newest first.
func (s *UserAdminService) AuditEvents(ctx context.Context, filter storageif.AuditFilter) ([]*storageif.AuditEvent, error) {
	return s.store.ListAuditEvents(ctx, filter)
}
