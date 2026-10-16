package service

import (
	"context"

	"github.com/google/uuid"

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

// Save upserts p on (id, org). A policy without an id gets a generated id, written back onto p: the policy panel
// created policies without one, and stored under the empty id they overwrote each other. When the organisation still
// has a policy under the empty id (a replica of the release before v48 wrote it, during a rolling deploy or after a
// rollback), the id-less save is an edit of it, from a panel that listed it: that row takes the new id and this
// save's content, where a new policy beside it on every edit would pile up gates the panel never shows. Otherwise it
// is a new policy.
func (s *PolicyService) Save(ctx context.Context, p *models.Policy) error {
	if p.ID == "" {
		p.ID = uuid.NewString()
		if _, err := s.store.RekeyIdlessPolicy(ctx, p.OrgID, p.ID); err != nil {
			return err
		}
	}
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
