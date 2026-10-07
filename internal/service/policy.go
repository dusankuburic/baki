package service

import (
	"context"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// PolicyService owns an organisation's persisted CI gate policies (named rule sets with a pass/fail
// gate severity). Callers authorize first: reads need org membership, writes org admin.
type PolicyService struct {
	store storageif.PolicyStore
}

func NewPolicyService(store storageif.PolicyStore) *PolicyService {
	return &PolicyService{store: store}
}

// Save upserts p on (id, org).
func (s *PolicyService) Save(ctx context.Context, p *models.Policy) error {
	return s.store.SavePolicy(ctx, p)
}

func (s *PolicyService) List(ctx context.Context, orgID string) ([]*models.Policy, error) {
	return s.store.ListPolicies(ctx, orgID)
}

func (s *PolicyService) Get(ctx context.Context, orgID, id string) (*models.Policy, error) {
	return s.store.GetPolicy(ctx, orgID, id)
}

func (s *PolicyService) Delete(ctx context.Context, orgID, id string) error {
	return s.store.DeletePolicy(ctx, orgID, id)
}
