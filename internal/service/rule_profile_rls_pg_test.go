package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/auth"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/pgtest"
)

// TestRuleProfileResolver_RLS_Postgres is B4 against the real policies: org_custom_rules is visible to org
// members only, so a non-member collaborator's request-scoped resolve reads no custom rules. That resolve used to
// fill the org's shared profile, hiding the rules from every member for the TTL. Each reader now keeps its own view:
// the member gets the org's rules even right after the non-member's resolve, the non-member never gets them (also
// after a member's), and a read without a scope (a background job, an exempt route) reads every row: the org's own
// entry, which the non-member's scoped resolves are never served from.
func TestRuleProfileResolver_RLS_Postgres(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "")
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })

	for _, id := range []string{"b4-member", "b4-outsider"} {
		if err := pg.CreateUser(ctx, &storageif.User{ID: id, Email: id + "@example.com", Password: "hash", Role: "member"}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
	if err := pg.SaveOrg(ctx, &storageif.Organisation{ID: "b4-org", Name: "B4", OwnerID: "b4-member",
		Members: []storageif.OrgMember{{UserID: "b4-member", Role: auth.RoleAdmin}}}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	storeRuleIn(t, pg, "b4-org", "b4-org-rule")

	r := NewRuleProfileResolver(pg, deploymentSettings(nil), nil)
	// profileAs resolves under userID's request scope, as a request on an RLS route does, and ends the scope.
	profileAs := func(userID string) *RuleProfile {
		t.Helper()
		reqCtx := auth.WithActor(ctx, userActor(userID, auth.RoleMember))
		reqCtx, scope := storagedb.WithLazyRLS(reqCtx, pg, userID, false)
		defer func() {
			if err := scope.Commit(); err != nil {
				t.Errorf("end %s's scope: %v", userID, err)
			}
		}()
		return r.Resolve(reqCtx, "b4-org")
	}
	resolveAs := func(userID string) bool {
		t.Helper()
		return hasRule(profileAs(userID).Rules, "b4-org-rule")
	}

	if resolveAs("b4-outsider") {
		t.Fatal("premise: the RLS policy shows a non-member the org's custom rules")
	}
	if !resolveAs("b4-member") {
		t.Error("B4: a member resolving right after a non-member got the non-member's profile, without the org's custom rules")
	}
	if resolveAs("b4-outsider") {
		t.Error("a non-member was served the member's profile: the org's custom rules leaked past the RLS policy")
	}
	if !hasRule(r.Resolve(ctx, "b4-org").Rules, "b4-org-rule") {
		t.Error("a background job's resolve (no RLS scope) is missing the org's custom rules")
	}

	// The non-member on an exempt route (the pool) reads every row; their next scoped resolve must not be served it.
	r.Invalidate("b4-org")
	if !hasRule(r.Resolve(auth.WithActor(ctx, userActor("b4-outsider", auth.RoleMember)), "b4-org").Rules, "b4-org-rule") {
		t.Fatal("premise: a read without a scope shows every row")
	}
	if resolveAs("b4-outsider") {
		t.Error("a non-member's scoped resolve was served their pool resolve, with the org's custom rules")
	}

	// The membership read in the scope tells the views apart (caches-1): only the non-member's is narrowed.
	r.Invalidate("b4-org")
	if profileAs("b4-member").Narrowed {
		t.Error("a member's scoped view is marked narrowed")
	}
	if !profileAs("b4-outsider").Narrowed {
		t.Error("a non-member's scoped view is not marked narrowed")
	}
}

func storeRuleIn(t *testing.T, store storageif.OrgCustomRuleStore, orgID, ruleID string) {
	t.Helper()
	b := testutil.NewFakeBackend()
	storeRule(t, b, orgID, ruleID, "^SET$", true)
	rules, err := b.ListOrgCustomRules(context.Background(), orgID, true)
	if err != nil || len(rules) != 1 {
		t.Fatalf("stage the rule: %v (%d rows)", err, len(rules))
	}
	if err := store.SaveOrgCustomRule(context.Background(), rules[0]); err != nil {
		t.Fatalf("SaveOrgCustomRule: %v", err)
	}
}
