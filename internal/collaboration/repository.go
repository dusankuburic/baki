package collaboration

import (
	"context"
	"sync"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// OrgStore abstracts persistence for organisations.
// The in-memory implementation is used in local/desktop mode and in tests.
// Cloud mode wires in a PostgreSQL implementation via the storage backend.
type OrgStore interface {
	SaveOrg(ctx context.Context, org *interfaces.Organisation) error
	LoadOrg(ctx context.Context, id string) (*interfaces.Organisation, error)
	ListOrgsForUser(ctx context.Context, userID string) ([]*interfaces.Organisation, error)
	DeleteOrg(ctx context.Context, id string) error
	MutateOrg(ctx context.Context, id string, fn func(*interfaces.Organisation) error) error

	// Invite operations
	SaveOrgInvite(ctx context.Context, invite *interfaces.OrgInvite) error
	ListOrgInvites(ctx context.Context, orgID string) ([]*interfaces.OrgInvite, error)
	GetOrgInvite(ctx context.Context, orgID, inviteID string) (*interfaces.OrgInvite, error)
	GetOrgInviteByTokenHash(ctx context.Context, tokenHash string) (*interfaces.OrgInvite, error)
	DeleteOrgInvite(ctx context.Context, orgID, inviteID string) error
	MarkOrgInviteAccepted(ctx context.Context, inviteID string, acceptedAt time.Time) error
}

// NewMemOrgStore returns an in-memory OrgStore.
func NewMemOrgStore() OrgStore {
	return &memOrgStore{
		orgs:    make(map[string]*interfaces.Organisation),
		invites: make(map[string]*interfaces.OrgInvite),
	}
}

type memOrgStore struct {
	mu      sync.RWMutex
	orgs    map[string]*interfaces.Organisation
	invites map[string]*interfaces.OrgInvite
}

func (s *memOrgStore) SaveOrg(_ context.Context, org *interfaces.Organisation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := *org
	stored.Members = make([]interfaces.OrgMember, len(org.Members))
	copy(stored.Members, org.Members)
	s.orgs[org.ID] = &stored
	return nil
}

func (s *memOrgStore) LoadOrg(_ context.Context, id string) (*interfaces.Organisation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	org, ok := s.orgs[id]
	if !ok {
		return nil, ErrOrgNotFound
	}
	result := *org
	result.Members = make([]interfaces.OrgMember, len(org.Members))
	copy(result.Members, org.Members)
	return &result, nil
}

func (s *memOrgStore) ListOrgsForUser(_ context.Context, userID string) ([]*interfaces.Organisation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*interfaces.Organisation
	for _, org := range s.orgs {
		for _, m := range org.Members {
			if m.UserID == userID {
				clone := *org
				clone.Members = make([]interfaces.OrgMember, len(org.Members))
				copy(clone.Members, org.Members)
				result = append(result, &clone)
				break
			}
		}
	}
	return result, nil
}

func (s *memOrgStore) DeleteOrg(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[id]; !ok {
		return ErrOrgNotFound
	}
	delete(s.orgs, id)
	// Cascade-delete the org's invites. The Postgres backend relies on an
	// ON DELETE CASCADE foreign key for this; the in-memory store has no such
	// cascade, so without this the invites would be orphaned indefinitely —
	// still returned by List/GetOrgInvites for a non-existent org, and leaking
	// memory for the lifetime of the process.
	for invID, inv := range s.invites {
		if inv.OrgID == id {
			delete(s.invites, invID)
		}
	}
	return nil
}

func (s *memOrgStore) MutateOrg(_ context.Context, id string, fn func(*interfaces.Organisation) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	org, ok := s.orgs[id]
	if !ok {
		return ErrOrgNotFound
	}
	if err := fn(org); err != nil {
		return err
	}
	return nil
}

// ---- Invite operations ----

func (s *memOrgStore) SaveOrgInvite(_ context.Context, invite *interfaces.OrgInvite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.invites {
		if existing.ID == invite.ID {
			continue
		}
		if existing.OrgID == invite.OrgID && existing.Email == invite.Email && existing.AcceptedAt == nil {
			return interfaces.ErrOrgInviteExists
		}
	}
	stored := *invite
	s.invites[invite.ID] = &stored
	return nil
}

func (s *memOrgStore) ListOrgInvites(_ context.Context, orgID string) ([]*interfaces.OrgInvite, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*interfaces.OrgInvite
	for _, inv := range s.invites {
		if inv.OrgID == orgID {
			clone := *inv
			result = append(result, &clone)
		}
	}
	return result, nil
}

func (s *memOrgStore) GetOrgInvite(_ context.Context, orgID, inviteID string) (*interfaces.OrgInvite, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inv, ok := s.invites[inviteID]
	if !ok || inv.OrgID != orgID {
		return nil, ErrInviteNotFound
	}
	clone := *inv
	return &clone, nil
}

func (s *memOrgStore) GetOrgInviteByTokenHash(_ context.Context, tokenHash string) (*interfaces.OrgInvite, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, inv := range s.invites {
		if inv.TokenHash == tokenHash {
			clone := *inv
			return &clone, nil
		}
	}
	return nil, ErrInviteNotFound
}

func (s *memOrgStore) DeleteOrgInvite(_ context.Context, orgID, inviteID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invites[inviteID]
	if !ok || inv.OrgID != orgID {
		return ErrInviteNotFound
	}
	delete(s.invites, inviteID)
	return nil
}

func (s *memOrgStore) MarkOrgInviteAccepted(_ context.Context, inviteID string, acceptedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invites[inviteID]
	if !ok {
		return ErrInviteNotFound
	}
	at := acceptedAt
	inv.AcceptedAt = &at
	return nil
}
