package database

import (
	"context"
	"testing"
)

// TestRLSHookRunner_HookQueriesRunAsTheUser: post-commit work reaches the database through a fresh RLS scope for
// the request's user, not the pool, where app.current_user_id is unset and every policy allows all rows.
func TestRLSHookRunner_HookQueriesRunAsTheUser(t *testing.T) {
	b := openMigrationTestDB(t)
	currentUser := func(ctx context.Context) string {
		var u string
		if err := b.query(ctx).QueryRowContext(ctx, `SELECT coalesce(current_setting('app.current_user_id', true), '')`).Scan(&u); err != nil {
			t.Fatalf("read app.current_user_id: %v", err)
		}
		return u
	}

	var inHook string
	RLSHookRunner(context.Background(), b, "u-hook")(func(ctx context.Context) { inHook = currentUser(ctx) })
	if inHook != "u-hook" {
		t.Errorf("hook ran as %q, want the request's user u-hook", inHook)
	}
	if onPool := currentUser(context.Background()); onPool != "" {
		t.Errorf("the pool carries user %q; the comparison above proves nothing", onPool)
	}
}
