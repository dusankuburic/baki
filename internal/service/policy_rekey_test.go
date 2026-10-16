package service

import (
	"context"
	"testing"

	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// seedIdlessPolicy stores a policy under the empty id, as the release before v48 did for every policy the panel
// created (and its replicas may still, during a rolling deploy or after a rollback): this release's store refuses
// one, so the row is written past it.
func seedIdlessPolicy(t *testing.T, b storageif.StorageBackend, orgID, name string) {
	t.Helper()
	switch be := b.(type) {
	case *testutil.FakeBackend:
		if be.Policies == nil {
			be.Policies = map[string]*models.Policy{}
		}
		be.Policies[orgID+"\x00"] = &models.Policy{ID: "", OrgID: orgID, Name: name, GateSeverity: models.SeverityWarning}
	case *storagedb.PostgresStorageBackend:
		if _, err := be.DB().ExecContext(context.Background(),
			`INSERT INTO policies (id, org_id, name, description, rules, gate_severity) VALUES ('', $1, $2, '', '[]', 'warning')`, orgID, name); err != nil {
			t.Fatalf("seed the id-less policy: %v", err)
		}
	default:
		t.Fatalf("no way to seed an id-less policy on %T", b)
	}
}

// TestPolicyService_IdlessSaveTakesOverTheIdlessRow: an id-less save in an organisation that has a policy under the
// empty id is an edit of that policy (a panel loaded before v48 edits it with the empty id; a replica of the release
// before wrote it there): the row gets the new id and the save's content, instead of a new policy beside it on every
// edit. Without such a row an id-less save is a new policy, as before.
func TestPolicyService_IdlessSaveTakesOverTheIdlessRow(t *testing.T) {
	forEachLoginBackend(t, func(t *testing.T, b storageif.StorageBackend) {
		ctx := context.Background()
		const org = "org-rekey"
		if err := b.SaveOrg(ctx, &storageif.Organisation{ID: org, Name: "Rekey", OwnerID: "owner",
			Members: []storageif.OrgMember{{UserID: "owner", Role: "admin"}}}); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
		seedIdlessPolicy(t, b, org, "Policy 1")
		svc := NewPolicyService(b)

		edit := &models.Policy{OrgID: org, Name: "Policy 1", Rules: []models.PolicyRule{{RuleID: "r1", Enabled: true}}, GateSeverity: models.SeverityError}
		if err := svc.Save(ctx, edit); err != nil {
			t.Fatalf("Save: %v", err)
		}
		list, err := svc.List(ctx, org)
		if err != nil {
			t.Fatal(err)
		}
		if edit.ID == "" || len(list) != 1 || list[0].ID != edit.ID || len(list[0].Rules) != 1 || list[0].GateSeverity != models.SeverityError {
			t.Fatalf("after an id-less edit: saved id %q, list %+v; want the one policy, under the new id, with the edit", edit.ID, list)
		}

		// The empty id is gone, so the next id-less save is a new policy.
		next := &models.Policy{OrgID: org, Name: "Policy 2", GateSeverity: models.SeverityWarning}
		if err := svc.Save(ctx, next); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if list, err := svc.List(ctx, org); err != nil || len(list) != 2 || next.ID == "" || next.ID == edit.ID {
			t.Errorf("a second id-less save = %q, list %d policies (%v); want a new policy beside the first", next.ID, len(list), err)
		}
	})
}
