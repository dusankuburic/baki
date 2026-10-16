package database

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_Lifecycle_ErasureKeepsOrgSpend: erasing a member keeps the AI spend they made for an organisation, as
// the organisation's (the row loses the member's id), and deletes their personal spend. The organisation's daily
// budget and its usage history read the same before and after, and so does the spend of the member who stays.
func TestPostgres_Lifecycle_ErasureKeepsOrgSpend(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	f := lifecycleFixture{t: t, b: b, ctx: ctx}
	run := fmt.Sprintf("%x", time.Now().UnixNano())

	u, w := "spend-u-"+run, "spend-w-"+run
	for _, id := range []string{u, w} {
		f.ok("SaveUser "+id, b.SaveUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "x"}))
	}
	org := "spend-org-" + run
	f.ok("SaveOrg", b.SaveOrg(ctx, &interfaces.Organisation{ID: org, Name: "Spend", OwnerID: w,
		Members: []interfaces.OrgMember{{UserID: w, Role: auth.RoleAdmin}, {UserID: u, Role: auth.RoleMember}}}))

	now := time.Now().UTC()
	for _, m := range []*interfaces.UsageMetric{
		{ID: "use-org-" + run, UserID: u, OrgID: org, Provider: "openai", Model: "m", PromptTokens: 10, CompletionTokens: 5, EstimatedCost: 1.5, CreatedAt: now},
		{ID: "use-own-" + run, UserID: u, Provider: "openai", Model: "m", PromptTokens: 3, CompletionTokens: 1, EstimatedCost: 0.7, CreatedAt: now},
		{ID: "use-w-" + run, UserID: w, OrgID: org, Provider: "openai", Model: "m", PromptTokens: 7, CompletionTokens: 2, EstimatedCost: 2, CreatedAt: now},
	} {
		f.ok("SaveUsageMetric "+m.ID, b.SaveUsageMetric(ctx, m))
	}
	orgSpend := func() float64 {
		t.Helper()
		v, err := b.GetDailyUsage(ctx, w, org)
		f.ok("GetDailyUsage(org)", err)
		return v
	}
	window := interfaces.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	orgHistory := func() float64 {
		t.Helper()
		rows, err := b.LoadUsageAnalytics(ctx, w, []string{org}, "org", window)
		f.ok("LoadUsageAnalytics(org)", err)
		var cost float64
		for _, d := range rows.Daily {
			cost += d.Cost
		}
		return cost
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if got := orgSpend(); !near(got, 3.5) {
		t.Fatalf("the organisation's spend before the erasure = %v, want 3.5", got)
	}
	if got := orgHistory(); !near(got, 3.5) {
		t.Fatalf("the organisation's usage history before the erasure = %v, want 3.5", got)
	}

	rctx, scope := WithLazyRLS(ctx, b, u, false)
	if err := b.DeleteUser(rctx, u); err != nil {
		_ = scope.Rollback()
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := scope.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := orgSpend(); !near(got, 3.5) {
		t.Errorf("the organisation's daily spend after erasing a member = %v, want 3.5 (its spend is kept)", got)
	}
	if got := orgHistory(); !near(got, 3.5) {
		t.Errorf("the organisation's usage history after erasing a member = %v, want 3.5", got)
	}
	if n := f.count(`SELECT count(*) FROM usage_metrics WHERE user_id = $1`, u); n != 0 {
		t.Errorf("%d usage row(s) still name the erased user", n)
	}
	if got := f.str(`SELECT user_id || '|' || org_id FROM usage_metrics WHERE id = $1`, "use-org-"+run); got != "|"+org {
		t.Errorf("the erased member's organisation spend = %q, want it kept as the organisation's, without the member", got)
	}
	if n := f.count(`SELECT count(*) FROM usage_metrics WHERE id = $1`, "use-own-"+run); n != 0 {
		t.Error("the erased user's personal spend survived the erasure")
	}
	if got := f.str(`SELECT user_id FROM usage_metrics WHERE id = $1`, "use-w-"+run); got != w {
		t.Errorf("another member's row = %q, want it untouched", got)
	}
	// The members' own budgets: the one who stays still reads exactly their spend, the erased id reads none. (The
	// empty user, under which the anonymised row now sits, is the scope of local mode without accounts, where no one
	// is erased; TestAIBudget_LocalUserCountsTheirOrgSpend holds that scope to its org spend.)
	if got, err := b.GetDailyUsage(ctx, w, ""); err != nil || !near(got, 2) {
		t.Errorf("GetDailyUsage for the member who stays = %v, %v; want 2", got, err)
	}
	if got, err := b.GetDailyUsage(ctx, u, ""); err != nil || got != 0 {
		t.Errorf("GetDailyUsage for the erased member = %v, %v; want 0", got, err)
	}
}

// TestPostgres_Lifecycle_EmptyIDErasesNothing: DeleteUser of the empty id is refused and leaves the usage rows keyed
// by it, the local user's spend and the organisation spend erasure keeps of former members. It deleted them all.
func TestPostgres_Lifecycle_EmptyIDErasesNothing(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	f := lifecycleFixture{t: t, b: b, ctx: ctx}
	f.ok("SaveUser", b.SaveUser(ctx, &interfaces.User{ID: "spend-owner", Email: "spend-owner@example.com", Password: "x"}))
	f.ok("SaveOrg", b.SaveOrg(ctx, &interfaces.Organisation{ID: "spend-org", Name: "Spend", OwnerID: "spend-owner",
		Members: []interfaces.OrgMember{{UserID: "spend-owner", Role: auth.RoleAdmin}}}))
	now := time.Now().UTC()
	for _, m := range []*interfaces.UsageMetric{
		{ID: "use-local", Provider: "openai", Model: "m", EstimatedCost: 1, CreatedAt: now},
		{ID: "use-former-member", OrgID: "spend-org", Provider: "openai", Model: "m", EstimatedCost: 2, CreatedAt: now},
	} {
		f.ok("SaveUsageMetric "+m.ID, b.SaveUsageMetric(ctx, m))
	}
	if err := b.DeleteUser(ctx, ""); !errors.Is(err, interfaces.ErrUserIDRequired) {
		t.Errorf("DeleteUser of the empty id: %v, want ErrUserIDRequired", err)
	}
	if n := f.count(`SELECT count(*) FROM usage_metrics WHERE user_id = ''`); n != 2 {
		t.Errorf("usage rows of the empty id after DeleteUser of it: %d, want both kept", n)
	}
}
