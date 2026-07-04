package filesystem

import (
	"context"
	"errors"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

func TestUserTokens_ConsumeOnce(t *testing.T) {
	b, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	ctx := context.Background()
	tok := &interfaces.UserToken{
		TokenHash: "hash-1",
		Purpose:   interfaces.TokenPurposePasswordReset,
		UserID:    "user-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := b.CreateUserToken(ctx, tok); err != nil {
		t.Fatalf("CreateUserToken: %v", err)
	}

	uid, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposePasswordReset, "hash-1")
	if err != nil || uid != "user-1" {
		t.Fatalf("first consume: uid=%q err=%v", uid, err)
	}
	// Single-use: a second consume must fail.
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposePasswordReset, "hash-1"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("replay should be ErrNotFound, got %v", err)
	}
}

func TestUserTokens_PurposeAndExpiry(t *testing.T) {
	b, _ := NewLocalStorageBackend(t.TempDir())
	ctx := context.Background()

	_ = b.CreateUserToken(ctx, &interfaces.UserToken{
		TokenHash: "h-purpose", Purpose: interfaces.TokenPurposePasswordReset,
		UserID: "u", ExpiresAt: time.Now().Add(time.Hour),
	})
	// Wrong purpose must not match.
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposeEmailVerify, "h-purpose"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("purpose mismatch should be ErrNotFound, got %v", err)
	}

	// Expired token must not match.
	_ = b.CreateUserToken(ctx, &interfaces.UserToken{
		TokenHash: "h-exp", Purpose: interfaces.TokenPurposeEmailVerify,
		UserID: "u", ExpiresAt: time.Now().Add(-time.Minute),
	})
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposeEmailVerify, "h-exp"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("expired token should be ErrNotFound, got %v", err)
	}
}

func TestSetUserEmailVerified(t *testing.T) {
	b, _ := NewLocalStorageBackend(t.TempDir())
	ctx := context.Background()
	u := &interfaces.User{ID: "u-1", Email: "u@x.com"}
	if err := b.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := b.SetUserEmailVerified(ctx, "u-1"); err != nil {
		t.Fatalf("SetUserEmailVerified: %v", err)
	}
	got, err := b.LoadUserByID(ctx, "u-1")
	if err != nil {
		t.Fatalf("LoadUserByID: %v", err)
	}
	if !got.EmailVerified {
		t.Error("expected EmailVerified=true after SetUserEmailVerified")
	}
}

// TestInvalidateUserTokens_RevokesOutstandingTokensForUser is the regression
// test for the account-takeover fix: after a password change/reset, every other
// outstanding reset/verify token for that user must become unredeemable, while
// tokens for other users and other purposes are untouched.
func TestInvalidateUserTokens_RevokesOutstandingTokensForUser(t *testing.T) {
	b, _ := NewLocalStorageBackend(t.TempDir())
	ctx := context.Background()
	future := time.Now().Add(time.Hour)

	tokens := []struct {
		hash    string
		purpose string
		user    string
	}{
		{"reset-A", interfaces.TokenPurposePasswordReset, "alice"}, // consumed by reset
		{"reset-B", interfaces.TokenPurposePasswordReset, "alice"}, // must be invalidated
		{"verify-A", interfaces.TokenPurposeEmailVerify, "alice"},  // must be invalidated
		{"reset-C", interfaces.TokenPurposePasswordReset, "bob"},   // other user: untouched
	}
	for _, tk := range tokens {
		if err := b.CreateUserToken(ctx, &interfaces.UserToken{
			TokenHash: tk.hash, Purpose: tk.purpose, UserID: tk.user, ExpiresAt: future,
		}); err != nil {
			t.Fatalf("CreateUserToken %s: %v", tk.hash, err)
		}
	}

	// Alice resets her password using reset-A.
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposePasswordReset, "reset-A"); err != nil {
		t.Fatalf("consume reset-A: %v", err)
	}
	// Invalidate all other alice reset/verify tokens (what the handler does next).
	if err := b.InvalidateUserTokens(ctx, "alice",
		interfaces.TokenPurposePasswordReset, interfaces.TokenPurposeEmailVerify); err != nil {
		t.Fatalf("InvalidateUserTokens: %v", err)
	}

	// reset-B (alice) and verify-A (alice) must now be unredeemable.
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposePasswordReset, "reset-B"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("alice reset-B: expected ErrNotFound after invalidation, got %v", err)
	}
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposeEmailVerify, "verify-A"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("alice verify-A: expected ErrNotFound after invalidation, got %v", err)
	}
	// bob's token is in a different purpose scope for a different user — still valid.
	if _, err := b.ConsumeUserToken(ctx, interfaces.TokenPurposePasswordReset, "reset-C"); err != nil {
		t.Errorf("bob reset-C: expected success (other user), got %v", err)
	}

	// Empty purposes is a no-op (never panics, never deletes anything).
	if err := b.InvalidateUserTokens(ctx, "alice"); err != nil {
		t.Fatalf("InvalidateUserTokens with no purposes: %v", err)
	}
}
