package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/sso"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// resetRaceStore runs during once, inside a password reset after its bcrypt time: before the write that redeems the
// token (or, as the reset ran before, between consuming the token and writing the password).
type resetRaceStore struct {
	storageif.StorageBackend
	during func()
}

func (s *resetRaceStore) RedeemPasswordReset(ctx context.Context, tokenHash, passwordHash string) (string, error) {
	s.run()
	return s.StorageBackend.RedeemPasswordReset(ctx, tokenHash, passwordHash)
}

func (s *resetRaceStore) ConsumeUserToken(ctx context.Context, purpose, tokenHash string) (string, error) {
	id, err := s.StorageBackend.ConsumeUserToken(ctx, purpose, tokenHash)
	s.run()
	return id, err
}

func (s *resetRaceStore) run() {
	if s.during != nil {
		during := s.during
		s.during = nil
		during()
	}
}

// TestResetPassword_KeepsAConcurrentSSOAdoption: an SSO sign-in adopts the unverified account (it invalidates the
// outstanding reset links, then clears the password) while a password reset runs. The reset consumed its token, spent
// its bcrypt time, then wrote the password whatever had happened meanwhile: the account the adoption had moved onto
// the identity provider got a password again. The token's consumption and the password write are now one change, so
// the adoption's invalidation refuses the reset.
func TestResetPassword_KeepsAConcurrentSSOAdoption(t *testing.T) {
	forEachLoginBackend(t, func(t *testing.T, b storageif.StorageBackend) {
		ctx := context.Background()
		u := seedLoginUser(t, b, "reset-adopt", false)
		raw, hash, err := auth.GenerateOpaqueToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := b.CreateUserToken(ctx, &storageif.UserToken{TokenHash: hash, Purpose: storageif.TokenPurposePasswordReset,
			UserID: u.ID, ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
			t.Fatalf("CreateUserToken: %v", err)
		}
		store := &resetRaceStore{StorageBackend: b, during: func() {
			if _, err := NewIdentityService(b, b).ResolveSSOUser(ctx, "oidc",
				&sso.Identity{Subject: "reset-adopt-subject", Email: u.Email, EmailVerified: true}); err != nil {
				t.Errorf("the SSO adoption: %v", err)
			}
		}}
		if _, err := NewAuthService(store, nil).ResetPassword(ctx, raw, "N3w-passw0rd!"); !errors.Is(err, ErrInvalidResetToken) {
			t.Errorf("the reset the adoption overtook: %v, want ErrInvalidResetToken", err)
		}
		got, err := b.LoadUserByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("LoadUserByID: %v", err)
		}
		if got.Password != "" || !got.EmailVerified {
			t.Errorf("after the adoption and the reset: password set %v, verified %v; want the adoption's (none, verified)", got.Password != "", got.EmailVerified)
		}
	})
}

// redeemCountingStore counts the redeem attempts, each of which follows a bcrypt hash of the new password.
type redeemCountingStore struct {
	storageif.StorageBackend
	redeems int
}

func (s *redeemCountingStore) RedeemPasswordReset(ctx context.Context, tokenHash, passwordHash string) (string, error) {
	s.redeems++
	return s.StorageBackend.RedeemPasswordReset(ctx, tokenHash, passwordHash)
}

// TestResetPassword_RefusesABadTokenBeforeHashing: the reset hashes the new password before it redeems the token, so a
// junk, used or expired token cost an unauthenticated caller a bcrypt hash; a read-only check refuses it first.
func TestResetPassword_RefusesABadTokenBeforeHashing(t *testing.T) {
	forEachLoginBackend(t, func(t *testing.T, b storageif.StorageBackend) {
		ctx := context.Background()
		u := seedLoginUser(t, b, "reset-cheap", true)
		token := func(ttl time.Duration) string {
			t.Helper()
			raw, hash, err := auth.GenerateOpaqueToken()
			if err != nil {
				t.Fatal(err)
			}
			if err := b.CreateUserToken(ctx, &storageif.UserToken{TokenHash: hash, Purpose: storageif.TokenPurposePasswordReset,
				UserID: u.ID, ExpiresAt: time.Now().UTC().Add(ttl)}); err != nil {
				t.Fatalf("CreateUserToken: %v", err)
			}
			return raw
		}
		store := &redeemCountingStore{StorageBackend: b}
		svc := NewAuthService(store, nil)
		valid := token(time.Hour)
		if _, err := svc.ResetPassword(ctx, valid, "N3w-passw0rd!"); err != nil {
			t.Fatalf("the reset with a valid token: %v", err)
		}
		store.redeems = 0
		for what, raw := range map[string]string{"a junk token": "junk", "a used token": valid, "an expired token": token(-time.Minute)} {
			if _, err := svc.ResetPassword(ctx, raw, "An0ther-passw0rd!"); !errors.Is(err, ErrInvalidResetToken) {
				t.Errorf("the reset with %s: %v, want ErrInvalidResetToken", what, err)
			}
		}
		if store.redeems != 0 {
			t.Errorf("%d of 3 bad tokens were hashed for and redeemed; want each refused before the hash", store.redeems)
		}
	})
}
