package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_Lifecycle_ErasureKeepsOrgUsage: erasing a member keeps the AI usage they made for an organisation, as
// the organisation's (the row loses the member's id), and deletes their personal usage. The organisation's usage
// history reads the same before and after, and so does the usage of the member who stays.
func TestPostgres_Lifecycle_ErasureKeepsOrgUsage(t *testing.T) {
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
		{ID: "use-org-" + run, UserID: u, OrgID: org, Provider: "openai", Model: "m", PromptTokens: 10, CompletionTokens: 5, CreatedAt: now},
		{ID: "use-own-" + run, UserID: u, Provider: "openai", Model: "m", PromptTokens: 3, CompletionTokens: 1, CreatedAt: now},
		{ID: "use-w-" + run, UserID: w, OrgID: org, Provider: "openai", Model: "m", PromptTokens: 7, CompletionTokens: 2, CreatedAt: now},
	} {
		f.ok("SaveUsageMetric "+m.ID, b.SaveUsageMetric(ctx, m))
	}
	window := interfaces.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	orgHistory := func() int {
		t.Helper()
		rows, err := b.LoadUsageAnalytics(ctx, w, []string{org}, "org", window)
		f.ok("LoadUsageAnalytics(org)", err)
		tokens := 0
		for _, d := range rows.Daily {
			tokens += d.TokensIn + d.TokensOut
		}
		return tokens
	}
	if got := orgHistory(); got != 24 {
		t.Fatalf("the organisation's usage history before the erasure = %d tokens, want 24", got)
	}

	rctx, scope := WithLazyRLS(ctx, b, u, false)
	if err := b.DeleteUser(rctx, u); err != nil {
		_ = scope.Rollback()
		t.Fatalf("DeleteUser: %v", err)
	}
	if err := scope.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := orgHistory(); got != 24 {
		t.Errorf("the organisation's usage history after erasing a member = %d tokens, want 24 (its usage is kept)", got)
	}
	if n := f.count(`SELECT count(*) FROM usage_metrics WHERE user_id = $1`, u); n != 0 {
		t.Errorf("%d usage row(s) still name the erased user", n)
	}
	if got := f.str(`SELECT user_id || '|' || org_id FROM usage_metrics WHERE id = $1`, "use-org-"+run); got != "|"+org {
		t.Errorf("the erased member's organisation usage = %q, want it kept as the organisation's, without the member", got)
	}
	if n := f.count(`SELECT count(*) FROM usage_metrics WHERE id = $1`, "use-own-"+run); n != 0 {
		t.Error("the erased user's personal usage survived the erasure")
	}
	if got := f.str(`SELECT user_id FROM usage_metrics WHERE id = $1`, "use-w-"+run); got != w {
		t.Errorf("another member's row = %q, want it untouched", got)
	}
	// The member who stays still owns exactly their usage; the erased id owns none.
	if n := f.count(`SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)::int FROM usage_metrics WHERE user_id = $1`, w); n != 9 {
		t.Errorf("tokens of the member who stays = %d, want 9", n)
	}
}

// TestPostgres_Lifecycle_EmptyIDErasesNothing: DeleteUser of the empty id is refused and leaves the usage rows keyed
// by it, the local user's usage and the organisation usage erasure keeps of former members. It deleted them all.
func TestPostgres_Lifecycle_EmptyIDErasesNothing(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	f := lifecycleFixture{t: t, b: b, ctx: ctx}
	f.ok("SaveUser", b.SaveUser(ctx, &interfaces.User{ID: "spend-owner", Email: "spend-owner@example.com", Password: "x"}))
	f.ok("SaveOrg", b.SaveOrg(ctx, &interfaces.Organisation{ID: "spend-org", Name: "Spend", OwnerID: "spend-owner",
		Members: []interfaces.OrgMember{{UserID: "spend-owner", Role: auth.RoleAdmin}}}))
	now := time.Now().UTC()
	for _, m := range []*interfaces.UsageMetric{
		{ID: "use-local", Provider: "openai", Model: "m", PromptTokens: 1, CreatedAt: now},
		{ID: "use-former-member", OrgID: "spend-org", Provider: "openai", Model: "m", PromptTokens: 2, CreatedAt: now},
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
