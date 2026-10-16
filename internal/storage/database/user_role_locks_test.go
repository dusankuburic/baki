package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// seedRoleUsers creates the accounts ids with role.
func seedRoleUsers(t *testing.T, b *PostgresStorageBackend, role auth.Role, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := b.CreateUser(context.Background(), &interfaces.User{ID: id, Email: id + "@example.com", Password: "h", Role: role}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}
}

// timedCall is one store call run with a deadline: what it returned and how long it took.
type timedCall struct {
	what string
	err  error
	took time.Duration
}

// callWithin runs fn under a deadline of limit and reports its outcome on out.
func callWithin(out chan<- timedCall, what string, limit time.Duration, fn func(ctx context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start := time.Now()
	err := fn(ctx)
	out <- timedCall{what: what, err: err, took: time.Since(start)}
}

// waitForLockWaiters waits until n sessions of this database wait on a lock.
func waitForLockWaiters(t *testing.T, b *PostgresStorageBackend, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := b.db.QueryRowContext(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions wait on a lock after 5 s, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestUpdateUserRole_TakesNoLockForeignKeyChecksWaitFor (ACC-1): a role change locked every admin row FOR UPDATE, the
// one row lock that the FOR KEY SHARE of a foreign-key check (a session, an Idempotency-Key claim, a collaborator, a
// settings save) conflicts with. With an admin's request transaction holding such a child row, any role change waited
// for it, another admin's sign-in and refresh waited behind the role change, and the holder then asking for a second
// admin's key share deadlocked with it (40P01, a 500). A demotion of an admin now locks the admin rows FOR NO KEY
// UPDATE, and a change between other roles locks no admin row.
func TestUpdateUserRole_TakesNoLockForeignKeyChecksWaitFor(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	seedRoleUsers(t, b, auth.RoleAdmin, "adm-1", "adm-2", "adm-3")
	seedRoleUsers(t, b, auth.RoleMember, "mem-1")

	// adm-2's request transaction, holding the key share a child row's insert takes on its account.
	holder, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM users WHERE id = 'adm-2' FOR KEY SHARE`); err != nil {
		t.Fatalf("key share on adm-2: %v", err)
	}

	calls := make(chan timedCall, 4)
	var wg sync.WaitGroup
	wg.Go(func() {
		callWithin(calls, "a member made a viewer", 4*time.Second, func(ctx context.Context) error {
			return b.UpdateUserRole(ctx, "mem-1", auth.RoleViewer)
		})
	})
	wg.Go(func() {
		callWithin(calls, "an admin demoted (two left)", 4*time.Second, func(ctx context.Context) error {
			return b.UpdateUserRole(ctx, "adm-3", auth.RoleMember)
		})
	})
	time.Sleep(200 * time.Millisecond) // the role changes take their locks first
	wg.Go(func() {
		callWithin(calls, "adm-1's right password", 1500*time.Millisecond, func(ctx context.Context) error {
			return b.ResetFailedLogins(ctx, "adm-1")
		})
	})
	wg.Go(func() {
		callWithin(calls, "adm-1's refresh", 1500*time.Millisecond, func(ctx context.Context) error {
			return b.StoreRefreshToken(ctx, "jti-adm-1", "adm-1", time.Now().Add(time.Hour), "", "")
		})
	})
	time.Sleep(200 * time.Millisecond)
	// The holder's request now writes a child row of another admin (adds adm-1 as a collaborator, say).
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, herr := holder.ExecContext(hctx, `SELECT 1 FROM users WHERE id = 'adm-1' FOR KEY SHARE`)
	cancel()
	if herr != nil {
		t.Errorf("the holder's key share on adm-1: %v", herr)
	}
	if err := holder.Commit(); err != nil && herr == nil {
		t.Errorf("commit the holder: %v", err)
	}
	wg.Wait()
	close(calls)
	for c := range calls {
		if c.err != nil || c.took > time.Second {
			t.Errorf("%s while an admin's request holds a child row: %v after %v; want done at once", c.what, c.err, c.took.Round(time.Millisecond))
		}
	}
	for id, want := range map[string]auth.Role{"mem-1": auth.RoleViewer, "adm-3": auth.RoleMember} {
		if u, err := b.LoadUserByID(ctx, id); err != nil || u.Role != want {
			t.Errorf("%s after the role changes: %+v, %v; want %s", id, u, err, want)
		}
	}

	// A change between member and viewer locks no admin row: it goes through while an admin row is being written. A
	// demotion of an admin waits for that write.
	writer, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.ExecContext(ctx, `UPDATE users SET updated_at = updated_at WHERE id = 'adm-1'`); err != nil {
		t.Fatal(err)
	}
	mctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	start := time.Now()
	err = b.UpdateUserRole(mctx, "mem-1", auth.RoleMember)
	cancel()
	if err != nil || time.Since(start) > time.Second {
		t.Errorf("a viewer made a member while an admin row is written: %v after %v; want done at once", err, time.Since(start).Round(time.Millisecond))
	}
	demoted := make(chan error, 1)
	go func() { demoted <- b.UpdateUserRole(context.Background(), "adm-2", auth.RoleMember) }()
	waitForLockWaiters(t, b, 1)
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-demoted; err != nil {
		t.Errorf("demote adm-2 after the write: %v", err)
	}
}

// TestUpdateUserRole_ConcurrentDemotionsLeaveOneAdmin: the last two admins demote each other while a transaction
// writes both their rows, so both demotions are in flight at once; exactly one is refused. Without the admin row
// locks both read two admins and both demote.
func TestUpdateUserRole_ConcurrentDemotionsLeaveOneAdmin(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	seedRoleUsers(t, b, auth.RoleAdmin, "adm-a", "adm-b")
	holder, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `UPDATE users SET updated_at = updated_at WHERE role = 'admin'`); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range []string{"adm-a", "adm-b"} {
		wg.Go(func() { errs[i] = b.UpdateUserRole(context.Background(), id, auth.RoleMember) })
	}
	waitForLockWaiters(t, b, 2)
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	refused := 0
	for i, err := range errs {
		switch {
		case errors.Is(err, interfaces.ErrLastAdmin):
			refused++
		case err != nil:
			t.Errorf("demotion %d: %v", i, err)
		}
	}
	admins, err := b.ListAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if refused != 1 || len(admins) != 1 {
		t.Errorf("%d demotions refused, %d admins left; want 1 and 1", refused, len(admins))
	}
}

// TestUpdateUserRole_DemotionWaitsForAnAdminsErasure: the admin rows are still locked against a delete, so a
// demotion that waits for an admin's erasure counts the admins the erasure leaves. Two admins: one is erased while
// the other is demoted; the demotion is refused.
func TestUpdateUserRole_DemotionWaitsForAnAdminsErasure(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	seedRoleUsers(t, b, auth.RoleAdmin, "adm-x", "adm-y")
	eraser, err := b.BeginRLS(ctx, "adm-y")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eraser.Rollback() }()
	if err := b.DeleteUser(WithRLSTx(ctx, eraser), "adm-y"); err != nil {
		t.Fatalf("erase adm-y: %v", err)
	}
	demoted := make(chan error, 1)
	go func() { demoted <- b.UpdateUserRole(context.Background(), "adm-x", auth.RoleMember) }()
	waitForLockWaiters(t, b, 1)
	if err := eraser.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-demoted; !errors.Is(err, interfaces.ErrLastAdmin) {
		t.Errorf("demoting the admin an erasure leaves alone: %v, want ErrLastAdmin", err)
	}
}

// TestUpdateUserRole_NoDeadlockWithTheOtherAccountWrites runs role changes among three admins against every other
// write of an admin's row at once, for a second: wrong and right passwords, sessions, key shares of a request writing
// child rows of two admins in descending order, and an SSO adoption then erasure of a further unverified admin. None
// may deadlock or time out.
func TestUpdateUserRole_NoDeadlockWithTheOtherAccountWrites(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	admins := []string{"adm-1", "adm-2", "adm-3"}
	seedRoleUsers(t, b, auth.RoleAdmin, admins...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stop := time.Now().Add(time.Second)
	var mu sync.Mutex
	var failures []string
	fail := func(what string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if len(failures) < 10 {
			failures = append(failures, fmt.Sprintf("%s: %v", what, err))
		}
	}
	loop := func(name string, fn func(i int) error) func() {
		return func() {
			for i := 0; time.Now().Before(stop); i++ {
				if err := fn(i); err != nil {
					fail(name, err)
					return
				}
			}
		}
	}
	allowed := func(err error, ok ...error) error {
		for _, e := range ok {
			if errors.Is(err, e) {
				return nil
			}
		}
		return err
	}
	var wg sync.WaitGroup
	for w := range 2 {
		wg.Go(loop(fmt.Sprintf("role change %d", w), func(i int) error {
			role := auth.RoleMember
			if (i+w)%2 == 0 {
				role = auth.RoleAdmin
			}
			return allowed(b.UpdateUserRole(ctx, admins[(i+w)%3], role), interfaces.ErrLastAdmin)
		}))
	}
	wg.Go(loop("wrong password", func(i int) error {
		_, _, err := b.RecordFailedLogin(ctx, admins[i%3], 1000, time.Minute)
		return allowed(err, interfaces.ErrAccountLocked)
	}))
	wg.Go(loop("right password", func(i int) error {
		return allowed(b.ResetFailedLogins(ctx, admins[(i+1)%3]), interfaces.ErrAccountLocked)
	}))
	wg.Go(loop("refresh", func(i int) error {
		return b.StoreRefreshToken(ctx, fmt.Sprintf("jti-%d", i), admins[(i+2)%3], time.Now().Add(time.Hour), "", "")
	}))
	wg.Go(loop("request writing child rows of two admins", func(i int) error {
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, id := range []string{admins[2-i%2], admins[0]} {
			if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR KEY SHARE`, id); err != nil {
				return err
			}
			time.Sleep(time.Millisecond)
		}
		return tx.Commit()
	}))
	wg.Go(loop("SSO adoption and erasure", func(i int) error {
		id := fmt.Sprintf("adm-sso-%d", i)
		u := &interfaces.User{ID: id, Email: id + "@example.com", Password: "h", Role: auth.RoleAdmin}
		if err := b.CreateUser(ctx, u); err != nil {
			return err
		}
		u.EmailVerified, u.Password = true, ""
		if err := b.SaveUserWithIdentityLink(ctx, u, &interfaces.IdentityLink{Provider: "oidc", Subject: id, UserID: id}); err != nil {
			return err
		}
		return b.DeleteUser(ctx, id)
	}))
	wg.Wait()
	for _, f := range failures {
		t.Error(f)
	}
}

