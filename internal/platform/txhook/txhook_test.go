package txhook

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestRegister_WaitsForCommitThenRunsInOrder(t *testing.T) {
	ctx, reg := With(context.Background())
	var ran []string
	Register(ctx, func(context.Context) { ran = append(ran, "a") })
	Register(ctx, func(context.Context) { ran = append(ran, "b") })
	if len(ran) != 0 {
		t.Fatalf("hooks ran before the commit: %v", ran)
	}
	reg.Commit(nil)
	if !slices.Equal(ran, []string{"a", "b"}) {
		t.Fatalf("ran %v, want [a b]", ran)
	}
	// After the commit the write is durable: a late registration runs at once.
	Register(ctx, func(context.Context) { ran = append(ran, "late") })
	if !slices.Equal(ran, []string{"a", "b", "late"}) {
		t.Fatalf("late hook: ran %v", ran)
	}
	if !Has(ctx) {
		t.Error("Has must stay true after a commit")
	}
}

// TestAbort_DropsRegisteredAndLateHooks: the write a hook follows was rolled back, so the hook (a blob purge, an
// email) must never run, not even when it is registered after the rollback.
func TestAbort_DropsRegisteredAndLateHooks(t *testing.T) {
	ctx, reg := With(context.Background())
	ran := false
	Register(ctx, func(context.Context) { ran = true })
	reg.Abort()
	Register(ctx, func(context.Context) { ran = true })
	reg.Commit(nil) // a commit after the abort changes nothing
	if ran {
		t.Fatal("a hook ran although its transaction rolled back")
	}
	if Has(ctx) {
		t.Error("Has must be false after an abort, so unsafe work is not registered")
	}
}

func TestRegister_WithoutRegistryRunsNowDetached(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	Register(ctx, func(hctx context.Context) {
		ran = true
		if hctx.Err() != nil {
			t.Error("the hook's context must not inherit the caller's cancellation")
		}
	})
	if !ran {
		t.Fatal("without a registry the write is already durable: the hook must run at once")
	}
	if Has(ctx) {
		t.Error("Has without a registry")
	}
}

func TestCommit_RunnerChoosesTheContextAndPanicsAreContained(t *testing.T) {
	type key struct{}
	ctx, reg := With(context.Background())
	var got []any
	Register(ctx, func(context.Context) { panic("first hook fails") })
	Register(ctx, func(hctx context.Context) { got = append(got, hctx.Value(key{})) })
	reg.Commit(func(fn Hook) { fn(context.WithValue(context.Background(), key{}, "owner-ctx")) })
	if !slices.Equal(got, []any{"owner-ctx"}) {
		t.Fatalf("second hook got %v: a panic must not stop the others, and the runner picks the context", got)
	}
}

func TestDetached_DropsCancellationKeepsValues(t *testing.T) {
	type key struct{}
	base, cancel := context.WithTimeout(context.WithValue(context.Background(), key{}, "v"), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	Detached(base)(func(ctx context.Context) {
		if ctx.Err() != nil || ctx.Value(key{}) != "v" {
			t.Errorf("err=%v value=%v, want no cancellation and the base's values", ctx.Err(), ctx.Value(key{}))
		}
	})
}

func TestOnRollback_RunsOnlyOnARollback(t *testing.T) {
	var ran []string
	ctx, reg := With(context.Background())
	Register(ctx, func(context.Context) { ran = append(ran, "commit") })
	OnRollback(ctx, func(context.Context) { ran = append(ran, "rollback") })
	reg.Abort()
	if !slices.Equal(ran, []string{"rollback"}) {
		t.Fatalf("after Abort ran %v, want [rollback]", ran)
	}
	OnRollback(ctx, func(context.Context) { ran = append(ran, "late") })
	if !slices.Equal(ran, []string{"rollback", "late"}) {
		t.Fatalf("a hook registered after the rollback did not run at once: %v", ran)
	}

	ran = nil
	ctx, reg = With(context.Background())
	OnRollback(ctx, func(context.Context) { ran = append(ran, "rollback") })
	reg.Commit(nil)
	OnRollback(ctx, func(context.Context) { ran = append(ran, "late") })
	if len(ran) != 0 {
		t.Fatalf("rollback hooks ran on a commit: %v", ran)
	}

	OnRollback(context.Background(), func(context.Context) { ran = append(ran, "no tx") })
	if len(ran) != 0 {
		t.Fatalf("a rollback hook ran without a transaction: %v", ran)
	}
}

func TestPendingAndPublished(t *testing.T) {
	if Pending(context.Background()) || Published(context.Background()) {
		t.Fatal("no transaction reported as open")
	}
	MarkPublished(context.Background()) // no-op without a registry

	ctx, reg := With(context.Background())
	if !Pending(ctx) || Published(ctx) {
		t.Fatalf("open transaction: pending %v published %v, want true false", Pending(ctx), Published(ctx))
	}
	MarkPublished(ctx)
	if !Published(ctx) {
		t.Fatal("MarkPublished not recorded")
	}
	reg.Commit(nil)
	if Pending(ctx) || Published(ctx) {
		t.Fatalf("after the commit: pending %v published %v, want false false", Pending(ctx), Published(ctx))
	}
	MarkPublished(ctx)
	if Published(ctx) {
		t.Fatal("MarkPublished after the commit was recorded")
	}
}
