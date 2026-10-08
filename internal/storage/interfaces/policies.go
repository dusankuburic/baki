package interfaces

import (
	"context"

	"pad-core/models"
)

// PolicyStore covers org-scoped governance policies (named rule sets with a pass/fail gate severity); filesystem backend stubs these as no-ops.
type PolicyStore interface {
	SavePolicy(ctx context.Context, p *models.Policy) error
	GetPolicy(ctx context.Context, orgID, id string) (*models.Policy, error)
	ListPolicies(ctx context.Context, orgID string) ([]*models.Policy, error)
	DeletePolicy(ctx context.Context, orgID, id string) error
}
