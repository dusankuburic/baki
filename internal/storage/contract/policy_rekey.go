package contract

import (
	"context"
	"errors"
	"testing"

	"pad-core/models"

	"pad-analyzer/internal/storage/interfaces"
)

// runPolicyRekeyScenarios holds RekeyIdlessPolicy to what the interface can see: it finds nothing to move in an
// organisation without a policy under the empty id (this release never stores one; the row it moves is one the
// release before v48 wrote, which the service tests seed past the store), touches no other policy, and refuses to
// move a row to the empty id.
func runPolicyRekeyScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(name string, fn func(t *testing.T))) {
	run("RekeyIdlessPolicy_without_an_idless_policy_moves_nothing", func(t *testing.T) {
		org := "contract-rekey-org-" + runID
		seedOrg(ctx, t, b, org, "contract-rekey-admin-"+runID)
		kept := &models.Policy{ID: "contract-rekey-kept-" + runID, OrgID: org, Name: "Kept", GateSeverity: models.SeverityWarning}
		if err := b.SavePolicy(ctx, kept); err != nil {
			t.Fatalf("SavePolicy: %v", err)
		}
		if moved, err := b.RekeyIdlessPolicy(ctx, org, "contract-rekey-new-"+runID); err != nil || moved {
			t.Errorf("RekeyIdlessPolicy = %v, %v; want nothing to move", moved, err)
		}
		if list, err := b.ListPolicies(ctx, org); err != nil || len(list) != 1 || list[0].ID != kept.ID {
			t.Errorf("ListPolicies after = %+v, %v; want the one policy as it was", list, err)
		}
		if moved, err := b.RekeyIdlessPolicy(ctx, "contract-rekey-missing-"+runID, "x"); err != nil || moved {
			t.Errorf("RekeyIdlessPolicy(unknown org) = %v, %v; want nothing to move", moved, err)
		}
		if _, err := b.RekeyIdlessPolicy(ctx, org, ""); !errors.Is(err, interfaces.ErrPolicyIDRequired) {
			t.Errorf("RekeyIdlessPolicy to the empty id: %v, want ErrPolicyIDRequired", err)
		}
	})
}
