package database_test

import (
	"context"
	"testing"
	"time"

	"pad-core/models"
)

// TestPostgres_PolicyWaivers_ThePreviousReleaseKeepsThem: v36 is an expand step. The release before writes a policy
// with its own statement, which names none of the new columns: a row it inserts reads with no waivers and no budgets,
// and its update of a policy saved by this release keeps that policy's waivers and budgets.
func TestPostgres_PolicyWaivers_ThePreviousReleaseKeepsThem(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	run := itoa(time.Now().UnixNano())
	orgID := "polw-org-" + run
	// A policy needs its organisation (v48).
	if _, err := b.DB().ExecContext(ctx, `INSERT INTO organisations (id, name, owner_id) VALUES ($1, $1, '')`, orgID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = b.DB().ExecContext(context.Background(), `DELETE FROM organisations WHERE id = $1`, orgID)
	})
	t.Cleanup(func() {
		_, _ = b.DB().ExecContext(context.Background(), `DELETE FROM policies WHERE org_id = $1`, orgID)
	})

	// The v35 release's SavePolicy statement, verbatim.
	oldSave := func(id, name string) {
		t.Helper()
		if _, err := b.DB().ExecContext(ctx,
			`INSERT INTO policies (id, org_id, name, description, rules, gate_severity, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		 ON CONFLICT (id, org_id) DO UPDATE SET
		   name = EXCLUDED.name,
		   description = EXCLUDED.description,
		   rules = EXCLUDED.rules,
		   gate_severity = EXCLUDED.gate_severity,
		   updated_at = NOW()
		 RETURNING created_at, updated_at`,
			id, orgID, name, "", []byte(`[]`), string(models.SeverityError)); err != nil {
			t.Fatalf("the previous release's save: %v", err)
		}
	}

	oldSave("old-"+run, "Old")
	old, err := b.GetPolicy(ctx, orgID, "old-"+run)
	if err != nil || old.Waivers != nil || old.MaxErrors != nil || old.MaxWarnings != nil || old.MaxInfo != nil {
		t.Fatalf("a row the previous release inserted = %+v, %v; want no waivers and no budgets", old, err)
	}

	two := 2
	p := &models.Policy{ID: "new-" + run, OrgID: orgID, Name: "New", Waivers: []models.PolicyWaiver{{FindingKey: "fp-9"}}, MaxWarnings: &two}
	if err := b.SavePolicy(ctx, p); err != nil {
		t.Fatalf("SavePolicy: %v", err)
	}
	oldSave(p.ID, "Renamed by the previous release")
	got, err := b.GetPolicy(ctx, orgID, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.Name != "Renamed by the previous release" || len(got.Waivers) != 1 || got.Waivers[0].FindingKey != "fp-9" || got.MaxWarnings == nil || *got.MaxWarnings != 2 {
		t.Errorf("after the previous release's update = %+v; want the rename with the waiver and the budget kept", got)
	}
}