// TestRedeemPasswordReset_NoDeadlockWithDeleteUser (ACC-R6-1): DeleteUser's final DELETE FROM users locks the users
// row and then cascades to user_tokens. RedeemPasswordReset locks the users row (FOR NO KEY UPDATE) before locking
// user_tokens in its own transaction, so when an erasure is parked mid-cascade on another token of the same user, a
// concurrent RedeemPasswordReset queues on the users row instead of taking the reset token first and deadlocking
// (40P01) with the cascade.
func TestRedeemPasswordReset_NoDeadlockWithDeleteUser(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	seedRoleUsers(t, b, auth.RoleMember, "u-acc")
	exp := time.Now().Add(time.Hour).UTC()
	for _, hash := range []string{"tok-park", "tok-redeem"} {
		if err := b.CreateUserToken(ctx, &interfaces.UserToken{
			TokenHash: hash,
			Purpose:   interfaces.TokenPurposePasswordReset,
			UserID:    "u-acc",
			ExpiresAt: exp,
		}); err != nil {
			t.Fatalf("CreateUserToken %s: %v", hash, err)
		}
	}

	// Third session parks the ON DELETE CASCADE of DeleteUser on tok-park after DeleteUser locks users.
	parker, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parker.Rollback() }()
	if _, err := parker.ExecContext(ctx, `SELECT 1 FROM user_tokens WHERE token_hash = 'tok-park' FOR UPDATE`); err != nil {
		t.Fatalf("lock tok-park: %v", err)
	}

	erased := make(chan error, 1)
	go func() {
		erased <- b.DeleteUser(ctx, "u-acc")
	}()
	waitForLockWaiters(t, b, 1)

	redeemed := make(chan error, 1)
	go func() {
		_, err := b.RedeemPasswordReset(ctx, "tok-redeem", "new-password-hash")
		redeemed <- err
	}()
	waitForLockWaiters(t, b, 2)

	if err := parker.Rollback(); err != nil {
		t.Fatalf("release parker: %v", err)
	}
	if err := <-erased; err != nil {
		t.Errorf("DeleteUser: %v, want nil", err)
	}
	if err := <-redeemed; !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("RedeemPasswordReset after DeleteUser: %v, want ErrNotFound", err)
	}
}
