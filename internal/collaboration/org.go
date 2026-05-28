package collaboration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// Errors returned by the organisation service.
var (
	ErrOrgNotFound    = errors.New("collaboration: organisation not found")
	ErrMemberNotFound = errors.New("collaboration: member not found")
	ErrAlreadyMember  = errors.New("collaboration: user is already a member")
	ErrLastAdmin      = errors.New("collaboration: cannot remove the last admin")
)

// OrgService manages organisations and their memberships.
// Business logic lives here; persistence is delegated to an OrgStore.
type OrgService struct {
	store OrgStore
}

// NewOrgService creates an OrgService backed by the given store.
// Pass NewMemOrgStore() for local/test mode, or a postgres-backed store for cloud mode.
func NewOrgService(store OrgStore) *OrgService {
	return &OrgService{store: store}
}

// Create creates a new organisation owned by ownerID.
func (s *OrgService) Create(name, ownerID string) (*interfaces.Organisation, error) {
	if name == "" {
		return nil, errors.New("collaboration: organisation name is required")
	}
	if ownerID == "" {
		return nil, errors.New("collaboration: owner ID is required")
	}

	now := time.Now().UTC()
	org := &interfaces.Organisation{
		ID:        uuid.New().String(),
		Name:      name,
		OwnerID:   ownerID,
		Members:   []interfaces.OrgMember{{UserID: ownerID, Role: auth.RoleAdmin, JoinedAt: now}},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.SaveOrg(context.Background(), org); err != nil {
		return nil, err
	}
	return org, nil
}

// Get returns the organisation with the given ID.
func (s *OrgService) Get(orgID string) (*interfaces.Organisation, error) {
	return s.store.LoadOrg(context.Background(), orgID)
}

// ListForUser returns all organisations the user belongs to.
func (s *OrgService) ListForUser(userID string) []*interfaces.Organisation {
	orgs, _ := s.store.ListOrgsForUser(context.Background(), userID)
	return orgs
}

// AddMember adds userID to orgID with the given role.
func (s *OrgService) AddMember(orgID, userID string, role auth.Role) error {
	if !role.IsValid() {
		return fmt.Errorf("collaboration: invalid role %q", role)
	}

	ctx := context.Background()
	org, err := s.store.LoadOrg(ctx, orgID)
	if err != nil {
		return err
	}

	for _, m := range org.Members {
		if m.UserID == userID {
			return ErrAlreadyMember
		}
	}

	org.Members = append(org.Members, interfaces.OrgMember{
		UserID:   userID,
		Role:     role,
		JoinedAt: time.Now().UTC(),
	})
	org.UpdatedAt = time.Now().UTC()
	return s.store.SaveOrg(ctx, org)
}

// RemoveMember removes userID from orgID.
func (s *OrgService) RemoveMember(orgID, userID string) error {
	ctx := context.Background()
	org, err := s.store.LoadOrg(ctx, orgID)
	if err != nil {
		return err
	}

	idx := -1
	for i, m := range org.Members {
		if m.UserID == userID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ErrMemberNotFound
	}

	if org.Members[idx].Role == auth.RoleAdmin && adminCount(org) == 1 {
		return ErrLastAdmin
	}

	org.Members = append(org.Members[:idx], org.Members[idx+1:]...)
	org.UpdatedAt = time.Now().UTC()
	return s.store.SaveOrg(ctx, org)
}

// SetRole changes a member's role within an organisation.
func (s *OrgService) SetRole(orgID, userID string, role auth.Role) error {
	if !role.IsValid() {
		return fmt.Errorf("collaboration: invalid role %q", role)
	}

	ctx := context.Background()
	org, err := s.store.LoadOrg(ctx, orgID)
	if err != nil {
		return err
	}

	for i, m := range org.Members {
		if m.UserID == userID {
			if m.Role == auth.RoleAdmin && role != auth.RoleAdmin && adminCount(org) == 1 {
				return ErrLastAdmin
			}
			org.Members[i].Role = role
			org.UpdatedAt = time.Now().UTC()
			return s.store.SaveOrg(ctx, org)
		}
	}
	return ErrMemberNotFound
}

// IsMember reports whether userID is a member of orgID.
func (s *OrgService) IsMember(orgID, userID string) bool {
	org, err := s.store.LoadOrg(context.Background(), orgID)
	if err != nil {
		return false
	}
	for _, m := range org.Members {
		if m.UserID == userID {
			return true
		}
	}
	return false
}

// MemberRole returns the role of userID in orgID.
func (s *OrgService) MemberRole(orgID, userID string) (auth.Role, error) {
	org, err := s.store.LoadOrg(context.Background(), orgID)
	if err != nil {
		return "", err
	}
	for _, m := range org.Members {
		if m.UserID == userID {
			return m.Role, nil
		}
	}
	return "", ErrMemberNotFound
}

// Delete removes an organisation entirely.
func (s *OrgService) Delete(orgID string) error {
	return s.store.DeleteOrg(context.Background(), orgID)
}

func adminCount(org *interfaces.Organisation) int {
	n := 0
	for _, m := range org.Members {
		if m.Role == auth.RoleAdmin {
			n++
		}
	}
	return n
}
