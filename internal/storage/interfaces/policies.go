package interfaces

import (
	"context"
	"errors"

	"pad-core/models"
)

// ErrPolicyIDRequired is SavePolicy's answer to a policy without an id: stored under the empty id, an organisation's id-less
// policies overwrote each other. PolicyService gives a new policy one.
var ErrPolicyIDRequired = errors.New("a policy needs an id")

// PolicyStore covers org-scoped governance policies (named rule sets with a pass/fail gate severity); filesystem backend stubs these as no-ops.
type PolicyStore interface {
	// SavePolicy upserts p by (id, org); an empty id is ErrPolicyIDRequired, an organisation that does not exist
	// ErrOrgNotFound.
	SavePolicy(ctx context.Context, p *models.Policy) error
	// RekeyIdlessPolicy gives orgID's policy stored under the empty id the id id, and reports whether it had one. The
	// release before v48 stored every policy saved without an id there (its replicas still may, during a rolling
	// deploy or after a rollback), and v48 refuses the empty id.
	RekeyIdlessPolicy(ctx context.Context, orgID, id string) (bool, error)
	GetPolicy(ctx context.Context, orgID, id string) (*models.Policy, error)
	ListPolicies(ctx context.Context, orgID string) ([]*models.Policy, error)
	DeletePolicy(ctx context.Context, orgID, id string) error
}
