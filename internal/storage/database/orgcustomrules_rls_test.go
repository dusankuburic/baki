package database_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_OrgCustomRules_RenameUnderRLS runs the rename guard the way a request does, inside an org admin's
// RLS transaction: renaming the admin's own rule is ErrOrgCustomRuleRenamed (it used to be a primary-key violation,
// a 500), saving through another org's row id is ErrNotFound (the row is invisible, and its key still collides),
// and neither changes a row. The contract suite covers the same without RLS.
func TestPostgres_OrgCustomRules_RenameUnderRLS(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	admin := azureTestUser(t, b, "cr-admin")
	other := azureTestUser(t, b, "cr-other")
	orgA, orgB := "cr-org-a-"+admin, "cr-org-b-"+other
	for _, o := range []*interfaces.Organisation{
		{ID: orgA, Name: "A", OwnerID: admin, Members: []interfaces.OrgMember{{UserID: admin, Role: "admin"}}},
		{ID: orgB, Name: "B", OwnerID: other, Members: []interfaces.OrgMember{{UserID: other, Role: "admin"}}},
	} {
		if err := b.SaveOrg(ctx, o); err != nil {
			t.Fatalf("SaveOrg(%s): %v", o.ID, err)
		}
		t.Cleanup(func() { _ = b.DeleteOrg(ctx, o.ID) })
	}
	cfg := func(id, match string) json.RawMessage {
		return json.RawMessage(`{"kind":"block","schemaVersion":1,"id":"` + id + `","rawTypeMatch":"` + match + `"}`)
	}
	ruleA := &interfaces.OrgCustomRule{ID: "cr-row-a-" + admin, OrgID: orgA, RuleID: "rule-a", Config: cfg("rule-a", "^SET$"), Enabled: true}
	ruleB := &interfaces.OrgCustomRule{ID: "cr-row-b-" + other, OrgID: orgB, RuleID: "rule-b", Config: cfg("rule-b", "^WAIT$"), Enabled: true}
	for _, r := range []*interfaces.OrgCustomRule{ruleA, ruleB} {
		if err := b.SaveOrgCustomRule(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.ID, err)
		}
	}

	tx, err := b.BeginRLS(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rctx := database.WithRLSTx(ctx, tx)

	err = b.SaveOrgCustomRule(rctx, &interfaces.OrgCustomRule{ID: ruleA.ID, OrgID: orgA, RuleID: "renamed", Config: cfg("renamed", "^IF$"), Enabled: true})
	if !errors.Is(err, interfaces.ErrOrgCustomRuleRenamed) {
		t.Fatalf("renaming the admin's own rule: err = %v, want ErrOrgCustomRuleRenamed", err)
	}
	// The refused statement left the transaction usable.
	err = b.SaveOrgCustomRule(rctx, &interfaces.OrgCustomRule{ID: ruleB.ID, OrgID: orgA, RuleID: "intruder", Config: cfg("intruder", "^IF$"), Enabled: true})
	if !errors.Is(err, interfaces.ErrNotFound) {
		t.Fatalf("saving through another org's row id: err = %v, want ErrNotFound", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// A fresh request: the ordinary re-save still works under RLS.
	tx, err = b.BeginRLS(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	rctx = database.WithRLSTx(ctx, tx)
	resave := &interfaces.OrgCustomRule{ID: ruleA.ID, OrgID: orgA, RuleID: "rule-a", Config: cfg("rule-a", "^IF$"), Enabled: false}
	if err := b.SaveOrgCustomRule(rctx, resave); err != nil {
		t.Fatalf("re-save under RLS: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for org, want := range map[string]struct{ id, ruleID, match string }{
		orgA: {ruleA.ID, "rule-a", "^IF$"},
		orgB: {ruleB.ID, "rule-b", "^WAIT$"},
	} {
		rows, err := b.ListOrgCustomRules(ctx, org, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != want.id || rows[0].RuleID != want.ruleID {
			t.Fatalf("%s rows = %+v, want only %s", org, rows, want.id)
		}
		var c struct {
			RawTypeMatch string `json:"rawTypeMatch"`
		}
		if err := json.Unmarshal(rows[0].Config, &c); err != nil || c.RawTypeMatch != want.match {
			t.Errorf("%s config = %s, want rawTypeMatch %s", org, rows[0].Config, want.match)
		}
	}
}
