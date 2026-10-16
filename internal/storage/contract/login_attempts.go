package contract

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// runLoginAttemptScenarios holds the failed-login counter's targeted writes and the avatar URL's: each changes
// only its own columns in one write, so parallel attempts each count, exactly one of them locks the account, and a
// reset, role change or rename made in between is never undone.
func runLoginAttemptScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	// lockSlack absorbs the skew between the test's clock and the database's (Postgres stamps the lock with NOW()).
	const lockSlack = time.Minute
	seed := func(t *testing.T, name string) *interfaces.User {
		t.Helper()
		u := &interfaces.User{ID: "contract-login-" + name + "-" + runID, Email: "contract-login-" + name + "-" + runID + "@example.com",
			Password: "hash-1", Role: auth.RoleViewer, DisplayName: "Kim", AvatarURL: "https://example.com/kim.png"}
		if err := b.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		t.Cleanup(func() { _ = b.DeleteUser(context.Background(), u.ID) })
		return u
	}
	load := func(t *testing.T, id string) *interfaces.User {
		t.Helper()
		u, err := b.LoadUserByID(ctx, id)
		if err != nil {
			t.Fatalf("LoadUserByID: %v", err)
		}
		return u
	}
	untouched := func(t *testing.T, got *interfaces.User) {
		t.Helper()
		if got.Password != "hash-1" || got.Role != auth.RoleViewer || got.DisplayName != "Kim" ||
			got.AvatarURL != "https://example.com/kim.png" || got.EmailVerified {
			t.Errorf("a counter write changed another column: %+v", got)
		}
	}

	run("RecordFailedLogin_counts_and_locks_at_the_threshold", func(t *testing.T) {
		u := seed(t, "count")
		for want := 1; want <= 2; want++ {
			attempts, until, err := b.RecordFailedLogin(ctx, u.ID, 3, time.Hour)
			if err != nil || attempts != want || until != nil {
				t.Fatalf("attempt %d: (%d, %v, %v), want (%d, no lock, nil)", want, attempts, until, err, want)
			}
		}
		before := time.Now()
		attempts, until, err := b.RecordFailedLogin(ctx, u.ID, 3, time.Hour)
		if err != nil || attempts != 3 || until == nil {
			t.Fatalf("the attempt reaching the threshold: (%d, %v, %v), want (3, a lock, nil)", attempts, until, err)
		}
		if until.Before(before.Add(time.Hour-lockSlack)) || until.After(time.Now().Add(time.Hour+lockSlack)) {
			t.Errorf("locked until %v, want about an hour from %v", until, before)
		}
		// While locked an attempt is refused and not counted.
		if _, _, err := b.RecordFailedLogin(ctx, u.ID, 3, time.Hour); !errors.Is(err, interfaces.ErrAccountLocked) {
			t.Errorf("an attempt on the locked account: %v, want ErrAccountLocked", err)
		}
		got := load(t, u.ID)
		if got.FailedLoginAttempts != 3 || got.LockedUntil == nil || !got.LockedUntil.Equal(*until) {
			t.Errorf("stored: %d attempts, locked until %v; want 3 and %v", got.FailedLoginAttempts, got.LockedUntil, until)
		}
		untouched(t, got)

		if _, _, err := b.RecordFailedLogin(ctx, "contract-login-missing-"+runID, 3, time.Hour); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("an unknown user: %v, want ErrNotFound", err)
		}
	})

	run("RecordFailedLogin_relocks_once_the_lock_has_expired", func(t *testing.T) {
		u := seed(t, "expired")
		expired := time.Now().Add(-time.Minute).UTC()
		u.FailedLoginAttempts, u.LockedUntil = 3, &expired
		if err := b.SaveUser(ctx, u); err != nil {
			t.Fatalf("SaveUser: %v", err)
		}
		attempts, until, err := b.RecordFailedLogin(ctx, u.ID, 3, time.Hour)
		if err != nil || attempts != 4 || until == nil || !until.After(time.Now().Add(time.Hour-lockSlack)) {
			t.Fatalf("an attempt after the lock expired: (%d, %v, %v), want (4, a new lock, nil)", attempts, until, err)
		}
	})

	run("RecordFailedLogin_parallel_attempts_lock_exactly_once", func(t *testing.T) {
		u := seed(t, "parallel")
		const n, threshold = 12, 5
		type result struct {
			attempts int
			err      error
		}
		results := make([]result, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				attempts, _, err := b.RecordFailedLogin(ctx, u.ID, threshold, time.Hour)
				results[i] = result{attempts, err}
			}()
		}
		close(start)
		wg.Wait()

		counted, locked, crossed := map[int]bool{}, 0, 0
		for _, r := range results {
			switch {
			case errors.Is(r.err, interfaces.ErrAccountLocked):
				locked++
			case r.err != nil:
				t.Fatalf("RecordFailedLogin: %v", r.err)
			default:
				if counted[r.attempts] {
					t.Errorf("two attempts both counted as attempt %d", r.attempts)
				}
				counted[r.attempts] = true
				if r.attempts >= threshold {
					crossed++
				}
			}
		}
		if len(counted) != threshold || locked != n-threshold || crossed != 1 {
			t.Errorf("%d parallel attempts: %d counted, %d refused as locked, %d crossed the threshold; want %d, %d, 1",
				n, len(counted), locked, crossed, threshold, n-threshold)
		}
		if got := load(t, u.ID); got.FailedLoginAttempts != threshold || got.LockedUntil == nil {
			t.Errorf("stored: %d attempts, locked until %v; want %d and a lock", got.FailedLoginAttempts, got.LockedUntil, threshold)
		}
	})

	run("ResetFailedLogins_clears_only_the_counter", func(t *testing.T) {
		u := seed(t, "reset")
		until := time.Now().Add(-time.Hour).UTC() // a lock that has run out: the reset clears it
		u.FailedLoginAttempts, u.LockedUntil = 4, &until
		if err := b.SaveUser(ctx, u); err != nil {
			t.Fatalf("SaveUser: %v", err)
		}
		if err := b.ResetFailedLogins(ctx, u.ID); err != nil {
			t.Fatalf("ResetFailedLogins: %v", err)
		}
		got := load(t, u.ID)
		if got.FailedLoginAttempts != 0 || got.LockedUntil != nil {
			t.Errorf("after the reset: %d attempts, locked until %v; want 0 and no lock", got.FailedLoginAttempts, got.LockedUntil)
		}
		untouched(t, got)

		// On a clear row it writes nothing, so updated_at stays.
		if err := b.ResetFailedLogins(ctx, u.ID); err != nil {
			t.Fatalf("ResetFailedLogins (already clear): %v", err)
		}
		if again := load(t, u.ID); !again.UpdatedAt.Equal(got.UpdatedAt) {
			t.Errorf("a reset of a clear row moved updated_at from %v to %v", got.UpdatedAt, again.UpdatedAt)
		}
		if err := b.ResetFailedLogins(ctx, "contract-login-missing-"+runID); err != nil {
			t.Errorf("ResetFailedLogins(unknown): %v, want nil", err)
		}
	})

	// The right password's write: it checks the lock against the row as it is now, so a login that read the account
	// unlocked before a parallel burst locked it is refused, and the lock stands.
	run("ResetFailedLogins_refuses_a_locked_account", func(t *testing.T) {
		u := seed(t, "reset-locked")
		for range 3 {
			if _, _, err := b.RecordFailedLogin(ctx, u.ID, 3, time.Hour); err != nil {
				t.Fatalf("RecordFailedLogin: %v", err)
			}
		}
		locked := load(t, u.ID)
		if err := b.ResetFailedLogins(ctx, u.ID); !errors.Is(err, interfaces.ErrAccountLocked) {
			t.Errorf("ResetFailedLogins on a locked account: %v, want ErrAccountLocked", err)
		}
		got := load(t, u.ID)
		if got.FailedLoginAttempts != 3 || got.LockedUntil == nil || !got.UpdatedAt.Equal(locked.UpdatedAt) {
			t.Errorf("after the refused reset: %d attempts, locked until %v, updated %v (was %v); want the lock untouched",
				got.FailedLoginAttempts, got.LockedUntil, got.UpdatedAt, locked.UpdatedAt)
		}
		untouched(t, got)
	})

	run("UpdateUserAvatarURL_writes_only_the_avatar_url", func(t *testing.T) {
		u := seed(t, "avatar")
		if err := b.UpdateUserAvatarURL(ctx, u.ID, "/api/users/x/avatar"); err != nil {
			t.Fatalf("UpdateUserAvatarURL: %v", err)
		}
		if got := load(t, u.ID); got.AvatarURL != "/api/users/x/avatar" || got.DisplayName != "Kim" {
			t.Errorf("after the update: avatar %q, name %q; want the new URL and the name kept", got.AvatarURL, got.DisplayName)
		}
		if err := b.UpdateUserAvatarURL(ctx, u.ID, ""); err != nil {
			t.Fatalf("UpdateUserAvatarURL(clear): %v", err)
		}
		if got := load(t, u.ID); got.AvatarURL != "" || got.DisplayName != "Kim" {
			t.Errorf("after clearing: avatar %q, name %q", got.AvatarURL, got.DisplayName)
		}
		if err := b.UpdateUserAvatarURL(ctx, "contract-login-missing-"+runID, "/x"); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("UpdateUserAvatarURL(unknown): %v, want ErrNotFound", err)
		}
	})

	// A password reset's token and its password are one change: the token redeems once, and an invalidated, expired
	// or verification token writes nothing. UserTokenValid gives the same answer and consumes nothing.
	run("RedeemPasswordReset_consumes_the_token_with_the_password", func(t *testing.T) {
		u := seed(t, "reset-redeem")
		token := func(name, purpose string, ttl time.Duration) string {
			t.Helper()
			hash := "contract-reset-" + name + "-" + runID
			if err := b.CreateUserToken(ctx, &interfaces.UserToken{TokenHash: hash, Purpose: purpose, UserID: u.ID,
				ExpiresAt: time.Now().UTC().Add(ttl)}); err != nil {
				t.Fatalf("CreateUserToken: %v", err)
			}
			return hash
		}
		invalidated := token("invalidated", interfaces.TokenPurposePasswordReset, time.Hour)
		if err := b.InvalidateUserTokens(ctx, u.ID, interfaces.TokenPurposePasswordReset); err != nil {
			t.Fatalf("InvalidateUserTokens: %v", err)
		}
		for _, tc := range []struct{ what, hash string }{
			{"an invalidated token", invalidated},
			{"an expired token", token("expired", interfaces.TokenPurposePasswordReset, -time.Minute)},
			{"a verification token", token("verify", interfaces.TokenPurposeEmailVerify, time.Hour)},
			{"an unknown token", "contract-reset-unknown-" + runID},
		} {
			if ok, err := b.UserTokenValid(ctx, interfaces.TokenPurposePasswordReset, tc.hash); ok || err != nil {
				t.Errorf("UserTokenValid with %s: (%v, %v), want (false, nil)", tc.what, ok, err)
			}
			if _, err := b.RedeemPasswordReset(ctx, tc.hash, "hash-2"); !errors.Is(err, interfaces.ErrNotFound) {
				t.Errorf("RedeemPasswordReset with %s: %v, want ErrNotFound", tc.what, err)
			}
		}
		untouched(t, load(t, u.ID))

		valid := token("valid", interfaces.TokenPurposePasswordReset, time.Hour)
		if _, _, err := b.RecordFailedLogin(ctx, u.ID, 1, time.Hour); err != nil {
			t.Fatalf("RecordFailedLogin: %v", err)
		}
		for range 2 {
			if ok, err := b.UserTokenValid(ctx, interfaces.TokenPurposePasswordReset, valid); !ok || err != nil {
				t.Fatalf("UserTokenValid with the valid token: (%v, %v), want (true, nil)", ok, err)
			}
		}
		if ok, err := b.UserTokenValid(ctx, interfaces.TokenPurposeEmailVerify, valid); ok || err != nil {
			t.Errorf("UserTokenValid of the reset token as a verification: (%v, %v), want (false, nil)", ok, err)
		}
		if id, err := b.RedeemPasswordReset(ctx, valid, "hash-2"); err != nil || id != u.ID {
			t.Fatalf("RedeemPasswordReset: (%q, %v), want (%q, nil)", id, err, u.ID)
		}
		got := load(t, u.ID)
		if got.Password != "hash-2" || got.FailedLoginAttempts != 0 || got.LockedUntil != nil || got.Role != auth.RoleViewer || got.DisplayName != "Kim" {
			t.Errorf("after the reset: %+v; want the new password, the lockout lifted and the rest kept", got)
		}
		if ok, err := b.UserTokenValid(ctx, interfaces.TokenPurposePasswordReset, valid); ok || err != nil {
			t.Errorf("UserTokenValid after the redeem: (%v, %v), want (false, nil)", ok, err)
		}
		if _, err := b.RedeemPasswordReset(ctx, valid, "hash-3"); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("the token redeemed again: %v, want ErrNotFound", err)
		}
		if got := load(t, u.ID); got.Password != "hash-2" {
			t.Errorf("the replay wrote the password %q", got.Password)
		}
	})
}
