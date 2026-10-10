package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/platform/reqtx"
)

// reqtx.ReadsFiltered is how the layers that must not import this package (the rule-profile cache keys its entries
// by it) learn whether their reads go through a scope or to the pool: it must follow hasRLSTx, including a fail-open
// scope that could not start and a cleared scope.
func TestReadsFiltered_FollowsTheScope(t *testing.T) {
	if reqtx.ReadsFiltered(context.Background()) {
		t.Error("a context without a scope reads filtered")
	}

	b, _ := newFakeBackend(t, 1, nil)
	ctx, scope := WithLazyRLS(context.Background(), b, "u1", false)
	if !reqtx.ReadsFiltered(ctx) {
		t.Error("a request's scope does not read filtered")
	}
	if reqtx.ReadsFiltered(WithRLSTx(ctx, nil)) {
		t.Error("a cleared scope still reads filtered")
	}
	_ = scope.Commit()

	failing, _ := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
		if strings.HasPrefix(query, "SELECT set_config(") {
			return fakeResult{err: errors.New("pool exhausted")}
		}
		return fakeResult{}
	})
	ctx, scope = WithLazyRLS(context.Background(), failing, "u1", true)
	if !reqtx.ReadsFiltered(ctx) {
		t.Error("an unstarted fail-open scope does not read filtered")
	}
	if _, err := failing.query(ctx).ExecContext(ctx, `SELECT 1`); err != nil {
		t.Fatalf("fail-open statement: %v", err)
	}
	if reqtx.ReadsFiltered(ctx) {
		t.Error("a fail-open scope that could not start reads filtered: its reads went to the pool")
	}
	_ = scope.Commit()
}
