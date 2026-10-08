package database

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// TestCreateUser_ConcurrentFirstRegistrationsMakeOneAdmin: of registrations racing on an empty table exactly one
// becomes the bootstrap admin, and none fails.
func TestCreateUser_ConcurrentFirstRegistrationsMakeOneAdmin(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := auth.WithAllowBootstrap(context.Background(), true)
	roles := registerConcurrently(t, b, ctx, "first", 12)
	admins := 0
	for _, r := range roles {
		if r == auth.RoleAdmin {
			admins++
		}
	}
	if admins != 1 {
		t.Errorf("%d bootstrap admins among %d concurrent first registrations, want 1", admins, len(roles))
	}
}

// TestCreateUser_ABurstOfSignUpsAllSucceed: registrations on a table that has users take no lock and cannot lose a
// serialization race (under SERIALIZABLE the emptiness read failed about a quarter of a burst of 16).
func TestCreateUser_ABurstOfSignUpsAllSucceed(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := auth.WithAllowBootstrap(context.Background(), true)
	for i := range 20 {
		if err := b.CreateUser(ctx, &interfaces.User{ID: fmt.Sprintf("seed-%d", i), Email: fmt.Sprintf("seed-%d@example.com", i), Password: "h", Role: auth.RoleMember}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for round := range 3 {
		for _, r := range registerConcurrently(t, b, ctx, fmt.Sprintf("burst-%d", round), 16) {
			if r != auth.RoleMember {
				t.Errorf("a sign-up on a populated table became %q", r)
			}
		}
	}
}

// registerConcurrently registers n users at once and returns their roles; any error fails the test.
func registerConcurrently(t *testing.T, b *PostgresStorageBackend, ctx context.Context, prefix string, n int) []auth.Role {
	t.Helper()
	roles := make([]auth.Role, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			u := &interfaces.User{ID: fmt.Sprintf("%s-%d", prefix, i), Email: fmt.Sprintf("%s-%d@example.com", prefix, i), Password: "h", Role: auth.RoleMember}
			errs[i] = b.CreateUser(ctx, u)
			roles[i] = u.Role
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("registration %d: %v", i, err)
		}
	}
	return roles
}
