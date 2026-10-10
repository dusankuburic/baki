package service

import (
	"context"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/cache"
)

// B7: an analytics payload depends on whether the caller is an instance admin (the instance-wide locked-account
// count), but the cache key was the user and the query: for the TTL, a session of the same user without the admin
// role (a role change, an old token) was served the admin's view, and the reverse. The flag is part of the key.
func TestBuildAnalytics_AdminFlagIsPartOfTheKey(t *testing.T) {
	q := anaQuery(30)
	scope := &stubScope{ids: []string{"A"}}
	stub := newAnaStub()
	stub.security = &storageif.SecurityRows{LockedRead: true, LockedAccounts: 3}
	svc := NewDashboardService(stub, nil, nil, nil)
	svc.scope = scope
	ctx := context.Background()
	build := func(admin bool) (locked *int) {
		t.Helper()
		out, err := svc.BuildAnalytics(ctx, "user-1", AnalyticsCaller{IsAdmin: admin}, q)
		if err != nil {
			t.Fatalf("BuildAnalytics: %v", err)
		}
		return out.Security.LockedAccounts
	}

	if got := build(true); got == nil || *got != 3 {
		t.Fatalf("premise: the admin's view carries the instance-wide locked count, got %v", got)
	}
	if got := build(false); got != nil {
		t.Errorf("a non-admin session of the same user was served the admin's view (locked accounts = %d)", *got)
	}
	if got := build(true); got == nil {
		t.Error("the admin session was served the non-admin view")
	}
	if n := scope.count(); n != 2 {
		t.Errorf("%d builds for two sessions' three calls, want 2 (one cached view per session)", n)
	}
	svc.InvalidateDashboardCache("user-1")
	build(true)
	build(false)
	if n := scope.count(); n != 4 {
		t.Errorf("dropping the user's views left one of them cached (%d builds, want 4)", n)
	}
}

// The home view is cached per user and dropped with the user's views; a drop of every view retires all of them.
func TestBuildHome_CachedPerUserAndDroppedOnInvalidation(t *testing.T) {
	store := &gatedDashStore{FakeBackend: testutil.NewFakeBackend()}
	svc := NewDashboardService(store, nil, nil, nil)
	ctx := context.Background()
	stats := func() cache.Stats { return homeViews.Info().Stats }

	before := stats()
	a := svc.BuildHome(ctx, "u1")
	if svc.BuildHome(ctx, "u1") != a || store.builds.Load() != 1 {
		t.Fatalf("a repeat load rebuilt the home view (%d builds)", store.builds.Load())
	}
	svc.BuildHome(ctx, "u2")
	if store.builds.Load() != 2 {
		t.Fatalf("another user's load was served u1's view (%d builds)", store.builds.Load())
	}
	after := stats()
	if after.Hits-before.Hits != 1 || after.Misses-before.Misses != 2 || after.Stores-before.Stores != 2 {
		t.Errorf("home cache counted %d hits, %d misses, %d stores; want 1, 2, 2",
			after.Hits-before.Hits, after.Misses-before.Misses, after.Stores-before.Stores)
	}

	svc.InvalidateFlowViews("u1", "")
	svc.BuildHome(ctx, "u1")
	svc.BuildHome(ctx, "u2")
	if store.builds.Load() != 3 {
		t.Errorf("an owner's drop: %d builds, want 3 (u1 rebuilt, u2 served)", store.builds.Load())
	}
	svc.InvalidateDashboardCache("")
	svc.BuildHome(ctx, "u1")
	svc.BuildHome(ctx, "u2")
	if store.builds.Load() != 5 {
		t.Errorf("a drop of every view: %d builds, want 5 (both rebuilt)", store.builds.Load())
	}
}
