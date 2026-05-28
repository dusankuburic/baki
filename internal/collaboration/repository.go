package collaboration

import (
	"context"
	"sync"

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
}

// NewMemOrgStore returns an in-memory OrgStore.
func NewMemOrgStore() OrgStore {
	return &memOrgStore{orgs: make(map[string]*interfaces.Organisation)}
}

type memOrgStore struct {
	mu   sync.RWMutex
	orgs map[string]*interfaces.Organisation
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
	return nil
}
